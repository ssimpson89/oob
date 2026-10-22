package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ssimpson89/oob/internal/bmc"
	"github.com/ssimpson89/oob/internal/config"
)

func toolNames(t *testing.T, h *harness) []string {
	t.Helper()
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func TestToolList(t *testing.T) {
	h := start(t, Options{}, "")
	want := []string{"bmc_info", "bmc_prompt", "boot", "connect", "disconnect", "list_hosts", "mc_reset", "nmi", "power",
		"read_output", "sel_clear", "sel_list", "send_break", "send_command", "send_keys",
		"sensors", "status", "uid"}
	if got := toolNames(t, h); !slices.Equal(got, want) {
		t.Errorf("tools\n got %v\nwant %v", got, want)
	}
	res, _ := h.cs.ListTools(context.Background(), nil)
	for _, tool := range res.Tools {
		if tool.Description == "" || tool.Annotations == nil {
			t.Errorf("%s has no description or annotations", tool.Name)
		}
		switch tool.Name {
		case "list_hosts", "status", "sel_list", "sensors", "bmc_info", "read_output":
			if !tool.Annotations.ReadOnlyHint {
				t.Errorf("%s should be read-only", tool.Name)
			}
		case "uid":
			if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || strings.Contains(tool.Description, "asks the user") {
				t.Errorf("uid is harmless and must not be marked destructive or described as asking: %q", tool.Description)
			}
		case "power", "boot", "nmi", "sel_clear", "mc_reset", "send_command", "send_keys", "send_break":
			if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Errorf("%s should be marked destructive", tool.Name)
			}
		}
	}
}

func TestReadOnlyServerHasOnlyReadOnlyTools(t *testing.T) {
	h := start(t, Options{ReadOnly: true}, "accept")
	want := []string{"bmc_info", "list_hosts", "sel_list", "sensors", "status"}
	if got := toolNames(t, h); !slices.Equal(got, want) {
		t.Errorf("tools %v, want %v", got, want)
	}
}

func TestListHostsNeverLeaksSecrets(t *testing.T) {
	h := start(t, Options{}, "")
	for _, filter := range []string{"", "node*", "tag:rack1", "login", "nothing"} {
		args := map[string]any{}
		if filter != "" {
			args["filter"] = filter
		}
		res := h.result("list_hosts", args)
		raw, _ := json.Marshal(res)
		for _, secret := range []string{secretPass, secretKey, "id_test", "s3cr3t", `"pass"`, `"key"`, "password"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("filter %q: result contains %q:\n%s", filter, secret, raw)
			}
		}
	}
	res := h.result("list_hosts", map[string]any{"filter": "tag:rack1"})
	var out listOut
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil || len(out.Hosts) != 1 {
		t.Fatalf("structured %s: %v", b, err)
	}
	got := out.Hosts[0]
	if got.Name != "node01" || got.IP != "192.0.2.11" || got.Type != "ipmi" || !slices.Equal(got.Tags, []string{"rack1", "gpu"}) || got.Notes != "first node" {
		t.Errorf("host = %+v", got)
	}
	var fields map[string]any
	var raw struct{ Hosts []map[string]any }
	json.Unmarshal(b, &raw)
	fields = raw.Hosts[0]
	for k := range fields {
		if !slices.Contains([]string{"name", "ip", "type", "tags", "notes"}, k) {
			t.Errorf("unexpected field %q", k)
		}
	}
	out2, _ := h.call("list_hosts", map[string]any{"filter": "nothing"})
	if out2 != "no hosts match" {
		t.Errorf("no match said %q", out2)
	}
}

