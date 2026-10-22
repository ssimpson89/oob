// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.

//go:build unix

package share

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/console"
)

// slowOpen is Options.Open for a console whose typer waits an hour between
// bytes: the first byte of what is queued goes out, the rest stays queued.
func slowOpen(f *fakeConsole) func() (*console.Transport, error) {
	long := time.Hour
	return func() (*console.Transport, error) {
		tr := f.transport()
		tr.Pacing = console.Pacing{Char: &long, Line: &long}
		return tr, nil
	}
}

func (f *fakeConsole) wasReleased() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released
}

// A write from an AI tool that has left is not typed under the lease of the
// one that replaced it: the hub checks that the writer is still here, and is
// the AI tool, in the same step that queues the bytes.
func TestADepartedAgentsWriteIsNotTypedUnderAReplacementsLease(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	h := hubOf(t, win)
	first := agent(t)
	old := agentPeer(t, h)
	first.Close()
	eventually(t, "the first AI tool is gone", func() bool {
		var gone bool
		h.query(func() { _, here := h.peers[old]; gone = !here && h.agent == nil })
		return gone
	})
	second := agent(t)
	_, _, release, err := second.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var got error
	inHub(t, h, func() {
		h.agentWrite(old, msg{Op: opWrite, Data: []byte("reboot\r")}, func(err error) { got = err })
	})
	if !errors.Is(got, errGone) {
		t.Fatalf("a departed agent's write = %v, want errGone", got)
	}
	time.Sleep(100 * time.Millisecond)
	if s := f.got(); s != "" {
		t.Errorf("typed %q for a client that had left", s)
	}
}

// A console that can't be shared (the socket can't be made) is released at the
// BMC and the lock is let go, not just closed.
func TestASocketSetupFailureReleasesTheBMCSession(t *testing.T) {
	d := env(t)
	sock, _, _ := paths("node01", "")
	if err := os.MkdirAll(sock+"/in-the-way/x", 0o700); err != nil { // Listen can't take the path, and Remove can't clear it
		t.Fatal(err)
	}
	f := newFake()
	_, err := Open(context.Background(), Options{Host: "node01", NoLinger: true, Open: func() (*console.Transport, error) { return f.transport(), nil }})
	if err == nil || !strings.Contains(err.Error(), "can't share") {
		t.Fatalf("Open = %v, want the sharing error", err)
	}
	if !f.isClosed() || !f.wasReleased() {
		t.Errorf("closed %v, released %v: the BMC session was left behind", f.isClosed(), f.wasReleased())
	}
	if lockHeld(t) {
		t.Error("the lock is still held")
	}
	_ = d
}

// The same when the owner's own client can't join (the hub refuses it).
func TestAnOwnerHandshakeFailureReleasesTheBMCSession(t *testing.T) {
	env(t)
	hostHook = func(stage string, h *hub) {
		if stage == "listening" {
			h.query(func() { h.end(console.Result{}) }) // the console ends before its owner is let in
		}
	}
	t.Cleanup(func() { hostHook = nil })
	f := newFake()
	_, err := Open(context.Background(), Options{Host: "node01", NoLinger: true, Open: func() (*console.Transport, error) { return f.transport(), nil }})
	if err == nil {
		t.Fatal("Open succeeded though its own client was refused")
	}
	eventually(t, "the BMC session is released", f.wasReleased)
	if !f.isClosed() {
		t.Error("the transport was left open")
	}
	eventually(t, "the lock is let go", func() bool { return !lockHeld(t) })
}

// The host's own client has its slot before the socket is served: an AI tool
// at the socket can't take the AI slot from an owner that is itself one.
func TestTheOwnersSlotIsTakenBeforeTheSocketIsServed(t *testing.T) {
	env(t)
	var rival *wire
	hostHook = func(stage string, h *hub) {
		switch stage {
		case "listening":
			rival = rawAgent(t) // its hello waits in the socket until the accept loop runs
		case "joined":
			var owner *peer
			h.query(func() { owner = h.agent })
			if owner == nil {
				t.Error("the owner's AI client isn't registered when the socket is about to be served")
			}
		}
	}
	t.Cleanup(func() { hostHook = nil })
	f := newFake()
	owner := host(t, f, Options{Agent: true, Name: "Claude"})
	if owner.Status().Agent != "Claude" {
		t.Fatalf("the owner is not the AI tool: %+v", owner.Status())
	}
	for {
		m, err := rival.recv()
		if err != nil {
			t.Fatal(err)
		}
		if m.T == tBye {
			if !strings.Contains(m.Text, "another AI tool") {
				t.Errorf("refused as %q", m.Text)
			}
			return
		}
		if m.T == tState {
			t.Fatal("the rival took the AI slot from the owner")
		}
	}
}

