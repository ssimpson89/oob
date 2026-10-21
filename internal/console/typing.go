package console

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ssimpson89/oob/internal/typer"
)

// serialLink is a link that can say whether what it is typing at is a serial
// line right now (IPMI SOL, iLO VSP, iDRAC com2, Supermicro SOL), where a
// burst of input overruns the BMC's buffer. A link without it, such as a plain
// ssh shell, has flow control and is never paced.
type serialLink interface {
	Serial() bool
}

// sizeLiner is a link whose FitSize types a command at the remote (stty over
// a serial console). It gives that line so the console can send it through
// the typing queue like everything else; ok is false when FitSize types
// nothing (an ssh window change).
type sizeLiner interface {
	SizeLine(rows, cols int) (line string, ok bool)
}

// The delays between bytes on a serial link: short between characters, long
// after Enter, when the remote may be busy running the line.
const (
	DefaultCharDelay = 10 * time.Millisecond
	DefaultLineDelay = 200 * time.Millisecond
	// DefaultEscDelay keeps a lone ESC apart from the next key, so vim and
	// readline don't read the two as one Alt-key. See typer.Typer.EscDelay.
	DefaultEscDelay = 150 * time.Millisecond
)

// Pacing overrides the default delays for serial links. A nil field keeps the
// default; a pointer to zero turns that delay off.
type Pacing struct {
	Char, Line *time.Duration
}

// PacingMS is Pacing from millisecond settings, as the host config holds them.
func PacingMS(char, line *int) Pacing {
	ms := func(n *int) *time.Duration {
		if n == nil {
			return nil
		}
		d := time.Duration(max(0, *n)) * time.Millisecond
		return &d
	}
	return Pacing{ms(char), ms(line)}
}

// NewTyper makes the input queue for a link: paced on a serial link, a plain
// writer otherwise. Run it with the link's Send as the writer.
func NewTyper(l Link, p Pacing) *typer.Typer {
	t := &typer.Typer{CharDelay: DefaultCharDelay, LineDelay: DefaultLineDelay, EscDelay: DefaultEscDelay}
	if p.Char != nil {
		t.CharDelay = *p.Char
	}
	if p.Line != nil {
		t.LineDelay = *p.Line
	}
	// Both delays off means a plain writer. The ESC gap is part of pacing, so
	// it goes too, or a zero-delay paste would still stall after every ESC.
	if t.CharDelay == 0 && t.LineDelay == 0 {
		t.EscDelay = 0
	}
	t.Paced = func() bool {
		s, ok := l.(serialLink)
		return ok && s.Serial()
	}
	return t
}

// pasteBytes is what a paste types: every line break is Enter (CR), Tab
// becomes a space (a CLI takes Tab as "complete this word"), and other control
// characters are dropped, since they would act instead of type.
func pasteBytes(text string) []byte {
	text = strings.NewReplacer("\r\n", "\r", "\n", "\r", "\t", " ").Replace(text)
	return []byte(strings.Map(func(r rune) rune {
		if r != '\r' && (r < ' ' || r == 0x7f) {
			return -1
		}
		return r
	}, text))
}

// newKeys is a consoleKeys with its typing queue made but not yet running.
func newKeys(cs *Setup, con *console, cleanup func(), stop <-chan struct{}) *consoleKeys {
	k := &consoleKeys{cs: cs, con: con, cleanup: cleanup, stop: stop}
	k.ty = NewTyper(cs.Link, cs.Pacing)
	k.ty.Progress = con.SetSend
	return k
}

// start runs the typing queue until the session ends. tr, if not nil, records
// the bytes it writes.
func (k *consoleKeys) start(tr *trace) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-k.stop
		cancel()
	}()
	guard(k.cleanup, func() {
		w := writerFunc(func(b []byte) (int, error) {
			tr.log("TX", b)
			k.track(b)
			return len(b), k.cs.Link.Send(b)
		})
		if err := k.ty.Run(ctx, w); ctx.Err() == nil {
			k.con.Note("typing stopped: "+err.Error(), true)
		}
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// typeKeys queues keystrokes. ctrl+c is the exception: it jumps the queue,
// dropping what was waiting, because nothing typed before an interrupt matters.
func (k *consoleKeys) typeKeys(b []byte) {
	if i := bytes.LastIndexByte(b, 0x03); i >= 0 {
		k.ty.Urgent([]byte{0x03})
		b = b[i+1:]
	}
	k.ty.Enqueue(b)
}

// fitSize makes the remote agree with rows x cols: where that means typing
// stty at a serial console, it goes through the queue like anything typed.
func (k *consoleKeys) fitSize(rows, cols int) error {
	if sl, ok := k.cs.Link.(sizeLiner); ok {
		if line, ok := sl.SizeLine(rows, cols); ok {
			k.ty.Enqueue([]byte(line + "\r"))
			return nil
		}
	}
	return k.cs.Link.FitSize(rows, cols)
}

// sizeError is the bar text for a size that didn't go out.
func sizeError(err error) string { return "size: " + err.Error() }

// sendLabel is the bar text for a queue being sent: "1.2k/4k" of bytes sent.
func sendLabel(sent, total int) string { return kilo(sent) + "/" + kilo(total) }

func kilo(n int) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
}

// sendShowMin is how much must be queued before the bar shows the progress:
// a line of typing is out in a moment and needs no meter.
const sendShowMin = 64

// trace is the opt-in byte trace, OOB_DEBUG_PASTE=<file>: every byte the
// queue writes (TX) and every byte the remote sends (RX), with the time. It
// holds everything typed, passwords included, so it is only for diagnosis.
type trace struct {
	mu sync.Mutex
	w  io.WriteCloser
	t0 time.Time
}

// openTrace opens the trace file named by OOB_DEBUG_PASTE (mode 0600, even if
// it existed). It returns nil, which logs nothing, if the variable is unset.
func openTrace() (*trace, error) {
	path := os.Getenv("OOB_DEBUG_PASTE")
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("OOB_DEBUG_PASTE: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, fmt.Errorf("OOB_DEBUG_PASTE: %w", err)
	}
	return &trace{w: f, t0: time.Now()}, nil
}

func (t *trace) close() {
	if t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.w.Close()
	}
}

func (t *trace) log(dir string, b []byte) {
	if t == nil || len(b) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.w, "%.6f %s %q\n", time.Since(t.t0).Seconds(), dir, b)
}
