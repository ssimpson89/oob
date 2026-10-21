package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
)

// hostInfo is what list_hosts says about a host. It has no field for a
// password, a key or a key path, so none can leak.
type hostInfo struct {
	Name  string   `json:"name"`
	IP    string   `json:"ip"`
	Type  string   `json:"type" jsonschema:"saved BMC type: ilo, idrac, supermicro, ipmi, or empty when not detected yet"`
	Tags  []string `json:"tags"`
	Notes string   `json:"notes"`
}

type listIn struct {
	Filter string `json:"filter,omitempty" jsonschema:"a glob such as node*, a substring, or tag:<tag>. Case-insensitive. Omit for every host"`
	Limit  int    `json:"limit,omitempty" jsonschema:"most hosts to return, default 100, at most 500. A long list is cut sooner to keep the result small"`
	Offset int    `json:"offset,omitempty" jsonschema:"hosts to skip, for the next page: use next_offset from the previous result"`
}

// pageInfo says which part of a long result this is.
type pageInfo struct {
	Total      int  `json:"total" jsonschema:"hosts that match"`
	Offset     int  `json:"offset" jsonschema:"hosts skipped"`
	Truncated  bool `json:"truncated" jsonschema:"true when more hosts match than this page holds"`
	NextOffset int  `json:"next_offset,omitempty" jsonschema:"the offset for the next page, when truncated"`
}

type listOut struct {
	Hosts []hostInfo `json:"hosts"`
	pageInfo
}

type hostIn struct {
	Host string `json:"host" jsonschema:"a host name from list_hosts"`
}

type statusIn struct {
	Selector string `json:"selector,omitempty" jsonschema:"a glob, a substring, or tag:<tag>; omit for every host"`
	Limit    int    `json:"limit,omitempty" jsonschema:"most hosts to read, default 100, at most 500. A long list is cut sooner to keep the result small"`
	Offset   int    `json:"offset,omitempty" jsonschema:"hosts to skip, for the next page: use next_offset from the previous result"`
}

type statusRow struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	Status string `json:"status"`
	OK     bool   `json:"ok"`
}

type statusOut struct {
	Hosts []statusRow `json:"hosts"`
	pageInfo
}

func (s *server) addHostTools(srv *mcp.Server) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &yes}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_hosts",
		Description: "List the hosts in oob's host list: name, BMC address, type, tags and notes. Never returns " +
			"passwords or keys. Nothing is sent to any BMC. Long lists come in pages: use limit and offset, and next_offset when truncated.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &no},
	}, s.listHosts)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "power_status",
		Description: "Read one host's chassis power state from its BMC.",
		Annotations: ro,
	}, s.powerStatus)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "status",
		Description: "Power state of many hosts at once, read quietly and concurrently. The selector is a glob, " +
			"a substring or tag:<tag>; omit it for every host. A host that fails shows the error in its row. Long lists come in pages: use limit and offset, and next_offset when truncated.",
		Annotations: ro,
	}, s.bulkStatus)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sel_list",
		Description: "List a host's system event log (SEL). Long logs show the newest entries.",
		Annotations: ro,
	}, s.selList)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sensors",
		Description: "Sensor readings (temperatures, fans, voltages) over IPMI.",
		Annotations: ro,
	}, s.sensors)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "bmc_info",
		Description: "BMC identity and firmware (ipmitool mc info).",
		Annotations: ro,
	}, s.bmcInfo)
}

// hostFor loads the config and finds a host. exact turns off prefix matching.
func (s *server) hostFor(name string, exact bool) (string, config.Host, error) {
	cfg, err := s.be.Config()
	if err != nil {
		return "", config.Host{}, err
	}
	if exact {
		h, err := cfg.LookupExact(name)
		if err != nil {
			return "", config.Host{}, errors.New(strings.TrimSuffix(err.Error(), " (see: "+app.Name+" ls)") + "; use list_hosts for the exact names")
		}
		return name, h, nil
	}
	n, h, err := cfg.Lookup(name)
	if err != nil {
		return "", config.Host{}, errors.New(strings.TrimSuffix(err.Error(), " (see: "+app.Name+" ls)") + "; use list_hosts for the names")
	}
	return n, h, nil
}

