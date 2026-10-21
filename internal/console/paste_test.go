package console

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// serialDev is a fake serial console behind a Link. Like a real BMC, it has a
// small input FIFO that drains slowly, and bytes written faster than that are
// lost. It echoes what it accepts, runs a line on Enter and prints a prompt.
type serialDev struct {
	mu        sync.Mutex
	con       *console
	fifo      []byte
	cap       int           // FIFO size in bytes
	drain     time.Duration // time to take one byte out of the FIFO
	dropped   int
	dropFirst int // bytes to lose from the start of the next Send, like a first byte lost on a laggy link
	sends     int // calls to Send
	sent      []byte
	buf       string // the input line so far
	applied   []string
	lineStart []time.Time   // when the first byte of each line was accepted
	promptAt  []time.Time   // when each prompt was printed
	holdPause time.Duration // "slow" takes this long before the prompt returns
	serial    bool
	sizeLine  bool
	fits      [][2]int
	stop      chan struct{}
}

const devPrompt = "node01# "

func newSerialDev(con *console) *serialDev {
	d := &serialDev{con: con, cap: 16, drain: time.Millisecond, serial: true, sizeLine: true, holdPause: 400 * time.Millisecond, stop: make(chan struct{})}
	go d.run()
	return d
}

func (d *serialDev) run() {
	t := time.NewTicker(d.drain)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
		}
		d.mu.Lock()
		if len(d.fifo) == 0 {
			d.mu.Unlock()
			continue
		}
		c := d.fifo[0]
		d.fifo = d.fifo[1:]
		d.mu.Unlock()
		d.take(c)
	}
}

func (d *serialDev) emit(s string) { d.con.Output([]byte(s)) }

func (d *serialDev) prompt() {
	d.mu.Lock()
	d.promptAt = append(d.promptAt, time.Now())
	d.mu.Unlock()
	d.emit(devPrompt)
}

func (d *serialDev) take(c byte) {
	d.mu.Lock()
	if d.buf == "" && c != '\r' {
		d.lineStart = append(d.lineStart, time.Now())
	}
	if c != '\r' {
		d.buf += string(rune(c))
		d.mu.Unlock()
		d.emit(string(rune(c)))
		return
	}
	line := d.buf
	d.buf = ""
	d.applied = append(d.applied, line)
	d.mu.Unlock()
	d.emit("\r\n")
	switch {
	case strings.HasPrefix(line, "bad"):
		d.emit("bash: " + line + ": command not found\r\n")
		d.prompt()
	case line == "reload":
		d.emit("Proceed with reload? [confirm]")
	case line == "slow":
		time.AfterFunc(d.holdPause, d.prompt)
	default:
		d.prompt()
	}
}

func (d *serialDev) Send(b []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sends++
	d.sent = append(d.sent, b...)
	if d.dropFirst > 0 {
		n := min(d.dropFirst, len(b))
		b, d.dropFirst = b[n:], d.dropFirst-n
		d.dropped += n
	}
	for _, c := range b {
		if len(d.fifo) >= d.cap {
			d.dropped++
			continue
		}
		d.fifo = append(d.fifo, c)
	}
	return nil
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
	return "stty rows " + strconv.Itoa(r) + " cols " + strconv.Itoa(c), d.sizeLine
}

// plainLink is a link that is not serial and has no size line: a plain ssh
// session. It records each Send.
type plainLink struct {
	fakeLink
	sends int
}

func (p *plainLink) Send(b []byte) error {
	p.mu.Lock()
	p.sends++
	p.mu.Unlock()
	return p.fakeLink.Send(b)
}

type pasteRig struct {
	k   *consoleKeys
	con *console
	dev *serialDev
	out *bytes.Buffer
}

