package paste

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// lineDev is a fake serial device with a line editor. It echoes what it is
// sent, runs a line on Enter, and answers with whatever its mode says: a
// shell prompt, a continuation prompt, or a full-screen program's redraw.
type lineDev struct {
	mu      sync.Mutex
	rx      chan []byte
	ps1     string
	ps2     string
	endWord string // a line equal to this ends multi-line input
	start   string // a line containing this starts multi-line input
	inMulti bool
	screen  bool // full-screen: cursor-addressed echo, no prompt
	silent  bool // echoes characters, then says nothing at all after Enter
	buf     string
	lines   []string
}

func newLineDev(ps1 string) *lineDev {
	return &lineDev{rx: make(chan []byte, 4096), ps1: ps1}
}

func (d *lineDev) emit(s string) { d.rx <- []byte(s) }

// write plays the device's part for each byte, with no delay: the pacer's own
// timers set the pace.
func (d *lineDev) write(b []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range b {
		if c != '\r' {
			d.buf += string(rune(c))
			if d.screen {
				// vim-style: move to the text row and column, draw, update the ruler,
				// and put the cursor back.
				col := len(d.buf)
				d.emit(fmt.Sprintf("\x1b[%d;%dH%c\x1b[24;60H%d,%d\x1b[%d;%dH", len(d.lines)+3, col, c, len(d.lines)+1, col, len(d.lines)+3, col+1))
			} else {
				d.emit(string(rune(c)))
			}
			continue
		}
		line := d.buf
		d.buf = ""
		d.lines = append(d.lines, line)
		switch {
		case d.silent:
		case d.screen:
			d.emit(fmt.Sprintf("\x1b[%d;1H\x1b[24;60H%d,1", len(d.lines)+3, len(d.lines)+1))
		case d.inMulti && line == d.endWord:
			d.inMulti = false
			d.emit("\r\n" + d.ps1)
		case d.inMulti:
			d.emit("\r\n" + d.ps2)
		case d.start != "" && strings.Contains(line, d.start):
			d.inMulti = true
			d.emit("\r\n" + d.ps2)
		default:
			d.emit("\r\n" + d.ps1)
		}
	}
	return nil
}

func (d *lineDev) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.lines...)
}

// runPaste pastes text with a hard deadline, so a stall fails the test instead
// of hanging it.
func runPaste(t *testing.T, d *lineDev, prompt, text string, limit time.Duration) (error, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	p := &Pacer{Write: d.write, Rx: d.rx, Baud: 115200, Prompt: prompt}
	t0 := time.Now()
	err := p.Run(ctx, Lines(text), func(int) {})
	return err, time.Since(t0)
}

func heredocBody(n int) []string {
	lines := []string{"cat > /tmp/example.conf <<'EOF'"}
	for i := 1; i <= n; i++ {
		lines = append(lines, fmt.Sprintf("key%d = value%d", i, i))
	}
	return append(lines, "EOF", "ls")
}

// A heredoc's continuation prompt ("> ") is not the learned prompt
// ("root@node01:/etc# "). The pacer used to wait for the learned prompt for a
// minute, with the bar stuck on "pasting 1/64".
func TestHeredocContinuationPrompt(t *testing.T) {
	t.Parallel()
	d := newLineDev("root@node01:/etc# ")
	d.ps2, d.start, d.endWord = "> ", "<<'EOF'", "EOF"
	want := heredocBody(30)
	err, took := runPaste(t, d, d.ps1, strings.Join(want, "\n")+"\n", 8*time.Second)
	if err != nil {
		t.Fatalf("paste: %v after %v, want it to finish", err, took)
	}
	if got := d.got(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("device got %q, want %q", got, want)
	}
	if took > 4*time.Second {
		t.Errorf("took %v: waiting on something it should recognise", took)
	}
}