const (
	pageDefault = 100
	pageMax     = statusMaxRows
	// pageBudget bounds one page of rows in bytes, counting the text and the
	// structured form together, so both stay under resultMax.
	pageBudget = 56 << 10
	// tagsMax bounds the tags shown for one host.
	tagsMax = 32
)

// pageArgs checks limit and offset and applies the default limit.
func pageArgs(limit, offset int) (int, int, error) {
	switch {
	case limit < 0 || offset < 0:
		return 0, 0, errors.New("limit and offset can't be negative")
	case limit > pageMax:
		return 0, 0, fmt.Errorf("limit can be at most %d", pageMax)
	case limit == 0:
		limit = pageDefault
	}
	return limit, offset, nil
}

// size is what a row costs in a result: its text plus its structured form.
func size(text string, row any) int {
	b, _ := json.Marshal(row)
	return len(text) + len(b)
}

// finish fills in the paging fields once a page of end-offset rows was taken
// from total, and returns the notice for the text, or "" when everything was
// shown.
func (p *pageInfo) finish(what string, total, offset, end int) string {
	p.Total, p.Offset = total, offset
	if end >= total {
		return ""
	}
	p.Truncated, p.NextOffset = true, end
	return fmt.Sprintf("Showing %s %d-%d of %d; the rest is left out. Call again with offset=%d for the next page, or narrow the filter.",
		what, offset+1, end, total, end)
}

func (s *server) listHosts(_ context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
	cfg, err := s.be.Config()
	if err != nil {
		return text(true, err.Error()), listOut{Hosts: []hostInfo{}}, nil
	}
	limit, offset, err := pageArgs(in.Limit, in.Offset)
	if err != nil {
		return text(true, err.Error()), listOut{Hosts: []hostInfo{}}, nil
	}
	names := cfg.Select(in.Filter)
	out := listOut{Hosts: []hostInfo{}}
	if len(names) == 0 {
		out.finish("hosts", 0, 0, 0)
		return text(false, "no hosts match"), out, nil
	}
	if offset >= len(names) {
		out.finish("hosts", len(names), len(names), len(names))
		return text(true, fmt.Sprintf("offset %d is past the last of %d matching hosts", offset, len(names))), out, nil
	}
	var b strings.Builder
	used, end := 0, offset
	for ; end < len(names) && end-offset < limit; end++ {
		h := cfg.Hosts[names[end]]
		hi := hostInfo{
			Name: console.CleanRemoteLine(names[end]), IP: console.CleanRemoteLine(h.IP), Type: console.CleanRemoteLine(h.Type),
			Tags: []string{}, Notes: console.CleanRemoteLine(h.Notes),
		}
		for _, t := range h.Tags[:min(len(h.Tags), tagsMax)] {
			hi.Tags = append(hi.Tags, console.CleanRemoteLine(t))
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\n", hi.Name, hi.IP, hi.Type, strings.Join(hi.Tags, ","), hi.Notes)
		cost := size(line, hi)
		if end > offset && used+cost > pageBudget {
			break
		}
		used += cost
		out.Hosts = append(out.Hosts, hi)
		b.WriteString(line)
	}
	note := out.finish("hosts", len(names), offset, end)
	return text(false, "name\tip\ttype\ttags\tnotes\n"+strings.TrimSuffix(b.String(), "\n"), note), out, nil
}

func (s *server) powerStatus(ctx context.Context, _ *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	name, h, err := s.hostFor(in.Host, false)
	if err != nil {
		return fail(err)
	}
	out, err := s.runBMC(ctx, readTimeout, func() error { return s.be.Driver(name, h).Power("status") })
	return bmcResult(name, "power status", out, err, readTimeout, false, false)
}

func (s *server) selList(ctx context.Context, _ *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	name, h, err := s.hostFor(in.Host, false)
	if err != nil {
		return fail(err)
	}
	out, err := s.runBMC(ctx, readTimeout, func() error { return s.be.Driver(name, h).SEL("list") })
	return bmcResult(name, "sel list", out, err, readTimeout, true, false)
}

func (s *server) sensors(ctx context.Context, _ *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	return s.ipmiRead(ctx, in.Host, "sensors", "sensor")
}

func (s *server) bmcInfo(ctx context.Context, _ *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	return s.ipmiRead(ctx, in.Host, "bmc info", "mc", "info")
}

func (s *server) ipmiRead(ctx context.Context, host, what string, args ...string) (*mcp.CallToolResult, any, error) {
	name, h, err := s.hostFor(host, false)
	if err != nil {
		return fail(err)
	}
	out, err := s.runBMC(ctx, readTimeout, func() error {
		code, err := s.be.IPMI(h, args...)
		return app.Code(code, err)
	})
	return bmcResult(name, what, out, err, readTimeout, false, false)
}

// bmcResult turns a captured BMC call into a tool result. keepEnd keeps the
// newest part of a long output (a log), else the start.
func bmcResult(name, what, out string, err error, timeout time.Duration, keepEnd, mayHaveRun bool) (*mcp.CallToolResult, any, error) {
	out = strings.TrimSpace(bound(out, bmcTextMax, keepEnd))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || console.IsTimeout(err) && out == "" {
			err = errTimeout(what+" on "+name, timeout, mayHaveRun)
		}
		return fail(out, err)
	}
	if out == "" {
		out = "(no output)"
	}
	return text(false, out), nil, nil
}

