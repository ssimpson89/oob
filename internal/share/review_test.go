// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.

//go:build unix

package share

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/console"
)

func hubOf(t *testing.T, c *Client) *hub {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.holder == nil {
		t.Fatal("not the host")
	}
	return c.holder.hub
}

// agentPeer is the hub's AI tool client, once it has joined.
func agentPeer(t *testing.T, h *hub) *peer {
	t.Helper()
	var p *peer
	eventually(t, "the agent joined", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		p = h.agent
		return p != nil
	})
	return p
}

func (f *fakeConsole) set(fn func()) { f.mu.Lock(); fn(); f.mu.Unlock() }

// A write that fails is not typed: the hub's count of what went out doesn't
// move, and the AI tool's write is told why, not that it succeeded.
func TestAFailedWriteIsNotCountedAsTypedAndItsErrorIsKnownToTheWriter(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	h := hubOf(t, win)
	ai := agent(t)
	ctx, _, release, err := ai.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	boom := errors.New("link down")
	f.set(func() { f.sendErr = boom })
	err = ai.Type(ctx, []byte("ls\r"))
	if err == nil || !strings.Contains(err.Error(), "link down") {
		t.Fatalf("Type over a failing link = %v, want the link's error", err)
	}
	if n := h.wrote.Load(); n != 0 {
		t.Errorf("hub counted %d bytes as typed after a failed write", n)
	}
	if h.lineDirty.Load() {
		t.Error("a failed write marked the line dirty")
	}
	h.mu.Lock()
	got := h.txErr
	h.mu.Unlock()
	if got == nil {
		t.Error("txErr not set")
	}
}

// What the AI tool types at a password prompt never reaches the windows, even
// when the console has already moved on to its next prompt by the time the
// typing is accounted for.
func TestAPasswordIsNeverMarkedEvenIfTheConsoleMovesOnFirst(t *testing.T) {
	env(t)
	f := newFake()
	f.set(func() {
		f.onSend = func(b []byte) {
			if strings.Contains(string(b), "\r") {
				f.emit("\r\n[root@node01 ~]# ") // the answer lands before the hub has finished with the write
				time.Sleep(100 * time.Millisecond)
			}
		}
	})
	win := host(t, f, Options{NoLinger: true})
	f.emit("node01 login: ")
	readUntil(t, win, "login: ")
	ai := agent(t)
	ctx, _, release, err := ai.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := ai.Type(ctx, []byte("hunter2\r")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	var marks []string
	for len(marks) == 0 {
		select {
		case n := <-win.Notes():
			if n.Mark {
				marks = append(marks, n.Text)
			}
		case <-deadline:
			t.Fatal("no mark for the AI tool's command")
		}
	}
	for _, m := range marks {
		if strings.Contains(m, "hunter2") {
			t.Fatalf("the password was logged: %q", m)
		}
		if !strings.Contains(m, "answered a login prompt") {
			t.Errorf("mark %q, want the redacted form", m)
		}
	}
}

// A lease request that is decided after its client has left is refused, and
// leaves the hub with no keyboard held and no AI tool recorded.
func TestALeaseForAClientThatHasLeftIsRefused(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	h := hubOf(t, win)
	ghost := &peer{agent: true, name: "gone"}
	if err := h.agentOp(ghost, msg{Op: opLease, Lease: 1}); !errors.Is(err, errGone) {
		t.Fatalf("lease for a peer that isn't registered = %v, want errGone", err)
	}
	h.mu.Lock()
	leased := h.lease
	h.mu.Unlock()
	if leased {
		t.Fatal("the hub holds the keyboard for a client that isn't there")
	}
	// And a window that is typing meanwhile doesn't hit a nil agent.
	win.Enqueue([]byte("ls\r"))
	eventually(t, "the window types", func() bool { return f.got() == "ls\r" })

	// Also once the hub is closed.
	ai := agent(t)
	p := agentPeer(t, h)
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	if err := h.agentOp(p, msg{Op: opLease, Lease: 2}); err == nil {
		t.Error("a lease was granted on a closed hub")
	}
	_ = ai
}

// A lease id is the client's own: one that left, with a release recorded
// ahead of its request, must not refuse the next client's request with the
// same number, and its record goes with it.
func TestCancelledLeaseIDsAreKeptPerClientAndForgottenWhenItLeaves(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	h := hubOf(t, win)
	w := rawAgent(t)
	_ = w.send(msg{T: tReq, ID: 1, Op: opRelease, Lease: 7})
	reply(t, w, 1)
	h.mu.Lock()
	n := len(h.cancelled)
	h.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d cancelled ids recorded, want 1", n)
	}
	// Its own request for 7 is refused.
	_ = w.send(msg{T: tReq, ID: 2, Op: opLease, Lease: 7})
	if r := reply(t, w, 2); r.Err == "" {
		t.Fatal("its cancelled request was granted")
	}
	// It leaves a cancelled id behind, and goes.
	_ = w.send(msg{T: tReq, ID: 3, Op: opRelease, Lease: 8})
	reply(t, w, 3)
	_ = w.w.(io.Closer).Close()
	eventually(t, "the hub forgot the client", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.agent == nil && len(h.cancelled) == 0
	})
	// The next client's lease 8 is its own.
	w2 := rawAgent(t)
	_ = w2.send(msg{T: tReq, ID: 1, Op: opLease, Lease: 8})
	if r := reply(t, w2, 1); r.Err != "" {
		t.Errorf("a new client's lease 8 was refused: %s", r.Err)
	}
}

