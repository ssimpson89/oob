package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 4340518873: an approval is bound to the address and type it was asked about,
// so reloading the host file between ask and retry can't move it to a new host.
func TestApprovalIsBoundToAddressAndType(t *testing.T) {
	h := start(t, Options{}, "accept")
	s := h.srv
	caps := &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}}
	req := func(state string) *mcp.CallToolRequest {
		args, _ := json.Marshal(map[string]any{"host": "node01", "action": "cycle"})
		return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
			Meta: mcp.Meta{mcp.MetaKeyClientCapabilities: caps}, Name: "power", Arguments: args, RequestState: state,
			InputResponses: mcp.InputResponseMap{approvalID: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"allow": true}}},
		}}
	}
	why := []string{whyRestart}
	old := target{name: "node01", addr: "192.0.2.11", typ: "ipmi"}
	for _, moved := range []target{
		{name: "node01", addr: "192.0.2.99", typ: "ipmi"},
		{name: "node01", addr: "192.0.2.11", typ: "ilo"},
	} {
		asking, err := s.approve(req(""), old, "power cycle node01", why)
		if err != nil || asking == nil {
			t.Fatalf("first call should ask: %v %v", asking, err)
		}
		if res, err := s.approve(req(asking.RequestState), moved, "power cycle node01", why); err == nil || res != nil {
			t.Errorf("an approval for %+v was accepted for %+v", old, moved)
		}
	}
	asking, _ := s.approve(req(""), old, "power cycle node01", why)
	if res, err := s.approve(req(asking.RequestState), old, "power cycle node01", why); res != nil || err != nil {
		t.Errorf("the unchanged host should go ahead: %v %v", res, err)
	}
}

// 4340518873, console side: the session keeps the address it connected to, so
// the user is shown it even when the host file changes afterwards.
func TestSessionApprovalShowsTheConnectedAddress(t *testing.T) {
	h := start(t, Options{}, "decline")
	h.connect()
	moved := h.cfg.Hosts["node01"]
	moved.IP = "192.0.2.99"
	h.cfg.Hosts["node01"] = moved
	h.send("s1", "reboot")
	if asked := h.askedAll(); !strings.Contains(asked, "192.0.2.11") || strings.Contains(asked, "192.0.2.99") {
		t.Errorf("the user was shown %q", asked)
	}
}

// 4340518934
func TestAliasKeysAreNotOffered(t *testing.T) {
	h := start(t, Options{}, "accept")
	h.connect()
	for _, k := range []string{"ctrl+m", "ctrl+j", "ctrl+o", "ctrl+i"} {
		if _, ok := keyBytes[k]; ok {
			t.Errorf("%s is in keyBytes", k)
		}
		before := h.host("node01").rawSent()
		if out, isErr := h.keys("s1", "reboot", k); !isErr {
			t.Errorf("%s was accepted: %q", k, out)
		}
		if h.host("node01").rawSent() != before {
			t.Errorf("%s reached the host", k)
		}
	}
	if _, ok := keyBytes["ctrl+u"]; !ok {
		t.Error("ctrl+u is gone")
	}
	if strings.Contains(h.askedAll(), "reboot") {
		t.Error("the user was asked about a key that should have been rejected up front")
	}
}

// 4340519114: only exact read-only forms of the partitioning tools skip approval.
func TestListOnlyIsExact(t *testing.T) {
	for _, l := range []string{
		"fdisk -l", "fdisk -l /dev/sda", "sfdisk --list", "sfdisk -l /dev/sda", "gdisk -l /dev/sda", "sgdisk -p /dev/sda",
		"sgdisk --print /dev/sda", "parted -l", "parted /dev/sda print", "parted -s /dev/sda print", "parted -s /dev/sda unit gb print free",
	} {
		if why := risk(l); why != "" {
			t.Errorf("%q flagged (%s)", l, why)
		}
	}
	for _, l := range []string{
		"sgdisk --zap-all --print /dev/sda", "sgdisk -p --zap-all /dev/sda", "sgdisk -p /dev/sda -o", "sgdisk -l backup.gpt /dev/sda",
		"parted -s /dev/sda rm 1 print", "parted -s /dev/sda mklabel gpt print", "parted /dev/sda print mklabel gpt",
		"parted /dev/sda print; parted /dev/sda rm 1", "fdisk -l; fdisk /dev/sda", "fdisk -l /dev/sda > /dev/sdb",
		"sfdisk -l --delete /dev/sda 1", "sfdisk /dev/sda < layout", "gdisk /dev/sda", "gdisk -l /dev/sda -x", "fdisk /dev/sda",
		"parted -s /dev/sda print resizepart 1 100%", "fdisk -l /dev/sda $(fdisk /dev/sdb)",
	} {
		if risk(l) == "" {
			t.Errorf("%q was exempted", l)
		}
	}
}

