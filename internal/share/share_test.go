// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// Adapted from bauddy (internal/share/share_test.go): the same behaviors,
// against a fake console transport.

//go:build unix

package share

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/console"
)

// fakeConsole is a server console behind a console.Transport. What is typed at
// it is recorded; output is whatever the test emits.
type fakeConsole struct {
	pr   *io.PipeReader
	pw   *io.PipeWriter
	done chan console.Result

	mu       sync.Mutex
	sent     []byte
	closed   bool
	released bool
	inSend   atomic.Int32
	overlap  atomic.Bool
	delay    time.Duration
	breaks   int
	atBMC    bool
	toggle   bool
	vsp      bool // maps input like an iLO VSP
}

func newFake() *fakeConsole {
	f := &fakeConsole{done: make(chan console.Result, 1)}
	f.pr, f.pw = io.Pipe()
	return f
}

func (f *fakeConsole) emit(s string) { _, _ = f.pw.Write([]byte(s)) }

func (f *fakeConsole) Send(b []byte) error {
	if f.inSend.Add(1) > 1 {
		f.overlap.Store(true)
	}
	defer f.inSend.Add(-1)
	time.Sleep(f.delay)
	f.mu.Lock()
	f.sent = append(f.sent, b...)
	f.mu.Unlock()
	return nil
}
func (f *fakeConsole) Break() error                     { f.mu.Lock(); f.breaks++; f.mu.Unlock(); return nil }
func (f *fakeConsole) Disconnect() error                { f.done <- console.Result{}; f.pw.Close(); return nil }
func (f *fakeConsole) Resize(rows, cols int) error      { return nil }
func (f *fakeConsole) FitSize(rows, cols int) error     { return nil }
func (f *fakeConsole) SizeIsFree() bool                 { return false }
func (f *fakeConsole) Serial() bool                     { return true }
func (f *fakeConsole) SizeLine(r, c int) (string, bool) { return "stty", true }

func (f *fakeConsole) MapInput(b []byte) ([]byte, string) {
	if !f.vsp {
		return b, ""
	}
	return console.MapVSPInput(b)
}

func (f *fakeConsole) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.sent)
}

func (f *fakeConsole) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeConsole) transport() *console.Transport {
	zero := time.Duration(0)
	t := &console.Transport{
		Kind: "sol", Link: f, Out: f.pr, Done: f.done, Pacing: console.Pacing{Char: &zero, Line: &zero},
		Close: func() {
			f.mu.Lock()
			f.closed = true
			f.mu.Unlock()
			f.pw.Close()
			select {
			case f.done <- console.Result{}:
			default:
			}
		},
		Release: func() { f.mu.Lock(); f.released = true; f.mu.Unlock() },
	}
	if f.toggle {
		t.Toggle = func() (string, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.atBMC = !f.atBMC
			if f.atBMC {
				return "ilo cli", true, nil
			}
			return "vsp", false, nil
		}
		t.Observe = func([]byte) (string, bool, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.atBMC {
				return "ilo cli", true, false
			}
			return "vsp", false, false
		}
	}
	return t
}

// env points the share folder at a short private one (socket paths are limited
// to about 100 bytes) and shortens the typing window.
func env(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "oob")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", d)
	old := typingWindow
	typingWindow = 150 * time.Millisecond
	t.Cleanup(func() { typingWindow = old; os.RemoveAll(d) })
	return d
}

