package console

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/term"
)

// All the timing here is virtual: the typing queue's Wait is replaced by one
// that advances a clock and returns at once, so nothing sleeps.

// serialDev is a fake serial console behind a Link. Like a real BMC it has a
// small input FIFO that drains slowly, and while it runs a line it takes
// nothing; bytes arriving into a full FIFO are lost. It keeps the line being
// typed and the lines it ran.
type serialDev struct {
	mu      sync.Mutex
	now     time.Duration // virtual time, advanced by wait
	free    time.Duration // when the device can take its next byte
	fifo    []byte
	cap     int
	drain   time.Duration // time to take one byte out of the FIFO
	busy    time.Duration // time to run a line after Enter
	dropped int
	sends   int
	sent    []byte
	buf     string
	applied []string
	fits    [][2]int
	serial  bool
	size    bool // has a size line (stty), not a window change
	note    chan struct{}
}

func newSerialDev() *serialDev {
	return &serialDev{cap: 16, drain: 8 * time.Millisecond, busy: 150 * time.Millisecond, serial: true, size: true, note: make(chan struct{}, 1)}
}

// advance lets the device take what it can by time to.
func (d *serialDev) advance(to time.Duration) {
	for len(d.fifo) > 0 && d.free <= to {
		c := d.fifo[0]
		d.fifo = d.fifo[1:]
		d.free += d.drain
		if c == '\r' {
			d.applied = append(d.applied, d.buf)
			d.buf = ""
			d.free += d.busy
		} else {
			d.buf += string(rune(c))
		}
	}
}

func (d *serialDev) Send(b []byte) error {
	d.mu.Lock()
	d.advance(d.now)
	if len(d.fifo) == 0 && d.free < d.now {
		d.free = d.now
	}
	d.sends++
	d.sent = append(d.sent, b...)
	for _, c := range b {
		if len(d.fifo) >= d.cap {
			d.dropped++
			continue
		}
		d.fifo = append(d.fifo, c)
	}
	d.mu.Unlock()
	select {
	case d.note <- struct{}{}:
	default:
	}
	return nil
}

func (d *serialDev) wait(dd time.Duration) <-chan time.Time {
	d.mu.Lock()
	d.now += dd
	d.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

// waitSent blocks until n bytes have been sent, then lets the device finish.
func (d *serialDev) waitSent(t *testing.T, n int) {
	t.Helper()
	for {
		d.mu.Lock()
		got := len(d.sent)
		d.mu.Unlock()
		if got >= n {
			break
		}
		select {
		case <-d.note:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d bytes were sent", got, n)
		}
	}
	d.mu.Lock()
	d.advance(d.now + time.Hour)
	d.mu.Unlock()
}

func (d *serialDev) lines() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.applied...)
}

func (d *serialDev) Break() error          { return nil }
func (d *serialDev) Disconnect() error     { return nil }
func (d *serialDev) Resize(r, c int) error { return nil }
func (d *serialDev) FitSize(r, c int) error {
	d.mu.Lock()
	d.fits = append(d.fits, [2]int{r, c})
	d.mu.Unlock()
	return nil
}
func (d *serialDev) SizeIsFree() bool { return false }
func (d *serialDev) Serial() bool     { return d.serial }
func (d *serialDev) SizeLine(r, c int) (string, bool) {
	return fmt.Sprintf("stty rows %d cols %d", r, c), d.size
}

// plainLink is a link that is not serial and has no size line: a plain ssh
// session. It records what it was sent and tells when something arrives.
type plainLink struct {
	fakeLink
	sends int
	note  chan struct{}
}

func (p *plainLink) Send(b []byte) error {
	p.mu.Lock()
	p.sends++
	p.mu.Unlock()
	err := p.fakeLink.Send(b)
	if p.note != nil {
		select {
		case p.note <- struct{}{}:
		default:
		}
	}
	return err
}

func (p *plainLink) waitSent(t *testing.T, n int) {
	t.Helper()
	for {
		p.mu.Lock()
		got := len(p.sent)
		p.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-p.note:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d bytes were sent", got, n)
		}
	}
}

// outLocked makes the console's output buffer safe to read while the console
// writes it.
type outLocked struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (o *outLocked) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.w.Write(b)
}