// Other continuation prompts: python, mysql, bash's own variants.
func TestContinuationPrompts(t *testing.T) {
	cases := []struct {
		name, ps1, ps2, start string
	}{
		{"python", ">>> ", "... ", "def "},
		{"mysql", "mysql> ", "    -> ", "select"},
		{"sh plus", "node01# ", "+ ", "for "},
		{"quote", "node01$ ", `"> `, `echo "`},
		{"unknown shape", "node01# ", "(cont)| ", "begin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := newLineDev(c.ps1)
			d.ps2, d.start, d.endWord = c.ps2, c.start, "end"
			want := []string{c.start + " one", "two", "three", "end", "after"}
			err, took := runPaste(t, d, c.ps1, strings.Join(want, "\n")+"\n", 8*time.Second)
			if err != nil {
				t.Fatalf("paste: %v after %v", err, took)
			}
			if got := d.got(); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("device got %q, want %q", got, want)
			}
			if took > 4*time.Second {
				t.Errorf("took %v", took)
			}
		})
	}
}

// vim in insert mode: no prompt, and the echo is cursor-addressed, so the
// cleaned line never matches what was typed. It used to stop with "the
// device didn't echo the whole line".
func TestFullScreenCursorAddressedEcho(t *testing.T) {
	t.Parallel()
	d := newLineDev("root@node01:/etc# ")
	d.screen = true
	want := []string{"first line of the file", "second line, a bit longer than the first", "third", "indented four"}
	err, took := runPaste(t, d, "", strings.Join(want, "\n")+"\n", 15*time.Second)
	if err != nil {
		t.Fatalf("paste: %v after %v, want it typed in full", err, took)
	}
	if got := d.got(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("device got %q, want %q", got, want)
	}
}

// A device that echoes the typing but says nothing after Enter must not hold
// the paste for a minute.
func TestSilentAfterEnterStops(t *testing.T) {
	t.Parallel()
	d := newLineDev("root@node01:~# ")
	d.silent = true
	err, took := runPaste(t, d, d.ps1, "echo one\necho two\n", 9*time.Second)
	var se *StopError
	if !errors.As(err, &se) || !strings.Contains(se.Reason, "no prompt or echo") {
		t.Fatalf("err = %v after %v, want a StopError about no prompt or echo", err, took)
	}
	if took > 8*time.Second {
		t.Errorf("took %v", took)
	}
}

// The full 64-line heredoc from the field report, end to end.
func TestHeredoc64Lines(t *testing.T) {
	t.Parallel()
	d := newLineDev("root@node01:/etc# ")
	d.ps2, d.start, d.endWord = "> ", "<<'EOF'", "EOF"
	want := heredocBody(61) // 64 lines with the cat line, EOF and ls
	if len(want) != 64 {
		t.Fatalf("setup: %d lines", len(want))
	}
	err, took := runPaste(t, d, d.ps1, strings.Join(want, "\n")+"\n", 20*time.Second)
	if err != nil || len(d.got()) != 64 {
		t.Fatalf("err %v after %v; device got %d lines, want 64", err, took, len(d.got()))
	}
}

// A paste into a program that is already full-screen (alternate screen on
// before the paste) is typed in timed writes from the first byte.
func TestFullScreenHintSkipsEchoCheck(t *testing.T) {
	t.Parallel()
	d := newLineDev("")
	d.silent = false
	d.screen = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var writes []string
	p := &Pacer{FullScreen: true, Baud: 115200, Rx: d.rx, Write: func(b []byte) error {
		writes = append(writes, string(b))
		return d.write(b)
	}}
	if err := p.Run(ctx, Lines("alpha beta gamma delta\nepsilon\n"), func(int) {}); err != nil {
		t.Fatal(err)
	}
	for _, w := range writes {
		if len(strings.TrimSuffix(w, "\r")) > blindChunk {
			t.Errorf("write %q is longer than %d bytes", w, blindChunk)
		}
	}
	if got := strings.Join(d.got(), "|"); got != "alpha beta gamma delta|epsilon" {
		t.Errorf("device got %q", got)
	}
}

