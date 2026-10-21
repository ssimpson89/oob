package mcpserver

import (
	"context"
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
}

type listOut struct {
	Hosts []hostInfo `json:"hosts"`
}

type hostIn struct {
	Host string `json:"host" jsonschema:"a host name from list_hosts"`
}

type statusIn struct {
	Selector string `json:"selector,omitempty" jsonschema:"a glob, a substring, or tag:<tag>; omit for every host"`
}

type statusRow struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	Status string `json:"status"`
	OK     bool   `json:"ok"`
}

type statusOut struct {
	Hosts []statusRow `json:"hosts"`
}

func (s *server) addHostTools(srv *mcp.Server) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &yes}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_hosts",
		Description: "List the hosts in oob's host list: name, BMC address, type, tags and notes. Never returns " +
			"passwords or keys. Nothing is sent to any BMC.",
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
			"a substring or tag:<tag>; omit it for every host. A host that fails shows the error in its row.",
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

func (s *server) listHosts(_ context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
	cfg, err := s.be.Config()
	if err != nil {
		return text(true, err.Error()), listOut{}, nil
	}
	out := listOut{Hosts: []hostInfo{}}
	var b strings.Builder
	for _, n := range cfg.Select(in.Filter) {
		h := cfg.Hosts[n]
		hi := hostInfo{
			Name: console.CleanRemoteLine(n), IP: console.CleanRemoteLine(h.IP), Type: console.CleanRemoteLine(h.Type),
			Tags: []string{}, Notes: console.CleanRemoteLine(h.Notes),
		}
		for _, t := range h.Tags {
			hi.Tags = append(hi.Tags, console.CleanRemoteLine(t))
		}
		out.Hosts = append(out.Hosts, hi)
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\n", hi.Name, hi.IP, hi.Type, strings.Join(hi.Tags, ","), hi.Notes)
	}
	if len(out.Hosts) == 0 {
		return text(false, "no hosts match"), out, nil
	}
	return text(false, "name\tip\ttype\ttags\tnotes\n"+strings.TrimSuffix(b.String(), "\n")), out, nil
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
		return text(true, err.Error()), statusOut{}, nil
	}
	names := cfg.Select(in.Selector)
	if len(names) == 0 {
		return text(true, "no hosts match"), statusOut{}, nil
	}
	var note string
	if len(names) > statusMaxRows {
		note = fmt.Sprintf("%d hosts match; showing the first %d. Narrow the selector.", len(names), statusMaxRows)
		names = names[:statusMaxRows]
	}
	ctx, cancel := context.WithTimeout(ctx, statusOverall)
	defer cancel()

	rows := make([]statusRow, len(names))
	for i, n := range names {
		rows[i] = statusRow{Name: n, IP: cfg.Hosts[n].IP, Status: "ERROR: not read (timed out)"}
	}
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
	out := statusOut{Hosts: append([]statusRow(nil), rows...)}
	mu.Unlock()
	var b strings.Builder
	for _, r := range out.Hosts {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", console.CleanRemoteLine(r.Name), console.CleanRemoteLine(r.IP), r.Status)
	}
	res := text(false, strings.TrimSuffix(b.String(), "\n"), note)
	return res, out, nil
}