func newPasteRig(t *testing.T) *pasteRig {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	con := newConsole(&outLocked{w: &out}, "node01", "sol", 24, 80)
	con.Start()
	dev := newSerialDev(con)
	con.Output([]byte(devPrompt))
	stop := make(chan struct{})
	k := &consoleKeys{cs: &Setup{Link: dev}, con: con, cleanup: func() {}, stop: stop}
	t.Cleanup(func() {
		close(stop)
		close(dev.stop)
		con.Close()
	})
	return &pasteRig{k: k, con: con, dev: dev, out: &out}
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

func (r *pasteRig) paste(text string) {
	r.k.Input([]byte(pasteStart + text + pasteEnd))
}

func (r *pasteRig) waitDone(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for r.k.isPasting() {
		if time.Now().After(deadline) {
			t.Fatal("paste never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// let the last echo and prompt land
	time.Sleep(50 * time.Millisecond)
}

func (r *pasteRig) wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *pasteRig) applied() []string {
	r.dev.mu.Lock()
	defer r.dev.mu.Unlock()
	return append([]string(nil), r.dev.applied...)
}

func (r *pasteRig) note() (string, bool) {
	r.con.mu.Lock()
	defer r.con.mu.Unlock()
	return r.con.st.note, r.con.st.noteErr
}

func (r *pasteRig) state() barState {
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

func TestPacedPasteDeliversIntact(t *testing.T) {
	r := newPasteRig(t)
	text := strings.Join(pasteLines, "\n") + "\n"
	r.paste(text)
	r.waitDone(t)
	if got := r.applied(); strings.Join(got, "|") != strings.Join(pasteLines, "|") {
		t.Errorf("device ran:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(pasteLines, "\n  "))
	}
	if r.dev.dropped != 0 {
		t.Errorf("dropped %d bytes", r.dev.dropped)
	}
	if n, isErr := r.note(); isErr || !strings.Contains(n, "pasted 6 lines") {
		t.Errorf("note = %q (err %v), want pasted 6 lines", n, isErr)
	}
	if r.state().pasteN != 0 {
		t.Error("progress still on the bar after the paste")
	}
}

func TestPasteTabsBecomeSpacesAndCRLFNormalised(t *testing.T) {
	r := newPasteRig(t)
	r.paste("echo\ta\tb\r\nls\r\n")
	r.waitDone(t)
	if got := strings.Join(r.applied(), "|"); got != "echo a b|ls" {
		t.Errorf("device ran %q", got)
	}
}

func TestRawPasteOverrunsTheDevice(t *testing.T) {
	r := newPasteRig(t)
	r.k.Input([]byte(string(rune(escapeByte)) + "w"))
	if !r.state().raw {
		t.Fatal("raw paste not shown on the bar")
	}
	text := strings.Join(pasteLines, "\n") + "\n"
	r.paste(text)
	r.wait(t, "the device to take the paste", func() bool {
		r.dev.mu.Lock()
		defer r.dev.mu.Unlock()
		return len(r.dev.fifo) == 0 && len(r.dev.sent) > 0
	})
	if r.dev.sends != 1 || string(r.dev.sent) != text {
		t.Errorf("raw paste: %d sends, sent %q; want the text in one write, unchanged", r.dev.sends, r.dev.sent)
	}
	if r.dev.dropped == 0 {
		t.Error("the fake device should lose bytes from an unpaced paste, or the paced test proves nothing")
	}
	r.k.Input([]byte(string(rune(escapeByte)) + "w"))
	if r.state().raw {
		t.Error("raw paste still on after toggling back")
	}
}

func TestSingleLinePasteStaysAtThePrompt(t *testing.T) {
	r := newPasteRig(t)
	r.paste("echo hello world from a long pasted line")
	r.waitDone(t)
	r.dev.mu.Lock()
	buf := r.dev.buf
	r.dev.mu.Unlock()
	if buf != "echo hello world from a long pasted line" || len(r.applied()) != 0 {
		t.Errorf("line = %q, ran %q; want it typed and left without Enter", buf, r.applied())
	}
	if r.dev.dropped != 0 {
		t.Errorf("dropped %d bytes", r.dev.dropped)
	}
}

func TestSingleKeystrokesStayImmediate(t *testing.T) {
	r := newPasteRig(t)
	r.k.Input([]byte("l"))
	r.dev.mu.Lock()
	sends, sent := r.dev.sends, string(r.dev.sent)
	r.dev.mu.Unlock()
	if sends != 1 || sent != "l" || r.k.isPasting() {
		t.Errorf("sends %d, sent %q, pasting %v", sends, sent, r.k.isPasting())
	}
}

func TestEchoMismatchStopsBeforeEnter(t *testing.T) {
	r := newPasteRig(t)
	r.dev.dropFirst = 1 // the first byte of the paste is lost: "s -l /tmp" instead of "ls -l /tmp"
	r.paste("ls -l /tmp/some/long/path\nuptime\n")
	r.waitDone(t)
	if got := r.applied(); len(got) != 0 {
		t.Errorf("device ran %q; Enter must not be pressed on a line that didn't echo right", got)
	}
	n, isErr := r.note()
	if !isErr || !strings.Contains(n, "paste stopped at line 1 of 2") || !strings.Contains(n, "ctrl+u") {
		t.Errorf("note = %q (err %v), want the stop reason and the ctrl+u hint", n, isErr)
	}
}

func TestPasteWaitsForThePromptBeforeTheNextLine(t *testing.T) {
	r := newPasteRig(t)
	r.paste("slow\nuptime\n")
	r.waitDone(t)
	if got := strings.Join(r.applied(), "|"); got != "slow|uptime" {
		t.Fatalf("device ran %q", got)
	}
	r.dev.mu.Lock()
	defer r.dev.mu.Unlock()
	if len(r.dev.lineStart) < 2 || len(r.dev.promptAt) < 2 {
		t.Fatalf("starts %d prompts %d", len(r.dev.lineStart), len(r.dev.promptAt))
	}
	// promptAt[0] is the prompt after "slow" (the initial prompt was printed
	// outside the device).
	if r.dev.lineStart[1].Before(r.dev.promptAt[0]) {
		t.Errorf("line 2 started %v before the prompt came back", r.dev.promptAt[0].Sub(r.dev.lineStart[1]))
	}
}

func TestPasteStopsAtDeviceError(t *testing.T) {
	r := newPasteRig(t)
	r.paste("uptime\nbadcmd --now\nls\nls\n")
	r.waitDone(t)
	if got := strings.Join(r.applied(), "|"); got != "uptime|badcmd --now" {
		t.Errorf("device ran %q; the paste should stop at the error", got)
	}
	if n, isErr := r.note(); !isErr || !strings.Contains(n, "paste stopped at line 2 of 4") || !strings.Contains(n, "command not found") {
		t.Errorf("note = %q (err %v)", n, isErr)
	}
}

func TestPasteStopsAtQuestion(t *testing.T) {
	r := newPasteRig(t)
	r.paste("reload\nyes\nls\n")
	r.waitDone(t)
	if got := strings.Join(r.applied(), "|"); got != "reload" {
		t.Errorf("device ran %q; the answer must not be pasted into a question", got)
	}
	if n, isErr := r.note(); !isErr || !strings.Contains(n, "paste stopped at line 1 of 3") || !strings.Contains(n, "asking") {
		t.Errorf("note = %q (err %v)", n, isErr)
	}
}

func TestCancelPaste(t *testing.T) {
	for _, tc := range []struct{ name, keys string }{{"esc", "\x1b"}, {"x", "x"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPasteRig(t)
			r.dev.holdPause = 5 * time.Second // the prompt doesn't come back, so the paste waits on line 1
			r.paste("slow\nuptime\nls\n")
			r.wait(t, "the paste to reach line 1", func() bool { return len(r.applied()) == 1 })
			if st := r.state(); st.pasteN != 3 || st.pasteAt != 1 {
				t.Errorf("bar progress %d/%d, want 1/3", st.pasteAt, st.pasteN)
			}
			r.k.Input([]byte(string(rune(escapeByte)) + tc.keys))
			r.k.Flush() // a bare ESC resolves after the wait
			r.waitDone(t)
			if got := strings.Join(r.applied(), "|"); got != "slow" {
				t.Errorf("device ran %q after cancel", got)
			}
			if n, isErr := r.note(); !isErr || !strings.Contains(n, "paste cancelled") {
				t.Errorf("note = %q (err %v), want a cancel note", n, isErr)
			}
			if r.state().pasteN != 0 {
				t.Error("progress still on the bar")
			}
		})
	}
}

func TestKeystrokesDuringPasteAreRejected(t *testing.T) {
	r := newPasteRig(t)
	r.dev.holdPause = 5 * time.Second
	r.paste("slow\nuptime\n")
	r.wait(t, "the paste to reach line 1", func() bool { return len(r.applied()) == 1 })
	r.dev.mu.Lock()
	before := len(r.dev.sent)
	r.dev.mu.Unlock()
	r.k.Input([]byte("zz\r"))
	r.dev.mu.Lock()
	after := len(r.dev.sent)
	r.dev.mu.Unlock()
	if after != before {
		t.Errorf("typed keys reached the console during a paste (%d bytes)", after-before)
	}
	if n, isErr := r.note(); !isErr || !strings.Contains(n, "ignored: paste in progress") {
		t.Errorf("note = %q (err %v)", n, isErr)
	}
	// Sending BREAK or the size is refused too, and a second paste.
	r.k.Input([]byte(string(rune(escapeByte)) + "s"))
	r.paste("ls\n")
	r.dev.mu.Lock()
	after = len(r.dev.sent)
	r.dev.mu.Unlock()
	if after != before {
		t.Errorf("size or a second paste typed during a paste (%d bytes)", after-before)
	}
	r.k.stopPaste()
	r.waitDone(t)
	r.k.Input([]byte("q"))
	r.dev.mu.Lock()
	last := string(r.dev.sent[len(r.dev.sent)-1:])
	r.dev.mu.Unlock()
	if last != "q" {
		t.Error("typing doesn't resume after the paste")
	}
}

func TestSizeKeyGoesThroughThePacer(t *testing.T) {
	r := newPasteRig(t)
	r.k.Input([]byte(string(rune(escapeByte)) + "s"))
	r.wait(t, "stty to run", func() bool { return len(r.applied()) == 1 })
	if got := r.applied()[0]; got != "stty rows 23 cols 80" {
		t.Errorf("device ran %q", got)
	}
	if len(r.dev.fits) != 0 {
		t.Error("FitSize was called; the console should type the line itself")
	}
	if n, isErr := r.note(); isErr || !strings.Contains(n, "sent size 23x80") {
		t.Errorf("note = %q (err %v)", n, isErr)
	}
	// Raw paste mode doesn't make the size line raw.
	r.k.Input([]byte(string(rune(escapeByte)) + "w"))
	r.k.Input([]byte(string(rune(escapeByte)) + "s"))
	r.wait(t, "second stty", func() bool { return len(r.applied()) == 2 })
	if r.dev.dropped != 0 || r.applied()[1] != "stty rows 23 cols 80" {
		t.Errorf("dropped %d, ran %q", r.dev.dropped, r.applied())
	}
}

func TestSizeLineWithFirstByteLostDoesNotRunTTY(t *testing.T) {
	r := newPasteRig(t)
	r.dev.dropFirst = 1 // "stty rows" arrives as "tty rows"
	r.k.Input([]byte(string(rune(escapeByte)) + "s"))
	n, isErr := r.note()
	if !isErr || !strings.Contains(n, "size not sent") {
		t.Errorf("note = %q (err %v), want a size not sent error", n, isErr)
	}
	time.Sleep(100 * time.Millisecond)
	if got := r.applied(); len(got) != 0 {
		t.Errorf("device ran %q; a line that didn't echo right must not get Enter", got)
	}
	r.dev.mu.Lock()
	buf := r.dev.buf
	r.dev.mu.Unlock()
	if !strings.HasPrefix(buf, "tty") {
		t.Errorf("line at the prompt = %q; want the line typed without its first byte and not entered", buf)
	}
}

func TestSizeWithoutSizeLineUsesFitSize(t *testing.T) {
	r := newPasteRig(t)
	r.dev.sizeLine = false // a window change over ssh types nothing
	r.k.Input([]byte(string(rune(escapeByte)) + "s"))
	r.dev.mu.Lock()
	defer r.dev.mu.Unlock()
	if len(r.dev.fits) != 1 || len(r.dev.sent) != 0 {
		t.Errorf("fits %v, sent %q", r.dev.fits, r.dev.sent)
	}
}

func TestPlainSSHIsNotPaced(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	con := newConsole(&outLocked{w: &out}, "node01", "ssh", 24, 80)
	con.Start()
	defer con.Close()
	pl := &plainLink{}
	stop := make(chan struct{})
	defer close(stop)
	k := &consoleKeys{cs: &Setup{Link: pl}, con: con, cleanup: func() {}, stop: stop}
	text := strings.Join(pasteLines, "\r") + "\r"
	k.Input([]byte(pasteStart + text + pasteEnd))
	if k.isPasting() {
		t.Fatal("a plain ssh paste was paced")
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.sends != 1 || string(pl.sent) != text {
		t.Errorf("%d sends, sent %q; want the paste in one write, as typed", pl.sends, pl.sent)
	}
}

func TestSerialLinkAtTheBMCPromptIsNotPaced(t *testing.T) {
	r := newPasteRig(t)
	r.dev.serial = false // the BMC's own CLI over ssh
	r.paste("ls\nuptime\n")
	if r.k.isPasting() {
		t.Fatal("paced at a non-serial prompt")
	}
	r.dev.mu.Lock()
	defer r.dev.mu.Unlock()
	if r.dev.sends != 1 {
		t.Errorf("sends = %d, want one raw write", r.dev.sends)
	}
	r.k.toggleRaw()
	if n, _ := r.note(); !strings.Contains(n, "never paced") {
		t.Errorf("note = %q", n)
	}
	if r.state().raw {
		t.Error("raw toggled on a link that is never paced")
	}
}

func TestBarShowsPasteProgressAndRaw(t *testing.T) {
	st := barState{host: "node01", kind: "sol", pasteAt: 3, pasteN: 12}
	if got := plain(barLine(st, 100)); !strings.Contains(got, "pasting 3/12") {
		t.Errorf("bar = %q", got)
	}
	if got := plain(barLine(st, 30)); !strings.Contains(got, "pasting 3/12") {
		t.Errorf("narrow bar lost the progress: %q", got)
	}
	st = barState{host: "node01", kind: "sol", raw: true}
	if got := plain(barLine(st, 80)); !strings.Contains(got, "[raw paste]") {
		t.Errorf("bar = %q", got)
	}
	cmd := barState{host: "node01", kind: "sol", mode: modeCommand}
	if got := plain(barLine(cmd, 200)); !strings.Contains(got, "w raw paste") || strings.Contains(got, "stop paste") {
		t.Errorf("command bar = %q, want raw paste and no stop paste while idle", got)
	}
	cmd.pasteN, cmd.raw = 5, true
	if got := plain(barLine(cmd, 200)); !strings.Contains(got, "w paced paste") || !strings.Contains(got, "x stop paste") {
		t.Errorf("command bar = %q, want paced paste and stop paste during a paste", got)
	}
}

// plainRig is a console on a plain ssh link, where pastes go out raw.
func plainRig(t *testing.T) (*consoleKeys, *plainLink, *console) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	con := newConsole(&outLocked{w: &out}, "node01", "ssh", 24, 80)
	con.Start()
	stop := make(chan struct{})
	pl := &plainLink{}
	t.Cleanup(func() { close(stop); con.Close() })
	return &consoleKeys{cs: &Setup{Link: pl}, con: con, cleanup: func() {}, stop: stop}, pl, con
}

func TestEveryPasteGoesOutAtItsOwnOffset(t *testing.T) {
	k, pl, _ := plainRig(t)
	k.Input([]byte("a" + pasteStart + "p" + pasteEnd + "b" + pasteStart + "q" + pasteEnd + "c"))
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if string(pl.sent) != "apbqc" {
		t.Errorf("sent %q, want apbqc (typing order)", pl.sent)
	}
}

func TestOversizedPasteIsRejectedWhole(t *testing.T) {
	k, pl, con := plainRig(t)
	big := strings.Repeat("x", pasteMax+1)
	// Arrive in several reads, with typing before and after.
	k.Input([]byte("a" + pasteStart + big[:pasteMax/2]))
	k.Input([]byte(big[pasteMax/2:] + "\r"))
	k.Input([]byte(pasteEnd + "z"))
	pl.mu.Lock()
	sent := string(pl.sent)
	pl.mu.Unlock()
	if sent != "az" {
		t.Errorf("sent %d bytes (%.20q...), want only the typed a and z", len(sent), sent)
	}
	con.mu.Lock()
	note, isErr := con.st.note, con.st.noteErr
	con.mu.Unlock()
	if !isErr || !strings.Contains(note, "paste not sent") {
		t.Errorf("note = %q (err %v), want a paste not sent error", note, isErr)
	}
}

func TestPasteAtTheLimitIsStillSent(t *testing.T) {
	k, pl, _ := plainRig(t)
	k.Input([]byte(pasteStart + strings.Repeat("x", pasteMax) + pasteEnd))
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if len(pl.sent) != pasteMax {
		t.Errorf("sent %d bytes, want %d", len(pl.sent), pasteMax)
	}
}