func TestReadOnlyTools(t *testing.T) {
	h := start(t, Options{}, "") // the client can't ask: read-only tools must not need to
	out, isErr := h.call("status", map[string]any{"filter": "node01"})
	if isErr || !strings.Contains(out, "Chassis Power is on") {
		t.Errorf("status of one host: %q %v", out, isErr)
	}
	out, isErr = h.call("sel_list", map[string]any{"host": "node02"})
	if isErr || !strings.Contains(out, "Presence detected") {
		t.Errorf("sel_list: %q %v", out, isErr)
	}
	if strings.ContainsAny(out, "\x1b\x07") || strings.Contains(out, "evil") && strings.Contains(out, "]0;") {
		t.Errorf("sel_list text isn't clean: %q", out)
	}
	out, isErr = h.call("sensors", map[string]any{"host": "node01"})
	if isErr || !strings.Contains(out, "CPU Temp") {
		t.Errorf("sensors: %q %v", out, isErr)
	}
	out, isErr = h.call("bmc_info", map[string]any{"host": "login1"})
	if isErr || !strings.Contains(out, "CPU Temp") {
		t.Errorf("bmc_info: %q %v", out, isErr)
	}
	if got := strings.Join(h.ipmi, "|"); got != "192.0.2.11 sensor|192.0.2.13 mc info" {
		t.Errorf("ipmi calls %q", got)
	}
	// Credentials were resolved inside oob and handed to the driver, not to the model.
	if c := h.called(); !strings.Contains(c, "driver:node01 pass=true") || !strings.Contains(c, "node01:power status") {
		t.Errorf("calls %q", c)
	}
	// Other tools take a unique prefix; an ambiguous one is an error.
	if out, isErr := h.call("sel_list", map[string]any{"host": "login"}); isErr || !strings.Contains(out, "Presence") {
		t.Errorf("prefix: %q %v", out, isErr)
	}
	if out, isErr := h.call("sel_list", map[string]any{"host": "node"}); !isErr || !strings.Contains(out, "ambiguous") {
		t.Errorf("ambiguous prefix: %q %v", out, isErr)
	}
	if out, isErr := h.call("sel_list", map[string]any{"host": "nope"}); !isErr || !strings.Contains(out, "list_hosts") {
		t.Errorf("unknown host: %q %v", out, isErr)
	}
	// status with a part of a name is the bulk form: one row per match, quiet.
	if out, isErr := h.call("status", map[string]any{"filter": "login"}); isErr || !strings.Contains(out, "login1\t") {
		t.Errorf("status of a substring: %q %v", out, isErr)
	}
	for _, s := range []string{secretPass, secretKey} {
		if all := h.called(); strings.Contains(all, s) {
			t.Errorf("secret in %q", all)
		}
	}
}