// rig is a console and key handler on a link, with the typing queue running.
type rig struct {
	k   *consoleKeys
	con *console
}

func newRig(t *testing.T, l Link, p Pacing, wait func(time.Duration) <-chan time.Time) *rig {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	con := newConsole(&outLocked{w: &out}, "node01", "sol", 24, 80)
	con.Start()
	stop := make(chan struct{})
	k := newKeys(&Setup{Link: l, Pacing: p}, con, func() {}, stop)
	if wait != nil {
		k.ty.Wait = wait
	}
	k.start(nil)
	t.Cleanup(func() { close(stop); con.Close() })
	return &rig{k, con}
}

func (r *rig) paste(text string) { r.k.Input([]byte(pasteStart + text + pasteEnd)) }

func (r *rig) state() barState {
	r.con.mu.Lock()
	defer r.con.mu.Unlock()
	return r.con.st
}

var pasteLines = []string{
	"interface bond0 description uplink-to-switch-a",
	"ip link set dev bond0 mtu 9000 up",
	"echo configured the second interface fully",
	"systemctl restart NetworkManager-wait-online",
	"ls",
	"cat /etc/os-release | grep -i pretty_name",
}

func TestPasteArrivesIntactOnASerialDeviceAtTheDefaults(t *testing.T) {
	d := newSerialDev()
	r := newRig(t, d, Pacing{}, d.wait)
	text := strings.Join(pasteLines, "\n") + "\n"
	r.paste(text)
	d.waitSent(t, len(text))
	if got := d.lines(); strings.Join(got, "|") != strings.Join(pasteLines, "|") {
		t.Errorf("device ran:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(pasteLines, "\n  "))
	}
	if d.dropped != 0 {
		t.Errorf("dropped %d bytes", d.dropped)
	}
}

func TestTheSameDeviceLosesAnUnpacedPaste(t *testing.T) {
	zero := time.Duration(0)
	d := newSerialDev()
	r := newRig(t, d, Pacing{Char: &zero, Line: &zero}, d.wait)
	text := strings.Join(pasteLines, "\n") + "\n"
	r.paste(text)
	d.waitSent(t, len(text))
	if d.dropped == 0 {
		t.Error("an unpaced paste did not overrun the model device, so the intact test above proves nothing")
	}
}

func TestKeysAndPastesKeepTheirOrder(t *testing.T) {
	d := newSerialDev()
	r := newRig(t, d, Pacing{}, d.wait)
	r.k.Input([]byte("ab" + pasteStart + "cd\n" + pasteEnd + "ef" + pasteStart + "gh" + pasteEnd + "\r"))
	d.waitSent(t, len("abcd\refgh\r"))
	if got := strings.Join(d.lines(), "|"); got != "abcd|efgh" {
		t.Errorf("device ran %q, want abcd|efgh (typing order)", got)
	}
}

func TestPlainSSHIsNotDelayed(t *testing.T) {
	pl := &plainLink{note: make(chan struct{}, 1)}
	waits := 0
	r := newRig(t, pl, Pacing{}, func(time.Duration) <-chan time.Time { waits++; return nil })
	text := strings.Join(pasteLines, "\r") + "\r"
	r.paste(text)
	pl.waitSent(t, len(text))
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.sends != 1 || string(pl.sent) != text || waits != 0 {
		t.Errorf("%d sends and %d waits, sent %q; want the paste in one write, as typed", pl.sends, waits, pl.sent)
	}
}

func TestSerialLinkAtTheBMCPromptIsNotDelayed(t *testing.T) {
	d := newSerialDev()
	d.serial = false // the BMC's own CLI over ssh
	r := newRig(t, d, Pacing{}, d.wait)
	r.paste("ls\runtime\r")
	d.waitSent(t, 10)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sends != 1 || d.now != 0 {
		t.Errorf("sends = %d, virtual time %v; want one write and no delay", d.sends, d.now)
	}
}

func TestSizeKeyTypesSttyThroughTheQueue(t *testing.T) {
	d := newSerialDev()
	r := newRig(t, d, Pacing{}, d.wait)
	r.k.Input([]byte(string(rune(escapeByte)) + "s"))
	d.waitSent(t, len("stty rows 23 cols 80\r"))
	if got := d.lines(); len(got) != 1 || got[0] != "stty rows 23 cols 80" {
		t.Errorf("device ran %q", got)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.fits) != 0 {
		t.Error("FitSize was called; the console should type the line itself")
	}
	if d.sends != len("stty rows 23 cols 80\r") {
		t.Errorf("%d writes, want one byte at a time", d.sends)
	}
}

func TestSizeWithoutASizeLineUsesFitSize(t *testing.T) {
	d := newSerialDev()
	d.size = false // a window change over ssh types nothing
	r := newRig(t, d, Pacing{}, d.wait)
	r.k.Input([]byte(string(rune(escapeByte)) + "s"))
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.fits) != 1 || len(d.sent) != 0 {
		t.Errorf("fits %v, sent %q", d.fits, d.sent)
	}
}