// 4340519156: any key that edits or recalls the line leaves it unknown, so the
// next Enter needs approval, also within one batch of keys.
func TestEditingKeysMakeTheLineUnknown(t *testing.T) {
	for _, k := range []string{"home", "end", "delete", "left", "right", "up", "down", "tab", "ctrl+a", "ctrl+e", "ctrl+k",
		"ctrl+w", "ctrl+y", "ctrl+p", "ctrl+n", "ctrl+r", "ctrl+h", "esc"} {
		var ty typing
		ty.add("ls", "", 1)
		ty.add("", k, 1)
		if _, unknown := ty.pending(1); !unknown {
			t.Errorf("%s left the line known", k)
		}
		ty.add("", "ctrl+u", 1)
		if text, unknown := ty.pending(1); unknown || text != "" {
			t.Errorf("ctrl+u after %s left %q %v", k, text, unknown)
		}
	}
	var ty typing
	ty.add("", "left", 1)
	if _, unknown := ty.pending(2); unknown {
		t.Error("a new prompt didn't clear it")
	}
}

func TestEnterAfterAnEditingKeyNeedsApproval(t *testing.T) {
	for _, k := range []string{"home", "ctrl+a", "ctrl+w", "left", "ctrl+r"} {
		// Across calls, with no way to ask: refused, and nothing is entered.
		h := start(t, Options{}, "")
		h.connect()
		if out, isErr := h.keys("s1", "", k); isErr {
			t.Fatalf("%s: %q", k, out)
		}
		if out, isErr := h.keys("s1", "", "enter"); !isErr || !strings.Contains(out, "approval") {
			t.Errorf("%s then enter: %q %v", k, out, isErr)
		}
		if got := h.host("node01").entered(); len(got) != 0 {
			t.Errorf("%s then enter entered %q", k, got)
		}
		// Cleared with ctrl+u, enter goes through.
		h.keys("s1", "", "ctrl+u")
		if out, isErr := h.keys("s1", "", "enter"); isErr {
			t.Errorf("%s, ctrl+u, enter: %q", k, out)
		}
		// Within one batch.
		h = start(t, Options{}, "")
		h.connect()
		if out, isErr := h.keys("s1", "uname -r", k, "enter"); !isErr || !strings.Contains(out, "approval") {
			t.Errorf("batch %s: %q %v", k, out, isErr)
		}
		if got := h.host("node01").entered(); len(got) != 0 {
			t.Errorf("batch %s entered %q", k, got)
		}
	}
	// send_command refuses to run over an edited line, too.
	h := start(t, Options{}, "")
	h.connect()
	h.keys("s1", "", "home")
	if out, isErr := h.send("s1", "uname -r"); !isErr || !strings.Contains(out, "nothing was sent") {
		t.Errorf("send_command over an edited line: %q %v", out, isErr)
	}
	if got := h.host("node01").entered(); len(got) != 0 {
		t.Errorf("send_command entered %q", got)
	}
}