func TestBulkStatus(t *testing.T) {
	h := start(t, Options{}, "")
	h.statuses["node02"] = bmc.StatusResult{Text: "ERROR: connection refused \x1b[31m", Failed: true}
	res := h.result("status", map[string]any{"filter": "node*"})
	if res.IsError {
		t.Fatalf("status failed: %+v", res)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out statusOut
	if err := json.Unmarshal(b, &out); err != nil || len(out.Hosts) != 2 {
		t.Fatalf("structured %s: %v", b, err)
	}
	if out.Hosts[0].Name != "node01" || !out.Hosts[0].OK || out.Hosts[1].Name != "node02" || out.Hosts[1].OK {
		t.Errorf("rows %+v", out.Hosts)
	}
	if strings.Contains(out.Hosts[1].Status, "\x1b") {
		t.Errorf("status not clean: %q", out.Hosts[1].Status)
	}
	// No match is the same for both tools: a plain "no hosts match", not an error.
	for _, tool := range []string{"list_hosts", "status"} {
		if text, isErr := h.call(tool, map[string]any{"filter": "zzz"}); isErr || text != "no hosts match" {
			t.Errorf("%s with no match: %q %v", tool, text, isErr)
		}
	}
	if text, isErr := h.call("status", nil); isErr || !strings.Contains(text, "login1") {
		t.Errorf("all hosts: %q %v", text, isErr)
	}
}

func TestBMCOutputIsBounded(t *testing.T) {
	h := start(t, Options{}, "")
	h.selLines = 5000
	out, isErr := h.call("sel_list", map[string]any{"host": "node01"})
	if isErr {
		t.Fatalf("sel_list: %s", out)
	}
	if len(out) > bmcTextMax+200 || !strings.Contains(out, "earlier bytes not shown") || !strings.Contains(out, "4999 |") {
		t.Errorf("not bounded or lost the newest entries: %d bytes", len(out))
	}
}

func TestReadToolsTimeOut(t *testing.T) {
	old := readTimeout
	readTimeout = 150 * time.Millisecond
	t.Cleanup(func() { readTimeout = old })
	h := start(t, Options{}, "")
	release := make(chan struct{})
	h.srv.be.Driver = func(name string, _ config.Host) bmc.Driver { return &blockingDriver{release: release} }
	t.Cleanup(func() { close(release) })
	start := time.Now()
	out, isErr := h.call("status", map[string]any{"filter": "node01"})
	if !isErr || !strings.Contains(out, "timed out") {
		t.Errorf("got %q %v", out, isErr)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("the tool didn't give up in time")
	}
}

// blockingDriver hangs until released, to test timeouts.
type blockingDriver struct {
	bmc.Driver
	release chan struct{}
}

func (d *blockingDriver) Power(string) error { <-d.release; return nil }

// The state-changing tools and what each should call on the driver.
var changeCases = []struct {
	tool string
	args map[string]any
	call string // what the driver should see
	says string // what the user is shown
}{
	{"power", map[string]any{"host": "node01", "action": "cycle"}, "node01:power cycle", "power cycle node01"},
	{"power", map[string]any{"host": "node01", "action": "off"}, "node01:power off", "power off node01"},
	{"power", map[string]any{"host": "node01", "action": "on"}, "node01:power on", "power on node01"},
	{"power", map[string]any{"host": "node01", "action": "reset"}, "node01:power reset", "power reset node01"},
	{"power", map[string]any{"host": "node01", "action": "soft"}, "node01:power soft", "power soft node01"},
	{"boot", map[string]any{"host": "node01", "device": "pxe", "legacy": true, "persistent": true}, "node01:boot pxe legacy=true persistent=true", "next boot device pxe"},
	{"boot", map[string]any{"host": "node01", "device": "disk"}, "node01:boot disk legacy=false persistent=false", "next boot device disk"},
	{"nmi", map[string]any{"host": "node01"}, "node01:nmi", "nmi node01"},
	{"sel_clear", map[string]any{"host": "node01"}, "node01:sel clear", "sel clear node01"},
	{"mc_reset", map[string]any{"host": "node01", "kind": "cold"}, "node01:mcreset cold", "cold BMC reset node01"},
	{"mc_reset", map[string]any{"host": "node01", "kind": "warm"}, "node01:mcreset warm", "warm BMC reset node01"},
}

func TestStateChangingToolsAlwaysAsk(t *testing.T) {
	for _, mode := range []struct {
		name   string
		answer string // "" means the client can't ask
		runs   bool
		says   string
	}{
		{"client can't ask", "", false, "can't ask for it"},
		{"user declines", "decline", false, "the user declined"},
		{"user cancels", "cancel", false, "the approval prompt was dismissed"},
		{"user accepts without ticking", "unticked", false, "without \"Allow it\" ticked"},
		{"user approves", "accept", true, ""},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, c := range changeCases {
				h := start(t, Options{}, mode.answer)
				out, isErr := h.call(c.tool, c.args)
				ran := strings.Contains(h.called(), c.call)
				if ran != mode.runs {
					t.Errorf("%s %v: ran = %v, want %v; calls %q; said %q", c.tool, c.args, ran, mode.runs, h.called(), out)
				}
				if mode.runs == isErr {
					t.Errorf("%s: isError = %v, said %q", c.tool, isErr, out)
				}
				if mode.says != "" && !strings.Contains(out, mode.says) {
					t.Errorf("%s said %q, want it to mention %q", c.tool, out, mode.says)
				}
				if mode.answer != "" && !strings.Contains(h.askedAll(), c.says) {
					t.Errorf("%s: the user was asked %q, want it to show %q", c.tool, h.askedAll(), c.says)
				}
				if mode.answer != "" && !strings.Contains(h.askedAll(), "192.0.2.11") {
					t.Errorf("%s: the approval doesn't name the host's address: %q", c.tool, h.askedAll())
				}
				if strings.Contains(h.askedAll()+out, secretPass) {
					t.Errorf("%s leaked a secret", c.tool)
				}
			}
		})
	}
}

func TestApprovalOnOlderProtocol(t *testing.T) {
	for answer, runs := range map[string]bool{"accept": true, "decline": false} {
		h := startVersion(t, Options{}, answer, "2025-11-25")
		h.call("power", map[string]any{"host": "node01", "action": "cycle"})
		if got := strings.Contains(h.called(), "node01:power cycle"); got != runs {
			t.Errorf("%s: ran = %v", answer, got)
		}
	}
}

func TestStateChangingToolsWantExactHostNames(t *testing.T) {
	h := start(t, Options{}, "accept")
	for _, c := range changeCases {
		args := map[string]any{}
		for k, v := range c.args {
			args[k] = v
		}
		args["host"] = "node0" // a prefix of node01 and node02
		if out, isErr := h.call(c.tool, args); !isErr || !strings.Contains(out, "no such host") {
			t.Errorf("%s with a prefix: %q %v", c.tool, out, isErr)
		}
		args["host"] = "login" // a unique prefix
		if out, isErr := h.call(c.tool, args); !isErr || !strings.Contains(out, "no such host") {
			t.Errorf("%s with a unique prefix: %q %v", c.tool, out, isErr)
		}
	}
	if h.askedAll() != "" || strings.Contains(h.called(), "power") {
		t.Errorf("nothing should have been asked or run; asked %q, calls %q", h.askedAll(), h.called())
	}
}