// gated is a link whose delays end only when the test says so, to stop a paste
// part way through.
type gated struct {
	fakeLink
	waits  chan chan time.Time
	wrote  chan struct{}
	serial bool
}

func newGated() *gated {
	return &gated{waits: make(chan chan time.Time, 4096), wrote: make(chan struct{}, 4096), serial: true}
}

func (g *gated) Serial() bool { return g.serial }

func (g *gated) Send(b []byte) error {
	err := g.fakeLink.Send(b)
	g.wrote <- struct{}{}
	return err
}

func (g *gated) wait(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	g.waits <- ch
	return ch
}

// held waits until the typing queue is inside a delay: its last write is done
// and the bar has been told.
func (g *gated) held(t *testing.T) chan time.Time {
	t.Helper()
	select {
	case ch := <-g.waits:
		return ch
	case <-time.After(10 * time.Second):
		t.Fatal("the queue is not in a delay")
		return nil
	}
}

func (g *gated) text() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return string(g.sent)
}

func TestCtrlXClearsTheQueue(t *testing.T) {
	g := newGated()
	r := newRig(t, g, Pacing{}, g.wait)
	big := strings.Repeat("0123456789", 20) // 200 bytes
	r.paste(big)
	g.held(t)
	if r.state().sending == "" || r.con.Pending() != 199 {
		t.Fatalf("sending %q, pending %d; want progress on the bar", r.state().sending, r.con.Pending())
	}
	r.k.Input([]byte(string(rune(escapeByte)) + "x"))
	// Clear no longer waits for Run, so the bar catches up a moment later.
	settle(r.con)
	if r.con.Pending() != 0 || r.state().sending != "" {
		t.Errorf("pending %d, sending %q after ctrl+] x", r.con.Pending(), r.state().sending)
	}
	if n := r.state().note; !strings.Contains(n, "cleared") {
		t.Errorf("note = %q", n)
	}
	if got := g.text(); got != "0" {
		t.Errorf("sent %q, want only the byte already out", got)
	}
}

func TestCtrlCJumpsTheQueue(t *testing.T) {
	g := newGated()
	r := newRig(t, g, Pacing{}, g.wait)
	r.paste(strings.Repeat("x", 100))
	g.held(t)
	r.k.Input([]byte{0x03}) // the delay hasn't ended, and ctrl+c still goes out
	<-g.wrote
	<-g.wrote
	if got := g.text(); got != "x\x03" {
		t.Errorf("sent %q, want x then ctrl+c", got)
	}
	settle(r.con)
	if r.con.Pending() != 0 {
		t.Errorf("pending %d, want the queue dropped", r.con.Pending())
	}
}

func TestChordWritesDropTheQueueFirst(t *testing.T) {
	g := newGated()
	r := newRig(t, g, Pacing{}, g.wait)
	r.paste(strings.Repeat("y", 100))
	g.held(t)
	r.k.Input([]byte(string(rune(escapeByte)) + "]"))
	<-g.wrote
	<-g.wrote
	if got := g.text(); got != "y\x1d" {
		t.Errorf("sent %q, want the literal ctrl+] out at once after the queue was dropped", got)
	}
}

// solModel is a remote line behind IPMI SOL: the link's Break sends what
// internal/bmc/sol.go sends, CR then the escape char then B. ipmitool eats the
// escape and B; the remote sees the CR as Enter and runs whatever is typed.
type solModel struct {
	gated
	rmu    sync.Mutex
	line   string
	ran    []string
	breaks int
	wire   []byte
}

func newSolModel() *solModel { return &solModel{gated: *newGated()} }

