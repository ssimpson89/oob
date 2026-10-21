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
	w, err := share.Open(context.Background(), share.Options{Host: "node01"})
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

func TestAgentJoinsTheUsersConsole(t *testing.T) {
	h := start(t, Options{}, "")
	// The user's oob sol opens it first.
	f := newFakeHost(t, "node01")
	user, err := share.Open(context.Background(), share.Options{Host: "node01", NoLinger: true,
		Open: func() (*bmc.ConsoleTransport, error) { return f.transport(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { user.Close() })
	out, _ := h.call("connect", map[string]any{"host": "node01"})
	if !strings.Contains(out, "Joined node01") {
		t.Errorf("connect: %q", out)
	}
	if got, isErr := h.send("s1", "uname -r"); isErr || !strings.Contains(got, "5.14.0-test") {
		t.Errorf("send_command on a joined console: %q %v", got, isErr)
	}
}