// 4340519210: echo, output and an identical prompt in one chunk still count as
// a new prompt, so an approval expires however the output was chunked.
func TestPromptCountDoesNotDependOnChunking(t *testing.T) {
	prompt := "[root@node01 ~]# "
	run := "uname -r\r\n5.14.0-test\r\n" + prompt
	counts := func(chunks ...string) int {
		h := start(t, Options{}, "")
		h.connect()
		c, err := h.srv.session("s1")
		if err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		before := c.prompts
		c.mu.Unlock()
		for _, ch := range chunks {
			c.feed([]byte(ch))
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.prompts - before
	}
	whole := counts(run)
	var bytewise []string
	for _, r := range run {
		bytewise = append(bytewise, string(r))
	}
	if whole != 1 {
		t.Errorf("one chunk counted %d prompts, want 1", whole)
	}
	if got := counts(bytewise...); got != 1 {
		t.Errorf("byte by byte counted %d prompts, want 1", got)
	}
	if got := counts("uname -r\r\n5.14.0-test\r\n", prompt); got != 1 {
		t.Errorf("split before the prompt counted %d, want 1", got)
	}
}

// And the approval for an answer is gone once the command that raised the
// question has finished, even when the next prompt is identical.
func TestAnswerApprovalExpiresWithTheNextPrompt(t *testing.T) {
	h := start(t, Options{}, "accept")
	h.connect()
	c, _ := h.srv.session("s1")
	c.mu.Lock()
	c.answersAt = c.prompts
	c.mu.Unlock()
	c.feed([]byte("dnf remove foo\r\nDone.\r\n[root@node01 ~]# "))
	c.mu.Lock()
	expired := c.answersAt != c.prompts
	c.mu.Unlock()
	if !expired {
		t.Error("the approval survived a finished command whose prompt was identical")
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 4340518996 (closeIdle): a tool that got the session before it was forgotten
// and waits for the send lock is let go, and sees the session is closed.
func TestIdleCloseReleasesWaitingTools(t *testing.T) {
	h := start(t, Options{IdleTimeout: 150 * time.Millisecond}, "")
	h.connect()
	c, err := h.srv.session("s1")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		// The tool found the session just before the sweep forgot it, and now
		// asks for the send lock.
		eventually(t, "the sweep forgets the session", func() bool {
			h.srv.mu.Lock()
			defer h.srv.mu.Unlock()
			return len(h.srv.sessions) == 0
		})
		c.sendMu.Lock()
		defer c.sendMu.Unlock()
		got <- c.canSend()
	}()
	select {
	case err := <-got:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Errorf("a tool that waited through the idle close got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a tool waiting for the send lock was never released")
	}
}

// 4340519263: a write that never completes can't hold the session or outlast the call.
func TestStuckWriteEndsWithTheCall(t *testing.T) {
	old := toolSlack
	toolSlack = 100 * time.Millisecond
	t.Cleanup(func() { toolSlack = old })
	for _, tool := range []string{"send_command", "send_keys"} {
		h := start(t, Options{}, "")
		h.connect()
		f := h.host("node01")
		f.setStuck()
		args := map[string]any{"session": "s1", "wait_seconds": 0.05}
		if tool == "send_command" {
			args["line"] = "uname -r"
		} else {
			args["text"] = "y"
		}
		start := time.Now()
		out, isErr := h.call(tool, args)
		if time.Since(start) > 8*time.Second {
			t.Errorf("%s took %s", tool, time.Since(start))
		}
		if !isErr || !strings.Contains(out, "stopped accepting input") {
			t.Errorf("%s: %q %v", tool, out, isErr)
		}
		eventually(t, "the transport is closed", f.isClosed)
		if got := f.rawSent(); got != "" {
			t.Errorf("%s: a late write landed: %q", tool, got)
		}
		out, isErr = h.call("read_output", map[string]any{"session": "s1"})
		if !isErr || !strings.Contains(out, "closed") {
			t.Errorf("%s: the session wasn't forgotten: %q %v", tool, out, isErr)
		}
		c := h.srv.sessions // forgotten sessions leave the table
		h.srv.mu.Lock()
		n := len(c)
		h.srv.mu.Unlock()
		if n != 0 {
			t.Errorf("%s: %d sessions left", tool, n)
		}
	}
}

// 4340519300: closing a session types nothing at the console, so staged input
// isn't submitted by an idle timeout, a disconnect or shutdown.
func TestClosingNeverTypesAtTheConsole(t *testing.T) {
	for _, how := range []string{"disconnect", "idle", "shutdown"} {
		h := start(t, Options{IdleTimeout: 200 * time.Millisecond}, "")
		h.connect()
		f := h.host("node01")
		if out, isErr := h.keys("s1", "uname -r"); isErr {
			t.Fatalf("%s: %q", how, out)
		}
		switch how {
		case "disconnect":
			h.call("disconnect", map[string]any{"session": "s1"})
		case "idle":
			eventually(t, "idle close", f.isClosed)
		case "shutdown":
			h.srv.closeAll()
		}
		eventually(t, how+": closed", f.isClosed)
		eventually(t, how+": released", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.released })
		f.mu.Lock()
		disc, first := f.disconnects, f.closedFirst
		f.mu.Unlock()
		if got := f.rawSent(); got != "uname -r" {
			t.Errorf("%s: the console was sent %q, want only the staged text", how, got)
		}
		if got := f.entered(); len(got) != 0 {
			t.Errorf("%s: staged input was run: %q", how, got)
		}
		if disc != 0 {
			t.Errorf("%s: Disconnect (which types an escape) was called %d times", how, disc)
		}
		if !first {
			t.Errorf("%s: the transport wasn't closed before Release", how)
		}
	}
}

// 4340518996: a later line of a batch is checked where the earlier ones left the console.
func TestBatchStopsWhenTheContextNeedsApproval(t *testing.T) {
	// Into a bootloader.
	h := start(t, Options{}, "")
	h.connect()
	out, isErr := h.send("s1", "grub\nboot")
	if isErr || !strings.Contains(out, "Stopped before line 2") || !strings.Contains(out, "bootloader") ||
		!strings.Contains(out, "Lines sent: 1") || !strings.Contains(out, "\nboot\n") {
		t.Errorf("grub then boot: %q %v", out, isErr)
	}
	if got := strings.Join(h.host("node01").entered(), "|"); got != "grub" {
		t.Errorf("the host got %q, want only grub", got)
	}
	// The rest isn't replayed or sneaked in: sending it again is a new call that needs approval.
	if out, isErr := h.send("s1", "boot"); !isErr || !strings.Contains(out, "approval") {
		t.Errorf("boot at the bootloader: %q %v", out, isErr)
	}
	if got := strings.Join(h.host("node01").entered(), "|"); got != "grub" {
		t.Errorf("the host got %q after the refused call", got)
	}

	// Into the BMC's own command line.
	h = start(t, Options{}, "")
	h.setup = func(f *fakeHost) {
		f.toggle = true
		f.respond = func(f *fakeHost, cmd string) bool {
			if cmd == "cli" {
				f.emit("\r\n</>hpiLO-> ")
				return true
			}
			return false
		}
	}
	h.connect()
	out, isErr = h.send("s1", "cli\nuname -r")
	if isErr || !strings.Contains(out, "Stopped before line 2") {
		t.Errorf("into the BMC: %q %v", out, isErr)
	}
	if got := strings.Join(h.host("node01").entered(), "|"); got != "cli" {
		t.Errorf("the host got %q, want only cli", got)
	}

	// A batch that was approved for the context it starts in goes on.
	h = start(t, Options{}, "accept")
	h.connect()
	h.send("s1", "grub")
	out, isErr = h.send("s1", "help\nls")
	if isErr || strings.Contains(out, "Stopped before") {
		t.Errorf("a batch approved at the bootloader was stopped: %q %v", out, isErr)
	}
	if got := strings.Join(h.host("node01").entered(), "|"); got != "grub|help|ls" {
		t.Errorf("the host got %q", got)
	}
}

// 4340519042: lines after a login or password prompt aren't typed into it.
func TestBatchStopsAtALoginPrompt(t *testing.T) {
	h := start(t, Options{}, "")
	h.setup = func(f *fakeHost) {
		f.respond = func(f *fakeHost, cmd string) bool {
			if cmd == "su" {
				f.emit("Password: ")
				return true
			}
			return false
		}
	}
	h.connect()
	out, isErr := h.send("s1", "su\nnot-a-real-password\nid")
	if isErr || !strings.Contains(out, "login or password prompt") || !strings.Contains(out, "weren't sent") {
		t.Errorf("got %q %v", out, isErr)
	}
	if got := strings.Join(h.host("node01").entered(), "|"); got != "su" {
		t.Errorf("the host got %q, want only su", got)
	}
}

// 4340519071: BREAK is sent after a carriage return, so it is refused while
// input is staged at the prompt, and nothing is submitted.
func TestBreakIsRefusedWithStagedInput(t *testing.T) {
	for _, staged := range []func(h *harness){
		func(h *harness) { h.keys("s1", "uname -r") },
		func(h *harness) { h.keys("s1", "", "left") },
	} {
		h := start(t, Options{}, "accept")
		h.connect()
		staged(h)
		out, isErr := h.call("send_break", map[string]any{"session": "s1"})
		f := h.host("node01")
		if !isErr || !strings.Contains(out, "ctrl+u") || f.breaks != 0 || len(f.entered()) != 0 || h.askedAll() != "" {
			t.Errorf("break over staged input: %q %v breaks=%d entered=%q asked=%q", out, isErr, f.breaks, f.entered(), h.askedAll())
		}
		h.keys("s1", "", "ctrl+u")
		if out, isErr := h.call("send_break", map[string]any{"session": "s1"}); isErr {
			t.Errorf("break after clearing the line: %q", out)
		}
		if f.breaks != 1 {
			t.Errorf("breaks = %d after clearing the line", f.breaks)
		}
	}
}