func (m *solModel) feed(b []byte) {
	m.rmu.Lock()
	defer m.rmu.Unlock()
	m.wire = append(m.wire, b...)
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == 0x1e && i+1 < len(b) && b[i+1] == 'B':
			m.breaks++
			i++
		case b[i] == 0x03:
			m.line = ""
		case b[i] == '\r':
			m.ran = append(m.ran, m.line)
			m.line = ""
		default:
			m.line += string(rune(b[i]))
		}
	}
}

func (m *solModel) Send(b []byte) error { m.feed(b); return m.gated.Send(b) }
func (m *solModel) Break() error        { return m.Send([]byte{'\r', 0x1e, 'B'}) }
func (m *solModel) snapshot() ([]string, string, int) {
	m.rmu.Lock()
	defer m.rmu.Unlock()
	return append([]string(nil), m.ran...), m.line, m.breaks
}

// pasteUntil pastes text and lets the typer write n bytes, leaving it held in
// the delay after the nth.
func (m *solModel) pasteUntil(t *testing.T, r *rig, text string, n int) {
	t.Helper()
	r.paste(text)
	for i := 0; i < n; i++ {
		if i > 0 {
			m.held(t) <- time.Time{}
		}
		<-m.wrote
	}
	m.held(t)
}

func TestBreakIsRefusedWhileAPasteIsPending(t *testing.T) {
	m := newSolModel()
	r := newRig(t, m, Pacing{}, m.wait)
	m.pasteUntil(t, r, "reboot --help", 6) // "reboot" is on the remote line, " --help" is still queued
	r.k.Input([]byte(string(rune(escapeByte)) + "b"))
	ran, line, breaks := m.snapshot()
	if len(ran) != 0 || breaks != 0 {
		t.Fatalf("remote ran %q with %d breaks; BREAK's leading CR must not submit a half-typed paste", ran, breaks)
	}
	if line != "reboot" {
		t.Errorf("remote line = %q, want the typed prefix left alone", line)
	}
	if n := r.state().note; !strings.Contains(n, "ctrl+c") {
		t.Errorf("note = %q, want it to say to clear the line first", n)
	}
	r.k.Input([]byte(string(rune(escapeByte)) + "rb")) // SysRq is refused the same way
	if ran, _, breaks := m.snapshot(); len(ran) != 0 || breaks != 0 {
		t.Errorf("SysRq got through: ran %q, breaks %d", ran, breaks)
	}
}

func TestBreakIsRefusedAfterThePasteWasStoppedMidLine(t *testing.T) {
	m := newSolModel()
	r := newRig(t, m, Pacing{}, m.wait)
	m.pasteUntil(t, r, "reboot --help", 6)
	r.k.Input([]byte(string(rune(escapeByte)) + "x")) // drop the queue; "reboot" is already out
	r.k.Input([]byte(string(rune(escapeByte)) + "b"))
	if ran, line, breaks := m.snapshot(); len(ran) != 0 || breaks != 0 || line != "reboot" {
		t.Fatalf("ran %q line %q breaks %d; BREAK must not run the leftover prefix", ran, line, breaks)
	}
	// ctrl+c clears the line, and then BREAK goes out with the real SOL bytes.
	r.k.Input([]byte{0x03})
	<-m.wrote
	r.k.Input([]byte(string(rune(escapeByte)) + "b"))
	ran, line, breaks := m.snapshot()
	if breaks != 1 || line != "" || strings.Contains(strings.Join(ran, "|"), "reboot") {
		t.Errorf("ran %q line %q breaks %d; want a clean BREAK after ctrl+c", ran, line, breaks)
	}
	m.rmu.Lock()
	tail := string(m.wire[len(m.wire)-3:])
	m.rmu.Unlock()
	if tail != "\r\x1eB" {
		t.Errorf("BREAK bytes = %q, want CR + 0x1e B", tail)
	}
}

func TestBreakGoesOutOnACleanLine(t *testing.T) {
	m := newSolModel()
	r := newRig(t, m, Pacing{}, m.wait)
	m.pasteUntil(t, r, "ls\n", 3)
	r.k.Input([]byte(string(rune(escapeByte)) + "b"))
	if _, _, breaks := m.snapshot(); breaks != 1 {
		t.Errorf("breaks = %d, want 1 on a line that ends in Enter", breaks)
	}
}