func TestChangeToolsRejectBadValues(t *testing.T) {
	h := start(t, Options{}, "accept")
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"power", map[string]any{"host": "node01", "action": "status"}},
		{"power", map[string]any{"host": "node01", "action": "explode"}},
		{"boot", map[string]any{"host": "node01", "device": "floppy"}},
		{"uid", map[string]any{"host": "node01", "state": "blink"}},
		{"mc_reset", map[string]any{"host": "node01", "kind": "hot"}},
		{"power", map[string]any{"host": "node01"}},
	} {
		res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
		if err == nil && !res.IsError {
			t.Errorf("%s %v was accepted", c.tool, c.args)
		}
	}
	if h.askedAll() != "" || strings.Contains(h.called(), "node01:") {
		t.Errorf("bad values reached the user or the driver: asked %q calls %q", h.askedAll(), h.called())
	}
}

// An approval is for one call: it can't be replayed, or used for another.
func TestApprovalIsForThatCallOnly(t *testing.T) {
	h := start(t, Options{}, "accept")
	s := h.srv
	caps := &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}}
	req := func(host, state string) *mcp.CallToolRequest {
		args, _ := json.Marshal(map[string]any{"host": host, "action": "cycle"})
		return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
			Meta: mcp.Meta{mcp.MetaKeyClientCapabilities: caps}, Name: "power", Arguments: args, RequestState: state,
			InputResponses: mcp.InputResponseMap{approvalID: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"allow": true}}},
		}}
	}
	why := []string{whyRestart}
	dev := func(n string) target { return target{name: n} }
	asking, err := s.approve(req("node01", ""), dev("node01"), "power cycle node01", why)
	if err != nil || asking == nil || asking.RequestState == "" {
		t.Fatalf("first call should ask: %v, %v", asking, err)
	}
	if _, err := s.approve(req("node02", asking.RequestState), dev("node02"), "power cycle node02", why); err == nil {
		t.Error("an approval for node01 was accepted for node02")
	}
	asking, _ = s.approve(req("node01", ""), dev("node01"), "power cycle node01", why)
	if _, err := s.approve(req("node01", asking.RequestState), dev("node01"), "power off node01", why); err == nil {
		t.Error("an approval was accepted for different text on the same call")
	}
	asking, _ = s.approve(req("node01", ""), dev("node01"), "power cycle node01", why)
	state := asking.RequestState
	if res, err := s.approve(req("node01", state), dev("node01"), "power cycle node01", why); res != nil || err != nil {
		t.Errorf("the approved call should go ahead: %v, %v", res, err)
	}
	if _, err := s.approve(req("node01", state), dev("node01"), "power cycle node01", why); err == nil {
		t.Error("an approval was used twice")
	}
	if _, err := s.approve(req("node01", "made-up"), dev("node01"), "power cycle node01", why); err == nil {
		t.Error("a made-up approval was accepted")
	}
	// Without the client saying it can ask, it is refused, whatever the answer says.
	noCaps := req("node01", "")
	delete(noCaps.Params.Meta, mcp.MetaKeyClientCapabilities)
	if res, err := s.approve(noCaps, dev("node01"), "power cycle node01", why); res != nil || err == nil {
		t.Errorf("a client that can't ask got %v, %v", res, err)
	}
}

func TestChangeToolsRunOneAtATimeAndStayQuiet(t *testing.T) {
	h := start(t, Options{}, "accept")
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, isErr := h.call("power", map[string]any{"host": "node01", "action": "cycle"})
			if isErr || !strings.Contains(out, "Chassis Power Control: cycle") || strings.Count(out, "Chassis") != 1 {
				t.Errorf("got %q %v", out, isErr)
			}
		}()
	}
	wg.Wait()
}

// If the queue gives up, errQueued must mean fn never ran: expiry and the
// callback starting are decided atomically.
func TestRunBMCQueuedMeansNeverRan(t *testing.T) {
	old := queueTimeout
	queueTimeout = 3 * time.Millisecond
	t.Cleanup(func() { queueTimeout = old })
	s := &server{}
	for i := 0; i < 300; i++ {
		release := make(chan struct{})
		held := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			bmc.Capture(context.Background(), func() error { close(held); <-release; return nil })
		}()
		<-held
		var ran atomic.Bool
		go func() {
			time.Sleep(2*time.Millisecond + time.Duration(i%20)*100*time.Microsecond)
			close(release)
		}()
		_, err := s.runBMC(context.Background(), time.Second, func() error { ran.Store(true); return nil })
		<-done
		time.Sleep(3 * time.Millisecond) // a late start would show now
		if errors.Is(err, errQueued) && ran.Load() {
			t.Fatalf("iteration %d: runBMC said it never started, but fn ran", i)
		}
		if err == nil && !ran.Load() {
			t.Fatalf("iteration %d: success without running fn", i)
		}
	}
}
