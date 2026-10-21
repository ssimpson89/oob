package console

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ssimpson89/oob/internal/paste"
)

// serialLink is a link that can say whether what it is typing at is a serial
// line right now (IPMI SOL, iLO VSP, iDRAC com2, Supermicro SOL), where a
// burst of input overruns the BMC's buffer. A link without it, such as a plain
// ssh shell, has flow control and is never paced.
type serialLink interface {
	Serial() bool
}

// sizeLiner is a link whose FitSize types a command at the remote (stty over
// a serial console). It gives that line so the console can type it paced and
// echo-checked; ok is false when FitSize types nothing.
type sizeLiner interface {
	SizeLine(rows, cols int) (line string, ok bool)
}

// pasteBaud is the line rate the pacer assumes. It only sets a short settle
// time after Enter, so a wrong guess costs a few milliseconds.
const pasteBaud = 115200

// pasteNoteTTL is how long the reason a paste stopped stays on the bar.
const pasteNoteTTL = 20 * time.Second

// typer is the paced-typing state of a console session.
type typer struct {
	wmu sync.Mutex // held while a paste or a size line is being typed

	mu      sync.Mutex
	raw     bool // pastes go out unpaced
	pasting bool
	cancel  context.CancelFunc
	at      int  // the line a paste is on, counting from 0
	byKey   bool // a keypress stopped the paste
}

// traceEnv names the file that gets a debug trace of each paste.
const traceEnv = "OOB_DEBUG_PASTE"

// terminalReply matches what the terminal itself sends on stdin without the
// user touching a key: cursor position and device attribute reports, focus
// events, and OSC answers. They never stop a paste.
var terminalReply = regexp.MustCompile(`^(\x1b\[[?>0-9;]*[Rc]|\x1b\[[IO]|\x1b\].*(\x07|\x1b\\))$`)

func (k *consoleKeys) isPasting() bool {
	k.ty.mu.Lock()
	defer k.ty.mu.Unlock()
	return k.ty.pasting
}

// paced reports whether a paste is typed paced: on a serial link, unless the
// user switched to raw.
func (k *consoleKeys) paced() bool {
	k.ty.mu.Lock()
	raw := k.ty.raw
	k.ty.mu.Unlock()
	return !raw && k.serial()
}

func (k *consoleKeys) serial() bool {
	s, ok := k.cs.Link.(serialLink)
	return ok && s.Serial()
}

// keyStopsPaste handles typing during a paste. The first keystroke stops the
// paste and is not sent: the user is taking over, and a stuck paste must never
// hold the keyboard. It reports true if b was consumed. Keys after that go
// out as usual (the send waits for the paste to let go of the line).
func (k *consoleKeys) keyStopsPaste(b []byte) bool {
	k.ty.mu.Lock()
	if !k.ty.pasting || k.ty.byKey {
		k.ty.mu.Unlock()
		return false
	}
	if terminalReply.Match(b) {
		k.ty.mu.Unlock()
		return true
	}
	k.ty.byKey = true
	cancel := k.ty.cancel
	k.ty.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	k.con.Note("paste stopped by keypress", true)
	return true
}

// busy notes that a paste is running and reports true if so. Anything that
// would type at the remote waits for it: it is dropped, not held, because a
// held Enter would run half of a pasted line. (Plain keystrokes are different:
// the first one stops the paste, see keyStopsPaste.)
func (k *consoleKeys) busy(what string) bool {
	if !k.isPasting() {
		return false
	}
	k.con.Note(what+" ignored: paste in progress (ctrl+] x stops it)", true)
	return true
}

// runCtx is a context that ends when the session does.
func (k *consoleKeys) runCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-k.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// pacer builds a Pacer that types at the link, reading the echo from a tap
// opened together with the prompt snapshot. Call untap when done.
func (k *consoleKeys) pacer() (p *paste.Pacer, untap func()) {
	rx, untap, cursor := k.con.Tap()
	p = &paste.Pacer{
		Write:      k.cs.Link.Send,
		Rx:         rx,
		Baud:       pasteBaud,
		Prompt:     cursor,
		FullScreen: k.con.taps.FullScreen(),
	}
	if path := os.Getenv(traceEnv); path != "" {
		tr, err := paste.OpenTrace(path)
		if err != nil {
			k.con.Note("paste trace not written: "+err.Error(), true)
		} else {
			p.Trace = tr
			stopTap := untap
			return p, func() { stopTap(); tr.Close() }
		}
	}
	return p, untap
}