func TestPasteMarkersAreNotForwardedAndPasteIsNormalised(t *testing.T) {
	pl := &plainLink{note: make(chan struct{}, 1)}
	r := newRig(t, pl, Pacing{}, nil)
	r.paste("a\tb\r\nc\nd\x1b[31m\x00e")
	want := "a b\rc\rd[31me"
	pl.waitSent(t, len(want))
	if got := string(pl.sent); got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

func TestSendLabelAndBarThreshold(t *testing.T) {
	for _, c := range []struct {
		sent, total int
		want        string
	}{{35, 100, "35/100"}, {1200, 4000, "1.2k/4k"}, {0, 1000, "0/1k"}, {999, 12500, "999/12.5k"}} {
		if got := sendLabel(c.sent, c.total); got != c.want {
			t.Errorf("sendLabel(%d,%d) = %q, want %q", c.sent, c.total, got, c.want)
		}
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	con := newConsole(&outLocked{w: &out}, "node01", "sol", 24, 80)
	bar := func() string {
		con.mu.Lock()
		defer con.mu.Unlock()
		return plain(barLine(con.st, 100))
	}
	con.SetSend(sendShowMin, 70)
	if strings.Contains(bar(), "sending") {
		t.Errorf("bar shows a meter for %d bytes: %q", sendShowMin, bar())
	}
	con.SetSend(sendShowMin+1, 4000)
	if b := bar(); !strings.Contains(b, "sending 3.9k/4k") || !strings.Contains(b, "x clears") {
		t.Errorf("bar = %q", b)
	}
	con.SetSend(0, 0)
	if strings.Contains(bar(), "sending") {
		t.Error("meter still on the bar after the queue emptied")
	}
	st := barState{host: "node01", kind: "sol", mode: modeCommand}
	if got := plain(barLine(st, 200)); strings.Contains(got, "clear queue") || strings.Contains(got, "raw paste") {
		t.Errorf("command bar = %q, want no clear queue hint while idle and no raw paste", got)
	}
	st.sending = "1/2k"
	if got := plain(barLine(st, 200)); !strings.Contains(got, "x clear queue") {
		t.Errorf("command bar = %q, want the clear queue hint while sending", got)
	}
}

func TestNewTyperDefaultsAndOverrides(t *testing.T) {
	if ty := NewTyper(&fakeLink{}, Pacing{}); ty.CharDelay != DefaultCharDelay || ty.LineDelay != DefaultLineDelay {
		t.Errorf("defaults = %v, %v", ty.CharDelay, ty.LineDelay)
	}
	ty := NewTyper(newSerialDev(), PacingMS(intp(20), intp(0)))
	if ty.CharDelay != 20*time.Millisecond || ty.LineDelay != 0 {
		t.Errorf("overrides = %v, %v", ty.CharDelay, ty.LineDelay)
	}
	if !ty.Paced() {
		t.Error("a serial link is paced")
	}
	if NewTyper(&fakeLink{}, Pacing{}).Paced() {
		t.Error("a link with no Serial is never paced")
	}
	if ty.EscDelay != DefaultEscDelay {
		t.Errorf("EscDelay = %v with one delay still on, want the default", ty.EscDelay)
	}
	if ty := NewTyper(newSerialDev(), PacingMS(intp(0), intp(0))); ty.EscDelay != 0 {
		t.Errorf("EscDelay = %v with both delays 0, want a plain writer", ty.EscDelay)
	}
	if p := PacingMS(nil, nil); p.Char != nil || p.Line != nil {
		t.Error("unset stays unset")
	}
}

func intp(n int) *int { return &n }

func TestOversizedPasteIsRejectedWhole(t *testing.T) {
	pl := &plainLink{note: make(chan struct{}, 1)}
	r := newRig(t, pl, Pacing{}, nil)
	big := strings.Repeat("x", pasteMax+1)
	// Arrive in several reads, with typing before and after.
	r.k.Input([]byte("a" + pasteStart + big[:pasteMax/2]))
	r.k.Input([]byte(big[pasteMax/2:] + "\r"))
	r.k.Input([]byte(pasteEnd + "z"))
	pl.waitSent(t, 2)
	pl.mu.Lock()
	sent := string(pl.sent)
	pl.mu.Unlock()
	if sent != "az" {
		t.Errorf("sent %d bytes (%.20q...), want only the typed a and z", len(sent), sent)
	}
	if st := r.state(); !st.noteErr || !strings.Contains(st.note, "paste not sent") {
		t.Errorf("note = %q (err %v), want a paste not sent error", st.note, st.noteErr)
	}
}

func TestTraceLogsBothWaysAndFixesTheMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OOB_DEBUG_PASTE", path)
	tr, err := openTrace()
	if err != nil {
		t.Fatal(err)
	}
	tr.log("TX", []byte("ls\r"))
	tr.log("RX", []byte("ls\r\n"))
	tr.close()
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || lines[0] != "old" || !strings.Contains(lines[1], ` TX "ls\r"`) || !strings.Contains(lines[2], ` RX "ls\r\n"`) {
		t.Errorf("trace = %q", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	t.Setenv("OOB_DEBUG_PASTE", "")
	if tr, err := openTrace(); tr != nil || err != nil {
		t.Errorf("unset: %v, %v; want no trace", tr, err)
	}
}

func TestEnterRawOpensTheTraceBeforeRawMode(t *testing.T) {
	raw := 0
	rawFn := func(int) (*term.State, error) { raw++; return &term.State{}, nil }
	boom := errors.New("trace refused")
	if _, _, err := enterRaw(0, func() (*trace, error) { return nil, boom }, rawFn); !errors.Is(err, boom) || raw != 0 {
		t.Errorf("err %v, raw mode entered %d times; want the trace error and no raw mode", err, raw)
	}
	if _, st, err := enterRaw(0, func() (*trace, error) { return nil, nil }, rawFn); err != nil || st == nil || raw != 1 {
		t.Errorf("err %v, state %v, raw %d; want raw mode entered once", err, st, raw)
	}
	// A raw-mode failure closes the trace that was already open.
	f, err := os.CreateTemp(t.TempDir(), "trace")
	if err != nil {
		t.Fatal(err)
	}
	tr := &trace{w: f}
	_, _, err = enterRaw(0, func() (*trace, error) { return tr, nil }, func(int) (*term.State, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, werr := f.Write([]byte("x")); werr == nil {
		t.Error("trace file still open after raw mode failed")
	}
}

func TestKeyAfterCtrlXInTheSameReadIsKept(t *testing.T) {
	g := newGated()
	r := newRig(t, g, Pacing{}, g.wait)
	r.paste(strings.Repeat("x", 100))
	delay := g.held(t)
	r.k.Input([]byte(string(rune(escapeByte)) + "xz"))
	delay <- time.Time{}
	for i := 0; i < 2; i++ {
		select {
		case <-g.wrote:
		case <-time.After(5 * time.Second):
			t.Fatalf("sent %q, want the queue cleared and then z", g.text())
		}
	}
	if got := g.text(); got != "xz" {
		t.Errorf("sent %q, want xz", got)
	}
}

func TestCtrlXClearsEvenIfPendingIsStale(t *testing.T) {
	g := newGated()
	r := newRig(t, g, Pacing{}, g.wait)
	r.k.ty.Progress = nil // the count never catches up, as if Enqueue had not reported yet
	r.paste(strings.Repeat("x", 100))
	delay := g.held(t)
	if r.con.Pending() != 0 {
		t.Fatalf("pending %d, want the stale count 0", r.con.Pending())
	}
	r.k.Input([]byte(string(rune(escapeByte)) + "x"))
	delay <- time.Time{}
	r.k.Input([]byte("z"))
	<-g.wrote
	<-g.wrote
	if got := g.text(); got != "xz" {
		t.Errorf("sent %q, want the queue cleared even though pending read 0", got)
	}
}

func TestZeroDelaysMakeEscapePasteAPlainWrite(t *testing.T) {
	zero := time.Duration(0)
	d := newSerialDev()
	r := newRig(t, d, Pacing{Char: &zero, Line: &zero}, func(time.Duration) <-chan time.Time {
		t.Error("typer waited with both delays set to 0")
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	})
	r.k.Input([]byte("\x1b:q"))
	d.waitSent(t, 3)
}

// recQueue is a shared console's queue as a window sees it: it records what
// the window asked of it.
type recQueue struct {
	mu         sync.Mutex
	enq        [][]byte
	urgent     [][]byte
	clears     int
	onProgress func(pending, total int)
}

func (q *recQueue) Enqueue(b []byte) <-chan struct{} {
	q.mu.Lock()
	q.enq = append(q.enq, append([]byte(nil), b...))
	q.mu.Unlock()
	done := make(chan struct{})
	close(done)
	return done
}
func (q *recQueue) Urgent(b []byte) { q.mu.Lock(); q.urgent = append(q.urgent, b); q.mu.Unlock() }
func (q *recQueue) Clear()          { q.mu.Lock(); q.clears++; q.mu.Unlock() }
func (q *recQueue) OnProgress(f func(pending, total int)) {
	q.mu.Lock()
	q.onProgress = f
	q.mu.Unlock()
}
func (q *recQueue) cleared() int { q.mu.Lock(); defer q.mu.Unlock(); return q.clears }

func TestLeavingASharedWindowDoesNotClearTheHubsQueue(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	con := newConsole(&outLocked{w: &out}, "node01", "sol", 24, 80)
	con.Start()
	t.Cleanup(func() { con.Close() })
	q := &recQueue{}
	fl := &fakeLink{}
	k := newKeys(&Setup{Link: fl, Queue: q}, con, func() {}, make(chan struct{}))
	k.start(nil)
	esc := string(rune(escapeByte))
	k.Input([]byte(esc + ".")) // disconnect: a joined window just leaves
	if fl.disconnects != 1 {
		t.Fatalf("disconnects = %d", fl.disconnects)
	}
	if n := q.cleared(); n != 0 {
		t.Errorf("leaving cleared the hub's queue %d times; it holds other windows' and the AI tool's typing", n)
	}
	// The explicit clear key still does: that is the user asking for it.
	k.Input([]byte(esc + "x"))
	if n := q.cleared(); n != 1 {
		t.Errorf("ctrl+] x cleared %d times, want 1", n)
	}
}

func TestLeavingALocalConsoleStillDropsItsOwnQueue(t *testing.T) {
	g := newGated()
	r := newRig(t, g, Pacing{}, g.wait)
	r.paste(strings.Repeat("x", 100))
	g.held(t)
	r.k.Input([]byte(string(rune(escapeByte)) + "."))
	if r.con.Pending() != 0 || g.disconnects != 1 {
		t.Errorf("pending %d, disconnects %d; want the local typer cleared and the console disconnected", r.con.Pending(), g.disconnects)
	}
}

// ctrlCGate is a link whose Send of ctrl+c blocks until released, like an
// interrupt still in flight to the BMC.
type ctrlCGate struct {
	fakeLink
	in      chan struct{}
	release chan struct{}
}

func (g *ctrlCGate) Send(b []byte) error {
	if bytes.Contains(b, []byte{0x03}) {
		g.in <- struct{}{}
		<-g.release
	}
	return g.fakeLink.Send(b)
}

func TestLineStaysDirtyWhileCtrlCIsInFlight(t *testing.T) {
	g := &ctrlCGate{in: make(chan struct{}, 1), release: make(chan struct{})}
	r := newRig(t, g, Pacing{}, nil)
	r.k.Input([]byte("reboot"))
	for deadline := time.Now().Add(5 * time.Second); !r.k.lineDirty.Load(); {
		if time.Now().After(deadline) {
			t.Fatal("line never became dirty")
		}
		time.Sleep(time.Millisecond)
	}
	r.k.Input([]byte{0x03})
	select {
	case <-g.in:
	case <-time.After(5 * time.Second):
		t.Fatal("ctrl+c was not sent")
	}
	if !r.k.lineDirty.Load() {
		t.Error("line marked clean before ctrl+c was sent")
	}
	close(g.release)
	for deadline := time.Now().Add(5 * time.Second); r.k.lineDirty.Load(); {
		if time.Now().After(deadline) {
			t.Fatal("line still dirty after ctrl+c was sent")
		}
		time.Sleep(time.Millisecond)
	}
}

// settle waits for the bar's pending count to reach zero. Clear and Urgent
// don't wait for Run, so the bar is told a moment after they return.
func settle(c *console) {
	for deadline := time.Now().Add(5 * time.Second); c.Pending() != 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
}