func host(t *testing.T, f *fakeConsole, opt Options) *Client {
	t.Helper()
	opt.Host = cmpHost(opt.Host)
	opt.Open = func() (*console.Transport, error) { return f.transport(), nil }
	c, err := Open(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func cmpHost(h string) string {
	if h == "" {
		return "node01"
	}
	return h
}

func join(t *testing.T, opt Options) (*Client, error) {
	t.Helper()
	opt.Host = cmpHost(opt.Host)
	c, err := Open(context.Background(), opt)
	if err == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

func agent(t *testing.T) *Client {
	t.Helper()
	c, err := join(t, Options{Agent: true, Name: "Claude"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// taps accumulate each client's output, read by one goroutine per client.
var taps sync.Map // *Client -> *tap

type tap struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (p *tap) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

func tapOf(c *Client) *tap {
	if v, ok := taps.Load(c); ok {
		return v.(*tap)
	}
	p := &tap{}
	taps.Store(c, p)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := c.out.Read(buf)
			p.mu.Lock()
			if p.buf.Len() < 1<<20 { // a flood only needs its end to be seen
				p.buf.Write(buf[:n])
			} else {
				p.buf.Reset()
				p.buf.Write(buf[:n])
			}
			p.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return p
}

// readUntil waits until the output c has received holds want, and returns it.
func readUntil(t *testing.T, c *Client, want string) string {
	t.Helper()
	p := tapOf(c)
	for i := 0; i < 500; i++ {
		if got := p.text(); strings.Contains(got, want) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output %q never contained %q", p.text(), want)
	return ""
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

func TestWindowAndAgentShareOneConsole(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	f.emit("boot line 1\r\nboot line 2\r\nlogin: ")
	readUntil(t, win, "login: ")

	ai := agent(t)
	if !ai.Joined() || win.Joined() {
		t.Errorf("joined: ai %v window %v", ai.Joined(), win.Joined())
	}
	// The AI gets only the line the cursor is on.
	if got := readUntil(t, ai, "login: "); strings.Contains(got, "boot line") {
		t.Errorf("the AI saw earlier output: %q", got)
	}
	// Output reaches both.
	f.emit("\r\nWelcome\r\n")
	readUntil(t, win, "Welcome")
	readUntil(t, ai, "Welcome")
	// A window joining later gets the recent output from a line start.
	win2, err := join(t, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := readUntil(t, win2, "Welcome"); !strings.Contains(got, "boot line 1") {
		t.Errorf("the second window's replay: %q", got)
	}
	eventually(t, "the bar knows the AI", func() bool { return win.Window().Status().Agent == "Claude" })
	if s := win.Window().Status(); s.Windows != 1 {
		t.Errorf("other windows = %d, want 1", s.Windows)
	}
}

func TestInputGoesThroughOneSerializedTyper(t *testing.T) {
	env(t)
	f := newFake()
	f.delay = time.Millisecond
	win := host(t, f, Options{NoLinger: true})
	win2, err := join(t, Options{})
	if err != nil {
		t.Fatal(err)
	}
	win.Enqueue([]byte("one-"))
	win2.Enqueue([]byte("two-"))
	win.Enqueue([]byte("three"))
	eventually(t, "all typed", func() bool { return len(f.got()) == len("one-two-three") })
	if f.overlap.Load() {
		t.Error("two writes reached the link at once")
	}
	got := f.got()
	for _, w := range []string{"one-", "two-", "three"} {
		if !strings.Contains(got, w) {
			t.Errorf("%q missing from %q: a client's bytes were split or lost", w, got)
		}
	}
	if strings.Index(got, "one-") > strings.Index(got, "three") {
		t.Errorf("one window's input was reordered: %q", got)
	}
}

func TestAgentTypesThroughTheLease(t *testing.T) {
	env(t)
	f := newFake()
	host(t, f, Options{NoLinger: true})
	ai := agent(t)
	if err := ai.Type(context.Background(), []byte("x")); !errors.Is(err, ErrStopped) {
		t.Errorf("a write without the keyboard: %v", err)
	}
	ctx, _, release, err := ai.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := ai.Type(ctx, []byte("uname -r\r")); err != nil {
		t.Fatal(err)
	}
	if got := f.got(); got != "uname -r\r" {
		t.Errorf("typed %q", got)
	}
	release()
}

func TestPauseBlocksTheAgentWithAClearError(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	ai := agent(t)
	if err := win.Window().TogglePause(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "state", func() bool { return ai.Status().Paused })
	if _, _, _, err := ai.Lease(context.Background()); !errors.Is(err, ErrPaused) || !strings.Contains(err.Error(), "the user paused the AI") {
		t.Errorf("lease while paused: %v", err)
	}
	if err := ai.Break(context.Background()); !errors.Is(err, ErrPaused) {
		t.Errorf("break while paused: %v", err)
	}
	if err := win.Window().TogglePause(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "resumed", func() bool { return !ai.Status().Paused })
	_, _, release, err := ai.Lease(context.Background())
	if err != nil {
		t.Fatalf("lease after resume: %v", err)
	}
	release()
	if f.got() != "" {
		t.Errorf("something was typed: %q", f.got())
	}
}

func TestPausingWhileWorkingStopsTheAgentAndErasesItsText(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	ai := agent(t)
	ctx, why, release, err := ai.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := ai.Type(ctx, []byte("rm me")); err != nil { // typed, not entered
		t.Fatal(err)
	}
	if err := win.Window().TogglePause(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the lease wasn't ended")
	}
	if got := why(); got != StopCleared {
		t.Errorf("why = %q, want %q", got, StopCleared)
	}
	eventually(t, "erased", func() bool { return f.got() == "rm me\b\b\b\b\b" })
	if err := ai.Type(context.Background(), []byte("more")); err == nil {
		t.Error("the AI typed after it was paused")
	}
}

func TestUserKeysAreDroppedWhileTheAgentWorks(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	ai := agent(t)
	_, _, release, err := ai.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	win.Enqueue([]byte("typed by the user"))
	var note string
	for note == "" {
		select {
		case n := <-win.Notes():
			if strings.Contains(n.Text, "pauses it") {
				note = n.Text
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the window wasn't told its keys were dropped")
		}
	}
	release()
	time.Sleep(50 * time.Millisecond)
	if f.got() != "" {
		t.Errorf("user keys reached the console while the AI worked: %q", f.got())
	}
}

func TestAgentWaitsWhileTheUserIsMidLine(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	ai := agent(t)
	win.Enqueue([]byte("ls -l")) // typed, not entered
	eventually(t, "typed", func() bool { return f.got() == "ls -l" })
	_, _, _, err := ai.Lease(context.Background())
	if !errors.Is(err, ErrTyping) || !strings.Contains(err.Error(), "the user is typing on this console") {
		t.Fatalf("lease mid-line: %v", err)
	}
	win.Enqueue([]byte("\r"))
	eventually(t, "enter", func() bool { return f.got() == "ls -l\r" })
	// Once the line is entered and the typing window passes, the AI may go on.
	time.Sleep(typingWindow + 50*time.Millisecond)
	_, _, release, err := ai.Lease(context.Background())
	if err != nil {
		t.Fatalf("lease after Enter: %v", err)
	}
	release()
}

func TestSecondAgentIsRejected(t *testing.T) {
	env(t)
	f := newFake()
	host(t, f, Options{NoLinger: true})
	agent(t)
	_, err := join(t, Options{Agent: true, Name: "Codex"})
	if err == nil || !strings.Contains(err.Error(), "another AI tool") {
		t.Errorf("second AI: %v", err)
	}
}

func TestWindowDetachLeavesTheConsoleForTheOther(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	other, err := join(t, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// ~. in the joined window only leaves.
	if err := other.Transport().Link.Disconnect(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-other.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the window didn't end")
	}
	if f.isClosed() {
		t.Fatal("the console closed when a joined window left")
	}
	f.emit("still here")
	readUntil(t, win, "still here")
	eventually(t, "window count", func() bool { return win.Window().Status().Windows == 0 })
}

func TestOwnerExitEndsTheConsoleForTheOthers(t *testing.T) {
	env(t)
	f := newFake()
	owner := host(t, f, Options{NoLinger: true}) // oob sol: the process is about to exit
	other, err := join(t, Options{})
	if err != nil {
		t.Fatal(err)
	}
	owner.Close()
	select {
	case res := <-other.Done():
		if res.Err == nil {
			t.Error("the joined window wasn't told why")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the joined window never learned the console closed")
	}
	if !f.isClosed() {
		t.Error("the transport wasn't closed")
	}
}

func TestMCPHostKeepsTheConsoleForAWindow(t *testing.T) {
	env(t)
	f := newFake()
	owner := host(t, f, Options{Agent: true, Name: "Claude"}) // oob mcp opened it first
	win, err := join(t, Options{})
	if err != nil {
		t.Fatal(err)
	}
	owner.Close() // the AI disconnects, or goes idle
	if f.isClosed() {
		t.Fatal("the console closed under the user's window")
	}
	f.emit("user keeps working")
	readUntil(t, win, "user keeps working")
	win.Enqueue([]byte("ok"))
	eventually(t, "typed", func() bool { return f.got() == "ok" })
	// A later AI connect joins the lingering hub.
	again := agent(t)
	if !again.Joined() {
		t.Error("the new AI should have joined")
	}
	again.Close()
	win.Close() // the last window leaves
	eventually(t, "closed", f.isClosed)
	eventually(t, "released", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.released })
}

func TestHostEndingTellsAgent(t *testing.T) {
	env(t)
	f := newFake()
	host(t, f, Options{NoLinger: true})
	ai := agent(t)
	f.done <- console.Result{Code: 3}
	f.pw.Close()
	select {
	case res := <-ai.Done():
		if res.Code != 3 {
			t.Errorf("code = %d", res.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the AI wasn't told the console ended")
	}
}

func TestSocketAndFolderArePrivate(t *testing.T) {
	d := env(t)
	f := newFake()
	host(t, f, Options{NoLinger: true})
	sock := filepath.Join(d, "oob", "share", "node01.sock")
	st, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 || st.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket mode %v", st.Mode())
	}
	ds, _ := os.Stat(filepath.Dir(sock))
	if ds.Mode().Perm() != 0o700 {
		t.Errorf("folder mode %v", ds.Mode().Perm())
	}
	if !Exists("node01", "") || Exists("other", "") {
		t.Error("Exists is wrong")
	}
}

func TestASocketThatIsntOursIsRefused(t *testing.T) {
	d := env(t)
	dir := filepath.Join(d, "oob", "share")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A regular file where the socket should be, with the lock held so we join.
	sock := filepath.Join(dir, "node01.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkSocket(sock); err == nil {
		t.Error("a regular file was accepted as a socket")
	}
	if Exists("node01", "") {
		t.Error("Exists trusted a non-socket")
	}
}

func TestStaleSocketAndLockAreRecovered(t *testing.T) {
	d := env(t)
	dir := filepath.Join(d, "oob", "share")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// What a killed oob leaves: a socket nobody listens on, and an unlocked lock file.
	sock := filepath.Join(dir, "node01.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if err := os.WriteFile(filepath.Join(dir, "node01.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if Exists("node01", "") {
		t.Error("a dead socket counted as a hub")
	}
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	if win.Joined() || !win.Hosting() {
		t.Error("the stale leftovers kept this oob from hosting")
	}
	other, err := join(t, Options{})
	if err != nil || !other.Joined() {
		t.Errorf("join after recovery: %v", err)
	}
}

func TestJoinOnlyWithNoHub(t *testing.T) {
	env(t)
	if _, err := Open(context.Background(), Options{Host: "node01"}); !errors.Is(err, ErrNoHub) {
		t.Errorf("got %v, want ErrNoHub", err)
	}
}

func TestVersionMismatchIsRefused(t *testing.T) {
	env(t)
	f := newFake()
	host(t, f, Options{NoLinger: true})
	conn, err := net.Dial("unix", func() string { s, _, _ := paths("node01", ""); return s }())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	w := newWire(conn)
	_ = w.send(msg{T: tHello, Version: 99})
	m, err := w.recv()
	if err != nil || m.T != tBye {
		t.Errorf("got %+v, %v; want a bye", m, err)
	}
}

func TestToggleAndObserveReachTheAgent(t *testing.T) {
	env(t)
	f := newFake()
	f.toggle = true
	host(t, f, Options{NoLinger: true})
	ai := agent(t)
	tr := ai.Transport()
	if tr.Toggle == nil || tr.Observe == nil {
		t.Fatal("a console with a BMC prompt lost its toggle")
	}
	kind, at, err := tr.Toggle()
	if err != nil || kind != "ilo cli" || !at {
		t.Fatalf("toggle: %q %v %v", kind, at, err)
	}
	eventually(t, "state", func() bool { k, a, _ := tr.Observe(nil); return k == "ilo cli" && a })
}

func TestAFloodOfOutputIsNotDropped(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	line := strings.Repeat("x", 100) + "\r\n"
	go func() {
		for i := 0; i < 50000; i++ { // 5 MB
			f.emit(line)
		}
		f.emit("END")
	}()
	readUntil(t, win, "END")
}

func TestBreakGoesThroughTheHubAndClearsTheQueue(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	if err := win.Transport().Link.Break(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	n := f.breaks
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("breaks = %d", n)
	}
}

func TestVSPInputIsMappedAtTheHub(t *testing.T) {
	env(t)
	f := newFake()
	f.vsp = true
	win := host(t, f, Options{NoLinger: true})
	win2, err := join(t, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A lone ESC is sent as ESC ESC, from either window.
	win.Enqueue([]byte("\x1b"))
	win2.Enqueue([]byte("a"))
	eventually(t, "mapped ESC typed", func() bool { return f.got() == "\x1b\x1ba" })

	// An arrow key is dropped, with one note to the windows; typing around it goes on.
	win.Enqueue([]byte("b\x1b[Ac\x1b[Ad"))
	eventually(t, "typed around the arrow keys", func() bool { return f.got() == "\x1b\x1babcd" })
	notes := 0
	timeout := time.After(300 * time.Millisecond)
loop:
	for {
		select {
		case n := <-win2.Notes():
			if n.Text == console.VSPKeysNote {
				notes++
			}
		case <-timeout:
			break loop
		}
	}
	if notes != 1 {
		t.Errorf("window got the VSP note %d times, want once per session", notes)
	}

	// Focus reports are dropped without a word, and ESC ( is never produced.
	win.Enqueue([]byte("\x1b[I\x1b[O\x1b(e"))
	eventually(t, "e typed", func() bool { return f.got() == "\x1b\x1babcde" })
	if strings.Contains(f.got(), "\x1b(") {
		t.Errorf("ESC ( reached the link: %q", f.got())
	}
}

func TestVSPAgentKeysAreRefusedNotDropped(t *testing.T) {
	env(t)
	f := newFake()
	f.vsp = true
	host(t, f, Options{NoLinger: true})
	ai := agent(t)
	ctx, _, release, err := ai.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := ai.Type(ctx, []byte("ls\x1b[A")); !errors.Is(err, console.ErrKeysDropped) {
		t.Errorf("arrow key on a VSP: err = %v, want ErrKeysDropped", err)
	}
	if got := f.got(); got != "" {
		t.Errorf("a refused write typed %q", got)
	}
	if err := ai.Type(ctx, []byte("\x1b")); err != nil {
		t.Fatal(err)
	}
	if got := f.got(); got != "\x1b\x1b" {
		t.Errorf("lone ESC from the AI tool typed %q, want ESC ESC", got)
	}
}

func TestNonVSPInputIsUnchangedAtTheHub(t *testing.T) {
	env(t)
	f := newFake()
	win := host(t, f, Options{NoLinger: true})
	win.Enqueue([]byte("\x1b\x1b[Ax"))
	eventually(t, "typed", func() bool { return f.got() == "\x1b\x1b[Ax" })
}

func TestTheSameNameAtAnotherAddressIsAnotherConsole(t *testing.T) {
	env(t)
	f1, f2 := newFake(), newFake()
	a := host(t, f1, Options{Addr: "192.0.2.1", Type: "ilo", NoLinger: true})
	// The config now points node01 at another BMC. It must not join the first
	// one's console and type at the wrong server: it opens its own.
	b := host(t, f2, Options{Addr: "192.0.2.2", Type: "ilo", NoLinger: true})
	if b.Joined() || !b.Hosting() || !a.Hosting() {
		t.Fatalf("joined/hosting: a %v/%v b %v/%v", a.Joined(), a.Hosting(), b.Joined(), b.Hosting())
	}
	b.Enqueue([]byte("on-b"))
	eventually(t, "b types at its own console", func() bool { return f2.got() == "on-b" })
	if f1.got() != "" {
		t.Errorf("the first console got %q from a window for another address", f1.got())
	}
	// Same name and address joins as before.
	w, err := join(t, Options{Addr: "192.0.2.1", Type: "ilo"})
	if err != nil || !w.Joined() {
		t.Fatalf("same address: joined %v, %v", w != nil && w.Joined(), err)
	}
	if Exists("node01", "192.0.2.3") || !Exists("node01", "192.0.2.1") {
		t.Error("Exists must look at the address too")
	}
	if _, err := join(t, Options{Addr: "192.0.2.3"}); !errors.Is(err, ErrNoHub) {
		t.Errorf("a join-only open for a third address: %v, want ErrNoHub", err)
	}
}

func TestJoinWithADifferentAddressOrTypeIsRefusedAtHello(t *testing.T) {
	env(t)
	f := newFake()
	host(t, f, Options{Addr: "192.0.2.1", Type: "ilo", NoLinger: true})
	sock, _, _ := paths("node01", "192.0.2.1")
	for _, c := range []struct {
		name string
		hi   msg
		want string
	}{
		{"address", msg{T: tHello, Version: version, Addr: "192.0.2.9", Type: "ilo"}, "different BMC address"},
		{"no address", msg{T: tHello, Version: version, Type: "ilo"}, "different BMC address"},
		{"type", msg{T: tHello, Version: version, Addr: "192.0.2.1", Type: "idrac"}, "ilo host"},
	} {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		w := newWire(conn)
		_ = w.send(c.hi)
		m, err := w.recv()
		conn.Close()
		if err != nil || m.T != tBye || !strings.Contains(m.Text, c.want) {
			t.Errorf("%s: got %+v, %v; want a bye mentioning %q", c.name, m, err, c.want)
		}
	}
	// An unknown type on either side still joins.
	w, err := join(t, Options{Addr: "192.0.2.1"})
	if err != nil || !w.Joined() {
		t.Errorf("a joiner with no type: %v", err)
	}
}