// Dropped characters on an ordinary console still stop the paste: only
// cursor addressing in the output switches the echo check off.
func TestCursorAddressingIsWhatSwitchesOffTheEchoCheck(t *testing.T) {
	t.Parallel()
	e := newEchoRig()
	first := true
	p := &Pacer{Baud: 115200, Prompt: "node01# ", Rx: e.rx, Write: func(b []byte) error {
		_ = e.write(b)
		if first {
			first, b = false, b[1:]
		}
		e.rx <- b
		return nil
	}}
	stopsUnentered(t, p.Type(context.Background(), "stty rows 50 cols 200\r"), e)
}

func TestScreenDetection(t *testing.T) {
	r := &run{}
	for in, want := range map[string]bool{
		"plain text\r\n":     false,
		"\x1b[0m\x1b[K":      false,
		"\x1b[?2004h":        false, // bracketed paste is not a screen
		"\x1b[?1049h":        true,
		"\x1b[?25l\x1b[?47h": true,
		"\x1b[3;7Hx":         true,
		"\x1b[H\x1b[2J":      true,
		"\x1b[?1049l":        false,
	} {
		r.tail = nil
		if got := r.sawScreenProgram([]byte(in)); got != want {
			t.Errorf("sawScreenProgram(%q) = %v, want %v", in, got, want)
		}
	}
	// A sequence split over two reads is still seen.
	r.tail = nil
	if r.sawScreenProgram([]byte("abc\x1b[?10")) || !r.sawScreenProgram([]byte("49h")) {
		t.Error("split alt-screen sequence not detected")
	}
	var s screenTracker
	s.feed([]byte("\x1b[?1049h"))
	on1 := s.on
	s.feed([]byte("\x1b[?1049l"))
	if !on1 || s.on {
		t.Errorf("tracker: on after set %v, after reset %v", on1, s.on)
	}
}

func TestContinuationPattern(t *testing.T) {
	for in, want := range map[string]bool{
		">": true, "+": true, ">>>": true, "...": true, "    ->": true, "'>": true, `">`: true, "`>": true,
		"dquote>": true, "heredoc>": true, "/*>": true,
		"node01>": false, "root@host:~#": false, "45%": false, "": false,
	} {
		if got := continuation.MatchString(in); got != want {
			t.Errorf("continuation.MatchString(%q) = %v, want %v", in, got, want)
		}
	}
}

// A continuation prompt must not teach the pacer a new host: after the
// heredoc ends, "Switch#" style prompts still need to match the real one.
func TestContinuationDoesNotReplaceLearnedHost(t *testing.T) {
	r := &run{}
	r.learn("root@node01:/etc# ")
	r.learn("> ")
	if r.host != "root@node01" {
		t.Errorf("host = %q after a continuation prompt", r.host)
	}
}

// The trace logs what went out and came back, and never a password.
func TestTraceRedactsPasswords(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/trace.log"
	tr, err := OpenTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	e := newEchoRig()
	p := &Pacer{Baud: 115200, Prompt: "[sudo] password for alice: ", Rx: e.rx, Write: e.write, Trace: tr}
	if err := p.Type(context.Background(), "hunter2-secret\r"); err != nil {
		t.Fatal(err)
	}
	p2 := &Pacer{Baud: 115200, Prompt: "node01# ", Rx: e.rx, Write: func(b []byte) error { e.rx <- b; return nil }, Trace: tr}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p2.Run(ctx, Lines("echo visible\n"), func(int) {}); err != nil {
		t.Fatal(err)
	}
	tr.Close()
	b, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if strings.Contains(log, "hunter2") {
		t.Errorf("trace leaks the password:\n%s", log)
	}
	for _, want := range []string{"redacted", `TX   "echo vis"`, "RX", "PACE"} {
		if !strings.Contains(log, want) {
			t.Errorf("trace lacks %q:\n%s", want, log)
		}
	}
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