// pasteText handles a finished bracketed paste.
func (k *consoleKeys) pasteText(text string) bool {
	if k.busy("paste") {
		return true
	}
	if !k.paced() {
		k.ty.wmu.Lock()
		defer k.ty.wmu.Unlock()
		return k.cs.Link.Send([]byte(text)) == nil
	}
	lines := paste.Lines(text)
	if len(lines) == 0 {
		return true
	}
	ctx, cancel := k.runCtx()
	k.ty.mu.Lock()
	k.ty.pasting, k.ty.cancel, k.ty.at, k.ty.byKey = true, cancel, 0, false
	k.ty.mu.Unlock()
	k.con.SetPaste(1, len(lines))
	guard(k.cleanup, func() {
		defer cancel()
		k.ty.wmu.Lock()
		defer k.ty.wmu.Unlock()
		p, untap := k.pacer()
		err := p.Run(ctx, lines, func(i int) {
			k.ty.mu.Lock()
			k.ty.at = i
			k.ty.mu.Unlock()
			k.con.SetPaste(i+1, len(lines))
		})
		untap()
		k.pasteEnded(err, lines)
	})
	return true
}

// pasteEnded reports how a paste finished, on the bar and in the log.
func (k *consoleKeys) pasteEnded(err error, lines []string) {
	k.ty.mu.Lock()
	at, byKey := k.ty.at, k.ty.byKey
	k.ty.pasting, k.ty.cancel, k.ty.byKey = false, nil, false
	k.ty.mu.Unlock()
	k.con.SetPaste(0, 0)
	n := len(lines)
	var se *paste.StopError
	var msg string
	switch {
	case err == nil:
		if n == 1 && !strings.HasSuffix(lines[0], "\r") {
			msg = "pasted 1 line, left at the prompt"
		} else {
			msg = fmt.Sprintf("pasted %d lines", n)
		}
		k.con.Note(msg, false)
		k.con.Mark(msg)
		return
	case errors.As(err, &se):
		msg = fmt.Sprintf("paste stopped at line %d of %d: %s", se.Line, n, se.Reason)
		if se.Unentered {
			msg += "; ctrl+u clears the line, ctrl+] w pastes raw"
		}
	case byKey && errors.Is(err, context.Canceled):
		msg = fmt.Sprintf("paste stopped by keypress at line %d of %d; a half-typed line may remain (ctrl+u clears it)", at+1, n)
	case errors.Is(err, context.Canceled):
		msg = fmt.Sprintf("paste cancelled at line %d of %d; a half-typed line may remain (ctrl+u clears it)", at+1, n)
	default:
		msg = fmt.Sprintf("paste stopped at line %d of %d: %v", at+1, n, err)
	}
	k.con.NoteFor(msg, true, pasteNoteTTL)
	k.con.Mark(msg)
}

// stopPaste cancels a paste in progress.
func (k *consoleKeys) stopPaste() {
	k.ty.mu.Lock()
	cancel := k.ty.cancel
	k.ty.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// toggleRaw switches between paced and raw paste.
func (k *consoleKeys) toggleRaw() {
	if !k.serial() {
		k.con.Note("pastes are never paced here: ssh has flow control", false)
		return
	}
	k.ty.mu.Lock()
	k.ty.raw = !k.ty.raw
	raw := k.ty.raw
	k.ty.mu.Unlock()
	k.con.SetRaw(raw)
	if raw {
		k.con.Note("raw paste on: pastes go out all at once", false)
	} else {
		k.con.Note("paced paste on", false)
	}
}

// fitSize makes the remote agree with rows x cols. Where that means typing
// stty at a serial console, the line is typed paced and echo-checked, so a
// dropped first byte can't turn stty into tty. This goes through the pacer
// even in raw paste mode.
func (k *consoleKeys) fitSize(rows, cols int) error {
	if sl, ok := k.cs.Link.(sizeLiner); ok {
		if line, ok := sl.SizeLine(rows, cols); ok {
			return k.typeLine(line + "\r")
		}
	}
	return k.cs.Link.FitSize(rows, cols)
}

// typeLine types one line, echo-checked, and presses Enter. It waits for a
// paste in progress to end.
func (k *consoleKeys) typeLine(line string) error {
	ctx, cancel := k.runCtx()
	defer cancel()
	k.ty.wmu.Lock()
	defer k.ty.wmu.Unlock()
	p, untap := k.pacer()
	defer untap()
	return p.Type(ctx, line)
}

// sizeError is the bar text for a size line that didn't go out.
func sizeError(err error) string {
	var se *paste.StopError
	if errors.As(err, &se) {
		return "size not sent: " + se.Reason + "; ctrl+u clears the line"
	}
	return "size: " + err.Error()
}