func (s *server) bulkStatus(ctx context.Context, _ *mcp.CallToolRequest, in statusIn) (*mcp.CallToolResult, statusOut, error) {
	cfg, err := s.be.Config()
	if err != nil {
		return text(true, err.Error()), statusOut{Hosts: []statusRow{}}, nil
	}
	limit, offset, err := pageArgs(in.Limit, in.Offset)
	if err != nil {
		return text(true, err.Error()), statusOut{Hosts: []statusRow{}}, nil
	}
	all := cfg.Select(in.Selector)
	if len(all) == 0 {
		return text(true, "no hosts match"), statusOut{Hosts: []statusRow{}}, nil
	}
	if offset >= len(all) {
		return text(true, fmt.Sprintf("offset %d is past the last of %d matching hosts", offset, len(all))), statusOut{Hosts: []statusRow{}}, nil
	}
	// Take the page before reading anything. A row's status isn't known yet, so
	// reserve room for a typical one; rows that turn out larger are dropped
	// from the end afterwards.
	var names []string
	var rows []statusRow
	used := 0
	for _, n := range all[offset:min(len(all), offset+limit)] {
		r := statusRow{Name: console.CleanRemoteLine(n), IP: console.CleanRemoteLine(cfg.Hosts[n].IP), Status: "ERROR: not read (timed out)"}
		cost := size(r.Name+"\t"+r.IP+"\t"+r.Status+"\n", r)
		if len(rows) > 0 && used+cost > pageBudget {
			break
		}
		used += cost
		names = append(names, n)
		rows = append(rows, r)
	}
	ctx, cancel := context.WithTimeout(ctx, statusOverall)
	defer cancel()

	var mu sync.Mutex
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(statusWorkers, len(names)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				_, h, _ := cfg.Lookup(names[i])
				r := s.be.PowerStatus(names[i], h, statusTimeout)
				mu.Lock()
				rows[i].Status, rows[i].OK = console.CleanRemoteLine(r.Text), !r.Failed
				mu.Unlock()
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := range names {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	mu.Lock()
	page := append([]statusRow(nil), rows...)
	mu.Unlock()

	// Now the sizes are known: keep what fits.
	var b strings.Builder
	out := statusOut{Hosts: []statusRow{}}
	used = 0
	for _, r := range page {
		line := r.Name + "\t" + r.IP + "\t" + r.Status + "\n"
		cost := size(line, r)
		if len(out.Hosts) > 0 && used+cost > pageBudget {
			break
		}
		used += cost
		out.Hosts = append(out.Hosts, r)
		b.WriteString(line)
	}
	note := out.finish("hosts", len(all), offset, offset+len(out.Hosts))
	return text(false, strings.TrimSuffix(b.String(), "\n"), note), out, nil
}