// What a window typed counts from what went out. A paste cleared part way
// leaves its first bytes on the line, and the AI tool stays out until the user
// finishes or clears the line, even once the typing window has passed.
func TestAClearedPartialPasteKeepsTheAgentBlockedUntilTheLineIsDone(t *testing.T) {
	env(t)
	f := newFake()
	win, err := Open(context.Background(), Options{Host: "node01", NoLinger: true, Open: slowOpen(f)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { win.Close() })
	h := win.holder.hub
	ai := agent(t)
	p := agentPeer(t, h)
	win.Enqueue([]byte("abc\r")) // "a" goes out, the rest waits
	eventually(t, "the first byte is out", func() bool { return f.got() == "a" })
	win.Clear()
	eventually(t, "the rest is dropped", func() bool { return h.ty.Queued() == 0 })
	idle := func() { inHub(t, h, func() { h.lastKey = time.Time{} }) } // the typing window is over: only the line speaks
	idle()
	if err := leaseNow(t, h, p, 1); !errors.Is(err, ErrTyping) {
		t.Fatalf("lease over a half-typed line = %v, want ErrTyping", err)
	}
	win.Urgent([]byte{0x03}) // the user clears the line
	eventually(t, "ctrl+c is out", func() bool { return f.got() == "a\x03" })
	idle()
	if err := leaseNow(t, h, p, 2); err != nil {
		t.Errorf("lease over a clean line: %v", err)
	}
	_ = ai
}

// An AI tool can't toggle to the BMC prompt over a window's queued typing: the
// toggle would drop it.
func TestAgentToggleIsRefusedWhileWindowInputIsQueued(t *testing.T) {
	env(t)
	f := newFake()
	f.toggle = true
	win, err := Open(context.Background(), Options{Host: "node01", NoLinger: true, Open: slowOpen(f)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { win.Close() })
	h := win.holder.hub
	ai := agent(t)
	win.Enqueue([]byte("abc")) // "a" goes out, "bc" waits
	eventually(t, "the first byte is out", func() bool { return f.got() == "a" })
	inHub(t, h, func() { h.lastKey, h.userTyped = time.Time{}, 0 }) // only the queue speaks
	if _, _, err := ai.Toggle(); !errors.Is(err, ErrTyping) {
		t.Fatalf("agent toggle over queued input = %v, want ErrTyping", err)
	}
	if q := h.ty.Queued(); q != 2 {
		t.Errorf("%d bytes queued after the refused toggle, want the window's 2 kept", q)
	}
	f.mu.Lock()
	atBMC := f.atBMC
	f.mu.Unlock()
	if atBMC {
		t.Error("the console was toggled anyway")
	}
	win.Clear()
	eventually(t, "the queue is dropped", func() bool { return h.ty.Queued() == 0 })
	inHub(t, h, func() { h.lastKey, h.userTyped = time.Time{}, 0 })
	if _, at, err := ai.Toggle(); err != nil || !at {
		t.Errorf("toggle over an empty queue = %v, %v", at, err)
	}
}

// An escape sequence the typer writes in pieces is one unit for the line's
// state, as it is when it is written whole.
func TestUserLineTrackingSpansWrites(t *testing.T) {
	h := &hub{}
	h.trackUser([]byte("\x1b"))
	h.trackUser([]byte("["))
	h.trackUser([]byte("1;5"))
	h.trackUser([]byte("A"))
	if h.userTyped != 0 {
		t.Fatalf("a split arrow key counted as %d typed characters", h.userTyped)
	}
	h.trackUser([]byte("ab"))
	h.trackUser([]byte{0x7f})
	if h.userTyped != 1 {
		t.Errorf("userTyped = %d, want 1", h.userTyped)
	}
	h.trackUser([]byte("\r"))
	if h.userTyped != 0 {
		t.Errorf("userTyped = %d after Enter", h.userTyped)
	}
}

// A byte already inside a blocked Send when the user pauses the AI tool still
// goes out when the link frees. It stays the AI tool's input: the hub counts
// it, so the AI tool's line is known to hold it.
func TestPausingKeepsTheOwnerOfAByteInsideABlockedSend(t *testing.T) {
	env(t)
	f := newFake()
	win, err := Open(context.Background(), Options{Host: "node01", NoLinger: true, Open: slowOpen(f)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { win.Close() })
	h := win.holder.hub
	ai := agent(t)
	ctx, _, release, err := ai.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	gate := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	f.set(func() {
		f.onSend = func([]byte) {
			once.Do(func() { close(entered); <-gate })
		}
	})
	go func() { _ = ai.Type(ctx, []byte("abc")) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the first byte never reached Send")
	}
	if err := win.Window().TogglePause(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pause took effect", func() bool { return ai.Status().Paused })
	close(gate)
	eventually(t, "the byte that was in flight is counted as the AI tool's input", func() bool {
		var chars int
		h.query(func() { chars = h.agentLine.chars })
		return chars == 1
	})
}

// A window's link doesn't map input (the hub does), but it says whether the
// hub's link does, so a paste in the window is stripped as a local one is.
func TestWindowLinkSaysWhetherPastesAreStripped(t *testing.T) {
	env(t)
	f := newFake() // maps input like an iLO VSP
	win := host(t, f, Options{NoLinger: true})
	l := win.Transport().Link
	if _, ok := l.(console.InputMapper); ok {
		t.Error("the window's link maps input too: it would be mapped twice")
	}
	ps, ok := l.(console.PasteStripper)
	if !ok || !ps.StripsPaste() {
		t.Errorf("the window's link doesn't strip pastes (%v) though the hub's link maps input", ok)
	}
}
