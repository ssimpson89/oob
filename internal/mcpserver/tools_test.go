package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/bmc"
	"github.com/ssimpson89/oob/internal/config"
)

// holdCapture takes bmc.Capture's one slot until the returned function is called.
func holdCapture(t *testing.T) func() {
	t.Helper()
	release := make(chan struct{})
	held := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		bmc.Capture(context.Background(), func() error { close(held); <-release; return nil })
	}()
	<-held
	var once sync.Once
	free := func() { once.Do(func() { close(release); <-done }) }
	t.Cleanup(free)
	return free
}

// A call queued behind another is not timed out for waiting: its own time
// starts when it gets its turn.
func TestQueueingIsNotATimeout(t *testing.T) {
	old := readTimeout
	readTimeout = 300 * time.Millisecond
	t.Cleanup(func() { readTimeout = old })
	h := start(t, Options{}, "")
	free := holdCapture(t)
	go func() { time.Sleep(900 * time.Millisecond); free() }() // three times the call's own limit
	out, isErr := h.call("status", map[string]any{"filter": "node01"})
	if isErr || !strings.Contains(out, "Chassis Power is on") {
		t.Errorf("a healthy host was reported as failed for queueing: %q %v", out, isErr)
	}
}

// Waiting too long for a turn says so, and says the host wasn't contacted.
func TestQueuedTooLongIsNotAHostTimeout(t *testing.T) {
	old := queueTimeout
	queueTimeout = 200 * time.Millisecond
	t.Cleanup(func() { queueTimeout = old })
	h := start(t, Options{}, "")
	holdCapture(t)
	out, isErr := h.call("sel_list", map[string]any{"host": "node01"})
	if !isErr || !strings.Contains(out, "did not start") || !strings.Contains(out, "behind other BMC calls") || strings.Contains(out, "timed out") {
		t.Errorf("got %q %v", out, isErr)
	}
	if strings.Contains(h.called(), "node01:sel") {
		t.Errorf("the call ran: %q", h.called())
	}
}

type selFailDriver struct {
	bmc.Driver
}

func (selFailDriver) SEL(string) error {
	fmt.Fprintln(bmc.Stdout(), "Error: Unable to establish IPMI v2 / RMCP+ session")
	return fmt.Errorf("exit status 1")
}

func TestIPMIFailureOnILOAndIDRACHintsAtIPMIOverLAN(t *testing.T) {
	h := start(t, Options{}, "")
	h.srv.be.IPMI = func(host config.Host, args ...string) (int, error) {
		fmt.Fprintln(bmc.Stdout(), "Error: Unable to establish IPMI v2 / RMCP+ session")
		return 1, nil
	}
	h.srv.be.Driver = func(string, config.Host) bmc.Driver { return selFailDriver{} }
	for _, c := range []struct {
		tool, host string
	}{{"sensors", "node02"}, {"bmc_info", "login1"}, {"sel_list", "node02"}} {
		out, isErr := h.call(c.tool, map[string]any{"host": c.host})
		if !isErr || !strings.Contains(out, "RMCP+") || !strings.Contains(out, "Enable it") || !strings.Contains(out, "IPMI/DCMI over LAN") {
			t.Errorf("%s: %q %v", c.tool, out, isErr)
		}
	}
	// Other failures and successes get no hint.
	h.srv.be.IPMI = func(host config.Host, args ...string) (int, error) {
		fmt.Fprintln(bmc.Stdout(), "Error: something else")
		return 1, nil
	}
	if out, _ := h.call("sensors", map[string]any{"host": "node02"}); strings.Contains(out, "Hint") {
		t.Errorf("unrelated failure got a hint: %q", out)
	}
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		switch tool.Name {
		case "sensors", "bmc_info", "sel_list":
			if !strings.Contains(tool.Description, "IPMI over LAN") {
				t.Errorf("%s doesn't mention IPMI over LAN: %q", tool.Name, tool.Description)
			}
		}
	}
}

// Hosts the overall time ran out before are "skipped", not reported like a dead BMC.
func TestStatusSkippedIsNotTimedOut(t *testing.T) {
	oldO := statusOverall
	statusOverall = 300 * time.Millisecond
	t.Cleanup(func() { statusOverall = oldO })
	h := start(t, Options{}, "")
	bigConfig(h, statusWorkers+10)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.srv.be.PowerStatus = func(string, config.Host, time.Duration) bmc.StatusResult {
		<-release
		return bmc.StatusResult{Text: "on"}
	}
	res := h.result("status", map[string]any{"filter": "big*"})
	b, _ := json.Marshal(res.StructuredContent)
	var out statusOut
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	var skipped, timedOut int
	for _, r := range out.Hosts {
		switch {
		case r.Status == statusSkipped:
			skipped++
		case strings.Contains(r.Status, "timed out"):
			timedOut++
		}
		if r.OK {
			t.Errorf("%s is ok", r.Name)
		}
	}
	if skipped != 10 || timedOut != statusWorkers || !strings.Contains(statusSkipped, "retry with a smaller limit") {
		t.Errorf("skipped %d, timed out %d of %d rows", skipped, timedOut, len(out.Hosts))
	}
}

// uid is a harmless LED: it runs even when the client can't ask.
func TestUIDNeedsNoApproval(t *testing.T) {
	h := start(t, Options{}, "")
	out, isErr := h.call("uid", map[string]any{"host": "node01", "state": "on"})
	if isErr || !strings.Contains(h.called(), "node01:uid on") || len(h.asked) != 0 {
		t.Errorf("uid: %q %v calls %q asked %v", out, isErr, h.called(), h.asked)
	}
	if out, isErr := h.call("uid", map[string]any{"host": "nope", "state": "on"}); !isErr {
		t.Errorf("unknown host accepted: %q", out)
	}
}

func TestStatusOfOneHostIsThePowerCheck(t *testing.T) {
	h := start(t, Options{}, "")
	out, isErr := h.call("status", map[string]any{"filter": "node01"})
	if isErr || strings.TrimSpace(out) != "Chassis Power is on" || !strings.Contains(h.called(), "node01:power status") {
		t.Errorf("one host: %q %v %q", out, isErr, h.called())
	}
	res := h.result("status", map[string]any{"filter": "node01"})
	b, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(b), `"name":"node01"`) || !strings.Contains(string(b), `"ok":true`) {
		t.Errorf("structured %s", b)
	}
}

func TestReadOutputHistory(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	h.send("s1", "uname -r")
	h.host("node01").emit("kernel: tick\r\n")
	if out, isErr := h.call("read_output", map[string]any{"session": "s1", "lines": 5, "wait_seconds": 1}); !isErr || !strings.Contains(out, "can't be combined") {
		t.Errorf("lines with wait_seconds: %q %v", out, isErr)
	}
	eventually(t, "tick is in the history", func() bool {
		out, _ := h.call("read_output", map[string]any{"session": "s1", "search": "tick"})
		return strings.Contains(out, "kernel: tick")
	})
	// Reading history leaves the new output unread.
	if out, _ := h.call("read_output", map[string]any{"session": "s1"}); !strings.Contains(out, "kernel: tick") {
		t.Errorf("history marked the output read: %q", out)
	}
}