// The keyboard isn't given to the AI tool while the typer still holds input
// that was queued ahead of it.
func TestLeaseIsRefusedWhileTheTyperHasPendingInput(t *testing.T) {
	env(t)
	f := newFake()
	f.delay = 100 * time.Millisecond
	win := host(t, f, Options{NoLinger: true})
	h := hubOf(t, win)
	ai := agent(t)
	p := agentPeer(t, h)
	win.Enqueue([]byte("a\r"))
	win.Enqueue([]byte("b\r"))
	eventually(t, "queued", func() bool { return h.ty.Queued() > 0 })
	h.mu.Lock()
	h.lastKey, h.userTyped = time.Time{}, 0 // the typing window is over: only the queue speaks
	h.mu.Unlock()
	if err := h.agentOp(p, msg{Op: opLease, Lease: 5}); !errors.Is(err, ErrTyping) {
		t.Fatalf("lease with input pending = %v, want ErrTyping", err)
	}
	eventually(t, "typed", func() bool { return h.ty.Queued() == 0 })
	if err := h.agentOp(p, msg{Op: opLease, Lease: 6}); err != nil {
		t.Errorf("lease once the queue is empty: %v", err)
	}
	_ = ai
}

// A window's keys during a BREAK or toggle are not typed.
func TestWindowKeysAreDroppedWhileABreakRuns(t *testing.T) {
	env(t)
	f := newFake()
	f.breakGate, f.inBreak = make(chan struct{}), make(chan struct{})
	win := host(t, f, Options{NoLinger: true})
	win2, err := join(t, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() { res <- win.Transport().Link.Break() }()
	select {
	case <-f.inBreak:
	case <-time.After(3 * time.Second):
		t.Fatal("BREAK never started")
	}
	win2.Enqueue([]byte("x"))
	var note string
	for note == "" {
		select {
		case n := <-win2.Notes():
			if strings.Contains(n.Text, "busy") {
				note = n.Text
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the window wasn't told its keys were dropped")
		}
	}
	close(f.breakGate)
	if err := <-res; err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := f.got(); got != "" {
		t.Errorf("typed %q during a BREAK", got)
	}
	// And typing works again afterwards.
	win2.Enqueue([]byte("y"))
	eventually(t, "typing resumed", func() bool { return f.got() == "y" })
}

// While a paused AI tool's text is being erased, a window's keys wait out: they
// can't land in the middle of the backspaces, and the keyboard comes back after.
func TestWindowKeysWaitOutTheErasePausingStarts(t *testing.T) {
	env(t)
	f := newFake()
	f.delay = 60 * time.Millisecond
	win := host(t, f, Options{NoLinger: true})
	win2, err := join(t, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ai := agent(t)
	ctx, _, release, err := ai.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := ai.Type(ctx, []byte("rm me")); err != nil {
		t.Fatal(err)
	}
	paused := make(chan error, 1)
	go func() { paused <- win.Window().TogglePause() }()
	eventually(t, "the erase has begun", func() bool { return strings.HasPrefix(f.got(), "rm me\b") })
	win2.Enqueue([]byte("Z")) // mid-erase
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := f.got(); got != "rm me\b\b\b\b\b" && got != "rm me\b\b\b\b\bZ" {
		t.Errorf("typed %q, want the erase whole, with the window's key dropped or after it, not inside it", got)
	}
	// Erase done and published: the keyboard is the windows' again.
	win2.Enqueue([]byte("Y"))
	eventually(t, "the window types again", func() bool { return strings.HasSuffix(f.got(), "Y") })
}

// A window's key with no mapping in force is typed as it is, at once: nothing
// is held back to be dropped.
func TestWindowFragmentsAreTypedAtOnceWhenTheLinkIsNotMapping(t *testing.T) {
	env(t)
	f := newFake() // MapInput passes everything through
	win := host(t, f, Options{NoLinger: true})
	win.Enqueue([]byte("\x1b[1;5"))
	eventually(t, "typed unchanged", func() bool { return f.got() == "\x1b[1;5" })
	time.Sleep(fragWait + 100*time.Millisecond)
	if got := f.got(); got != "\x1b[1;5" {
		t.Errorf("typed %q", got)
	}
}

func lockHeld(t *testing.T) bool {
	t.Helper()
	_, path, ok := paths("node01", "")
	if !ok {
		t.Fatal("no lock path")
	}
	f, err := lock(path)
	if err == nil {
		unlock(f)
		return false
	}
	if !errors.Is(err, errHeld) {
		t.Fatal(err)
	}
	return true
}

// The ownership lock is kept until the BMC-side release is done: let go first,
// another oob could open the console and have the release take its session.
func TestTheOwnershipLockIsHeldUntilTheBMCReleaseFinishes(t *testing.T) {
	t.Run("a lingering hub's last window leaves", func(t *testing.T) {
		env(t)
		f := newFake()
		f.releaseGate = make(chan struct{})
		owner := host(t, f, Options{Agent: true, Name: "Claude"})
		win, err := join(t, Options{})
		if err != nil {
			t.Fatal(err)
		}
		owner.Close()
		win.Close()
		eventually(t, "the transport closed", f.isClosed)
		time.Sleep(100 * time.Millisecond) // the release is blocked at the BMC
		if !lockHeld(t) {
			t.Fatal("the lock was let go before the release finished")
		}
		close(f.releaseGate)
		eventually(t, "the lock was let go", func() bool { return !lockHeld(t) })
	})
	t.Run("the host closes and releases", func(t *testing.T) {
		env(t)
		f := newFake()
		f.releaseGate = make(chan struct{})
		c, err := Open(context.Background(), Options{Host: "node01", NoLinger: true, Open: func() (*console.Transport, error) { return f.transport(), nil }})
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		done := make(chan struct{})
		go func() { c.Release(); close(done) }()
		time.Sleep(100 * time.Millisecond)
		if !lockHeld(t) {
			t.Fatal("the lock was let go before the release finished")
		}
		close(f.releaseGate)
		<-done
		if lockHeld(t) {
			t.Error("the lock is still held after the release")
		}
	})
	t.Run("a release that never finishes is bounded", func(t *testing.T) {
		env(t)
		old := releaseWait.Swap(int64(100 * time.Millisecond))
		defer releaseWait.Store(old)
		f := newFake()
		f.releaseGate = make(chan struct{})
		defer close(f.releaseGate)
		c, err := Open(context.Background(), Options{Host: "node01", NoLinger: true, Open: func() (*console.Transport, error) { return f.transport(), nil }})
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		c.Release()
		if lockHeld(t) {
			t.Error("the lock is held after the bounded release")
		}
	})
}

// Exclusive is the ownership lock: another oob that has the console, or
// another Exclusive, keeps it out, and the lock is the one a hosting oob takes.
func TestExclusiveTakesTheOwnershipLock(t *testing.T) {
	env(t)
	rel, err := Exclusive("node01", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Exclusive("node01", ""); !errors.Is(err, ErrShared) {
		t.Errorf("second Exclusive = %v, want ErrShared", err)
	}
	if !lockHeld(t) {
		t.Error("the lock isn't held")
	}
	rel()
	rel() // twice is fine
	if lockHeld(t) {
		t.Error("the lock is held after release")
	}

	// A console another oob hosts keeps Exclusive out.
	f := newFake()
	host(t, f, Options{NoLinger: true})
	if _, err := Exclusive("node01", ""); !errors.Is(err, ErrShared) {
		t.Errorf("Exclusive on a hosted console = %v, want ErrShared", err)
	}
}

// An operation that took the lock and then hosts the console (sol --force)
// hosts under that lock, with no gap another oob could use.
func TestExclusiveIsHandedToTheHubThatHostsUnderIt(t *testing.T) {
	env(t)
	rel, err := Exclusive("node01", "")
	if err != nil {
		t.Fatal(err)
	}
	f := newFake()
	c, err := Open(context.Background(), Options{Host: "node01", NoLinger: true, Open: func() (*console.Transport, error) { return f.transport(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Hosting() {
		t.Fatal("not hosting under the lock it was handed")
	}
	rel() // nothing left to release: the hub has it
	if !lockHeld(t) {
		t.Error("the hub's lock was let go by the operation's release")
	}
	c.Close()
}
