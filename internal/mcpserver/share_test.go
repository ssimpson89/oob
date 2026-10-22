// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.

package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/bmc"
	"github.com/ssimpson89/oob/internal/share"
)

// window joins the console the agent opened, the way `oob sol` does.
func window(t *testing.T) *share.Client {
	t.Helper()
	w, err := share.Open(context.Background(), share.Options{Host: "node01", Addr: "192.0.2.11:22", Type: "ipmi"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func TestAWindowJoinsTheAgentsConsole(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	w := window(t)
	if w.Joined() != true || w.Status().Agent == "" {
		t.Errorf("joined %v, agent %q", w.Joined(), w.Status().Agent)
	}
	out, isErr := h.send("s1", "uname -r")
	if isErr || !strings.Contains(out, "5.14.0-test") {
		t.Fatalf("send_command with a window on the console: %q %v", out, isErr)
	}
	buf := make([]byte, 4096)
	var seen string
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(seen, "5.14.0-test") && time.Now().Before(deadline) {
		n, _ := w.Transport().Out.Read(buf)
		seen += string(buf[:n])
	}
	if !strings.Contains(seen, "5.14.0-test") {
		t.Errorf("the window never saw the agent's command output: %q", seen)
	}
}

func TestPausingTheAgentRefusesItsCommandsWithAClearMessage(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	w := window(t)
	if err := w.Window().TogglePause(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pause reaches the agent", func() bool {
		out, isErr := h.send("s1", "uname -r")
		return isErr && strings.Contains(out, "Paused by the user in oob")
	})
	if got := h.host("node01").entered(); len(got) != 0 {
		t.Errorf("the host ran %q while paused", got)
	}
	if err := w.Window().TogglePause(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "resumed", func() bool {
		out, isErr := h.send("s1", "uname -r")
		return !isErr && strings.Contains(out, "5.14.0-test")
	})
}

func TestAgentDisconnectLeavesTheConsoleForTheUsersWindow(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	w := window(t)
	if out, isErr := h.call("disconnect", map[string]any{"session": "s1"}); isErr {
		t.Fatalf("disconnect: %q", out)
	}
	f := h.host("node01")
	time.Sleep(100 * time.Millisecond)
	if f.isClosed() {
		t.Fatal("disconnecting the agent closed the console under the user's window")
	}
	w.Enqueue([]byte("echo hi\r"))
	eventually(t, "the window can still type", func() bool { return len(f.entered()) == 1 })
	w.Close() // the last window leaves
	eventually(t, "the console closes with the last window", f.isClosed)
}

// userConsole is the console the user's oob sol opened first, for the agent to
// find when it connects.
func userConsole(t *testing.T) {
	t.Helper()
	f := newFakeHost(t, "node01")
	user, err := share.Open(context.Background(), share.Options{Host: "node01", Addr: "192.0.2.11:22", Type: "ipmi", NoLinger: true,
		Open: func() (*bmc.ConsoleTransport, error) { return f.transport(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { user.Close() })
}

// Joining the console the user has open puts the agent in their live session:
// it is only done once they approve, and the question says so.
func TestAgentJoinsTheUsersConsoleOnlyWithApproval(t *testing.T) {
	for _, c := range []struct {
		answer, want string
		joins        bool
	}{{"", "can't ask", false}, {"decline", "the user declined", false}, {"accept", "Joined node01", true}} {
		t.Run(c.answer, func(t *testing.T) {
			h := start(t, Options{}, c.answer)
			userConsole(t)
			out, _ := h.call("connect", map[string]any{"host": "node01"})
			if !strings.Contains(out, c.want) {
				t.Errorf("connect: %q, want %q", out, c.want)
			}
			if c.answer != "" && !strings.Contains(h.askedAll(), "live session will be shared") {
				t.Errorf("the question didn't say the live session is shared: %q", h.askedAll())
			}
			got, isErr := h.send("s1", "uname -r")
			if c.joins != (!isErr && strings.Contains(got, "5.14.0-test")) {
				t.Errorf("send_command: %q %v, joins = %v", got, isErr, c.joins)
			}
		})
	}
}

// Hosting a fresh console takes no approval: nobody else is on it.
func TestAgentHostingAFreshConsoleAsksNothing(t *testing.T) {
	h := start(t, Options{}, "decline")
	if out, isErr := h.call("connect", map[string]any{"host": "node01"}); isErr || !strings.Contains(out, "Connected to node01") {
		t.Fatalf("connect: %q %v", out, isErr)
	}
	if asked := h.askedAll(); asked != "" {
		t.Errorf("the user was asked: %q", asked)
	}
}

// movedBeforeKeyboard makes the server drop into a bootloader just as the
// call goes for the keyboard, and waits until the session has seen it.
func movedBeforeKeyboard(t *testing.T, f *fakeHost) {
	t.Helper()
	old := beforeKeyboard
	t.Cleanup(func() { beforeKeyboard = old })
	beforeKeyboard = func(c *session) {
		beforeKeyboard = old // once
		f.emit("\r\ngrub> ")
		deadline := time.Now().Add(5 * time.Second)
		for !c.line().atBootloader() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestSendCommandStopsIfTheConsoleMovedBeforeItHadTheKeyboard(t *testing.T) {
	h := start(t, Options{}, "accept")
	h.connect()
	f := h.host("node01")
	movedBeforeKeyboard(t, f)
	// uname needs no approval at a shell prompt, but at a bootloader anything does.
	out, isErr := h.send("s1", "uname -r")
	if !isErr || !strings.Contains(out, "console changed") || !strings.Contains(out, "bootloader") {
		t.Fatalf("send_command: %q %v", out, isErr)
	}
	if got := f.entered(); len(got) != 0 {
		t.Fatalf("the host got %q after the console moved", got)
	}
	if n := len(h.asked); n != 0 {
		t.Errorf("asked %d times before the stop", n)
	}
	// The next call is checked against where the console is now, and asks.
	h.send("s1", "uname -r")
	if n := len(h.asked); n != 1 {
		t.Errorf("the user was asked %d times, want 1 (a fresh approval for the bootloader)", n)
	}
}

func TestSendKeysStopsIfTheConsoleMovedBeforeItHadTheKeyboard(t *testing.T) {
	h := start(t, Options{}, "accept")
	h.connect()
	f := h.host("node01")
	movedBeforeKeyboard(t, f)
	// An approved command: the approval was for the shell prompt.
	out, isErr := h.call("send_keys", map[string]any{"session": "s1", "text": "reboot", "keys": []string{"enter"}})
	if !isErr || !strings.Contains(out, "console changed") {
		t.Fatalf("send_keys: %q %v", out, isErr)
	}
	if got := f.entered(); len(got) != 0 {
		t.Fatalf("the host got %q after the console moved", got)
	}
	h.srv.mu.Lock()
	c := h.srv.sessions["s1"]
	h.srv.mu.Unlock()
	c.mu.Lock()
	at := c.answersAt
	c.mu.Unlock()
	if at != -1 {
		t.Errorf("answersAt = %d, the stale approval was kept", at)
	}
}

func TestAnApprovalDoesNotCarryToAnotherPlaceOnTheConsole(t *testing.T) {
	h := start(t, Options{}, "accept")
	h.connect()
	f := h.host("node01")
	h.mu.Lock()
	h.onAsk = func() { // the user is deciding; the server falls into a bootloader
		f.emit("\r\ngrub> ")
		for i := 0; i < 1000 && !h.srv.sessions["s1"].line().atBootloader(); i++ {
			time.Sleep(5 * time.Millisecond)
		}
		h.onAsk = nil
	}
	h.mu.Unlock()
	out, isErr := h.send("s1", "reboot")
	if !isErr || !strings.Contains(out, "different call") {
		t.Fatalf("send_command: %q %v", out, isErr)
	}
	if got := f.entered(); len(got) != 0 {
		t.Fatalf("the host got %q on an approval for the shell prompt", got)
	}
}

func TestABreakSentFromAWindowCountsForTheAgentsApprovalChecks(t *testing.T) {
	h := start(t, Options{}, "accept")
	h.connect()
	w := window(t)
	if err := w.Transport().Link.Break(); err != nil {
		t.Fatal(err)
	}
	h.srv.mu.Lock()
	c := h.srv.sessions["s1"]
	h.srv.mu.Unlock()
	eventually(t, "the agent's session learns of the BREAK", c.sinceBreak)
	// The next key could be a SysRq command, so even uname needs the user's say.
	out, isErr := h.send("s1", "uname -r")
	if isErr || !strings.Contains(out, "5.14.0-test") {
		t.Fatalf("send_command: %q %v", out, isErr)
	}
	if n := len(h.asked); n != 1 || !strings.Contains(h.asked[0], "right after a BREAK") {
		t.Errorf("approvals asked: %q, want one about the BREAK", h.asked)
	}
}

func TestAnAgentJoiningJustAfterAWindowsBreakKnowsOfIt(t *testing.T) {
	h := start(t, Options{}, "accept")
	f := newFakeHost(t, "node01")
	user, err := share.Open(context.Background(), share.Options{Host: "node01", Addr: "192.0.2.11:22", Type: "ipmi", NoLinger: true,
		Open: func() (*bmc.ConsoleTransport, error) { return f.transport(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { user.Close() })
	if err := user.Transport().Link.Break(); err != nil {
		t.Fatal(err)
	}
	out, _ := h.call("connect", map[string]any{"host": "node01"})
	if !strings.Contains(out, "Joined node01") {
		t.Fatalf("connect: %q", out)
	}
	h.srv.mu.Lock()
	c := h.srv.sessions["s1"]
	h.srv.mu.Unlock()
	eventually(t, "the joiner is told of the earlier BREAK", c.sinceBreak)
}

// The server's exit ends the console even with the user's window on it: the
// window is told, and the BMC session is released.
func TestServerExitReleasesAConsoleAWindowIsOn(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	w := window(t)
	f := h.host("node01")
	h.srv.closeAll()
	select {
	case <-w.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the window was never told the console closed")
	}
	eventually(t, "the BMC session is released", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.released })
	if !f.isClosed() {
		t.Error("the console was left open")
	}
}
