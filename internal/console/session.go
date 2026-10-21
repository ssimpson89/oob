package console

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ssimpson89/oob/internal/typer"
	"golang.org/x/term"
)

// Link is the transport under the console: what the key menu acts on.
type Link interface {
	Send(b []byte) error
	// Break sends a serial BREAK.
	Break() error
	// Disconnect asks the transport to end the session.
	Disconnect() error
	// Resize tells the transport the window is now rows x cols. It runs on
	// every resize and never types anything at the remote.
	Resize(rows, cols int) error
	// FitSize makes the remote agree with rows x cols (s key). Over SOL that
	// types stty into the remote shell, so it only runs on request or, if
	// auto-size is on, after a resize settles.
	FitSize(rows, cols int) error
	// SizeIsFree reports that resizing is safe to follow automatically
	// because it doesn't type anything (ssh window-change).
	SizeIsFree() bool
}

// toggler is a link that can switch between the host console and the BMC's own
// CLI prompt (c key). It returns the bar label and whether it is at the prompt.
type toggler interface {
	Toggle() (kind string, atPrompt bool, err error)
}

// observer is a link that wants to see the remote's output, to resync its
// idea of console versus prompt.
type observer interface {
	Observe(b []byte) (kind string, atPrompt, changed bool)
}

// Result is how the transport ended.
type Result struct {
	Code int
	Err  error
}

// Setup describes one console session.
type Setup struct {
	Host  string
	Kind  string // "sol" or "ssh"
	Link  Link
	Out   io.Reader     // the remote's output
	Done  <-chan Result // receives once when the transport has ended
	Power func() (string, error)
	// Connecting, if set, is shown on the bar until the remote first outputs.
	Connecting string
	// AtPrompt: the session starts at the BMC's CLI prompt, not a console.
	AtPrompt bool
	// NoToggle is the bar note for the c key when link can't toggle; empty
	// means the IPMI SOL text.
	NoToggle string
	// Pacing overrides the typing delays on serial links.
	Pacing Pacing
	// Queue, if set, replaces the console's own typing queue: the console is
	// shared and one typer at the hub paces what every window types.
	Queue Queue
	// Shared, if set, says the console is shared: the bar shows who is on it
	// and the i key pauses the AI tool.
	Shared Shared
}

// UseBar reports whether the bottom bar can run: stdin and stdout must both be
// terminals and tall enough.
func UseBar() bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return false
	}
	_, h, err := term.GetSize(int(os.Stdout.Fd()))
	return err == nil && h >= minBarRows
}

// RemoteRows is the height the remote sees: everything above the bar, or the
// whole window when it is too small for a bar.
func RemoteRows(rows int) int {
	if rows < minBarRows {
		return max(1, rows)
	}
	return rows - 1
}

// Timing in the session.
const (
	// winchDebounce lets a window drag settle before auto-size types stty into
	// the remote, so one resize sends one command.
	winchDebounce = 300 * time.Millisecond
	// sysrqGap separates BREAK from the SysRq key: the remote has to see the
	// BREAK first or the key is just a character.
	sysrqGap = 100 * time.Millisecond
	// outDrain is how long to wait for the last remote output after the
	// transport ends, before tearing the bar down.
	outDrain = 500 * time.Millisecond
)

// guard runs fn in a goroutine. A panic restores the terminal before the
// program dies, since the main goroutine's defers won't run.
func guard(cleanup func(), fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				cleanup()
				panic(r)
			}
		}()
		fn()
	}()
}

// enterRaw opens the trace and then puts the terminal in raw mode. The trace
// goes first so that a failure to open it leaves the terminal as it was.
func enterRaw(fd int, open func() (*trace, error), raw func(int) (*term.State, error)) (*trace, *term.State, error) {
	tr, err := open()
	if err != nil {
		return nil, nil, err
	}
	old, err := raw(fd)
	if err != nil {
		tr.close()
		return nil, nil, err
	}
	return tr, old, nil
}

// Run puts the terminal in raw mode, runs the bar and key menu around
// the transport, and restores everything on every exit path.
func Run(cs Setup) (int, error) {
	fd, ofd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	w, h, err := term.GetSize(ofd)
	if err != nil {
		return 1, err
	}
	tr, old, err := enterRaw(fd, openTrace, term.MakeRaw)
	if err != nil {
		return 1, err
	}
	defer tr.close()
	con := newConsole(os.Stdout, cs.Host, cs.Kind, h, w)
	var once sync.Once
	var logs []string
	cleanup := func() {
		once.Do(func() {
			logs = con.Close()
			_ = term.Restore(fd, old)
		})
	}
	defer func() {
		cleanup()
		for _, p := range logs {
			fmt.Fprintln(os.Stderr, "log saved:", p)
		}
	}()
	defer RestoreOnSignal(fd, old, cleanup, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)()
	con.SetKind(cs.Kind, cs.AtPrompt)
	con.Start()
	if cs.Connecting != "" {
		con.Sticky(cs.Connecting)
	}

	stop := make(chan struct{}) // closed when Run returns
	defer close(stop)

	auto := cs.Link.SizeIsFree()
	con.SetAuto(auto)

	if cs.Shared != nil {
		con.SetShared(cs.Shared.Status())
		guard(cleanup, func() {
			for {
				select {
				case <-stop:
					return
				case n := <-cs.Shared.Notes():
					con.SharedNote(n, cs.Shared.Status())
				}
			}
		})
	}

	outDone := make(chan struct{})
	guard(cleanup, func() {
		defer close(outDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := cs.Out.Read(buf)
			tr.log("RX", buf[:n])
			con.Output(buf[:n])
			if o, ok := cs.Link.(observer); ok && n > 0 {
				if kind, at, changed := o.Observe(buf[:n]); changed {
					con.SetKind(kind, at)
				}
			}
			if err != nil {
				return
			}
		}
	})

	keys := newKeys(&cs, con, cleanup, stop)
	keys.auto = auto
	keys.start(tr)

	// Window changes: follow the size at once, type stty only if asked.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	guard(cleanup, func() {
		var timer *time.Timer
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
		for {
			select {
			case <-stop:
				return
			case <-winch:
			}
			nw, nh, err := term.GetSize(ofd)
			if err != nil {
				continue
			}
			con.SetSize(nh, nw)
			_ = cs.Link.Resize(RemoteRows(nh), nw)
			if keys.autoOn() && !cs.Link.SizeIsFree() {
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(winchDebounce, func() {
					select {
					case <-stop:
						return
					default:
					}
					r, c := con.Size()
					if err := keys.fitSize(RemoteRows(r), c); err != nil {
						con.Note(sizeError(err), true)
					}
				})
			}
		}
	})

	in := make(chan []byte)
	go func() { // blocked in Read until a key or exit; it just hands bytes over
		defer close(in)
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				select {
				case in <- append([]byte(nil), buf[:n]...):
				case <-stop:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	guard(cleanup, func() {
		for {
			var wait <-chan time.Time
			if keys.parser.Pending() {
				wait = time.After(escWait)
			}
			var ok bool
			select {
			case <-stop:
				return
			case b, open := <-in:
				if !open {
					return
				}
				ok = keys.Input(b)
			case <-wait:
				ok = keys.Flush()
			}
			if !ok {
				return
			}
		}
	})

	res := <-cs.Done
	select { // let the last output land before the bar goes away
	case <-outDone:
	case <-time.After(outDrain):
	}
	return res.Code, res.Err
}

// consoleKeys carries out what the user types: the key parser plus the actions
// it produces. It is separate from Run so tests can drive it with a fake
// link.
type consoleKeys struct {
	cs      *Setup
	con     *console
	cleanup func()
	stop    <-chan struct{}
	parser  keyParser

	autoMu sync.Mutex // guards auto
	auto   bool

	ty *typer.Typer // everything typed or pasted goes through it, in order (nil when shared)
	q  Queue        // ty, or the hub's queue

	noted atomic.Bool // the link's input note was shown

	// lineDirty: the last byte written was neither Enter nor ctrl+c, so the
	// remote line may hold unsubmitted input. See breakSafe.
	lineDirty atomic.Bool
}

func (k *consoleKeys) autoOn() bool {
	k.autoMu.Lock()
	defer k.autoMu.Unlock()
	return k.auto
}

// Input handles a read from the keyboard. It reports false when the session is
// over: the console is stopping or the link refused the bytes.
func (k *consoleKeys) Input(b []byte) bool {
	fwd, acts := k.parser.Feed(b)
	return k.apply(fwd, acts)
}

// Flush resolves a held ESC after escWait with no more input.
func (k *consoleKeys) Flush() bool {
	fwd, acts := k.parser.Flush()
	return k.apply(fwd, acts)
}

func (k *consoleKeys) apply(fwd []byte, acts []action) bool {
	select {
	case <-k.stop:
		return false
	default:
	}
	k.con.SetMode(k.parser.Mode())
	// Bytes and actions run in the order the keys were pressed: each action
	// carries the number of forwarded bytes that came before it.
	pos := 0
	for _, a := range acts {
		if a.at > pos {
			k.typeKeys(fwd[pos:a.at])
			pos = a.at
		}
		k.do(a)
	}
	k.typeKeys(fwd[pos:])
	return true
}

// breakSafe reports whether BREAK (and SysRq, which starts with one) may go
// out now, and says why on the bar if not. A BREAK is not free of side
// effects: IPMI SOL's escape starts with a CR, which the remote sees as Enter.
// If a paste is part way through, or the remote line already holds typed text,
// that CR would run a truncated command ("reboot" out of "reboot --help"). So
// BREAK is refused until the line is clean. Dropping the queue first would not
// help, since the bytes already sent stay on the remote line.
func (k *consoleKeys) breakSafe() bool {
	if k.ty == nil { // shared: the hub's typer and line tracking decide, and refuse
		return true
	}
	if k.ty.Queued() == 0 && !k.lineDirty.Load() {
		return true
	}
	k.con.Note("BREAK not sent: input is pending on the line; clear the line first (ctrl+c)", true)
	return false
}

// track records what the remote line holds after b is written: Enter or
// ctrl+c leaves it clean, anything else leaves typed-but-unsubmitted input.
// It is deliberately pessimistic (it cannot see the remote's line editing).
func (k *consoleKeys) track(b []byte) {
	for _, c := range b {
		k.lineDirty.Store(c != '\r' && c != '\n' && c != 0x03)
	}
}

// do carries out one action. Anything that writes to the remote outside the
// queue (the disconnect sequence, the BMC prompt switch) clears the queue
// first, so no half-typed paste follows it.
func (k *consoleKeys) do(a action) {
	cs, con := k.cs, k.con
	rows, cols := con.Size()
	switch a.act {
	case actPaste:
		text := a.text
		if _, ok := cs.Link.(InputMapper); ok {
			text = StripEsc(text)
		}
		b := pasteBytes(text)
		k.q.Enqueue(b)
		con.Mark(fmt.Sprintf("pasted %d bytes", len(b)))
	case actPasteTooBig:
		con.Note(fmt.Sprintf("paste not sent: over %d MiB", pasteMax>>20), true)
	case actStop:
		// Always clear: Enqueue may not have updated Pending yet. The count
		// only decides whether there is anything to tell the user.
		queued := con.Pending() > 0
		k.q.Clear()
		if queued {
			con.Note("typing queue cleared", false)
			con.Mark("typing queue cleared")
		}
	case actSize:
		if err := k.fitSize(RemoteRows(rows), cols); err != nil {
			con.Note(sizeError(err), true)
			return
		}
		con.Note(fmt.Sprintf("sent size %dx%d", RemoteRows(rows), cols), false)
		con.Mark(fmt.Sprintf("size sent: %d rows, %d cols", RemoteRows(rows), cols))
	case actAuto:
		if cs.Link.SizeIsFree() {
			con.Note("ssh follows the window size automatically", false)
			return
		}
		k.autoMu.Lock()
		k.auto = !k.auto
		on := k.auto
		k.autoMu.Unlock()
		con.SetAuto(on)
		if on {
			con.Note("auto-size on", false)
		} else {
			con.Note("auto-size off", false)
		}
	case actLog:
		con.ToggleLog()
	case actBreak:
		if !k.breakSafe() {
			return
		}
		if err := cs.Link.Break(); err != nil {
			con.Note("break: "+err.Error(), true)
			return
		}
		con.Note("BREAK sent", false)
		con.Mark("BREAK sent")
	case actSysrq:
		if !k.breakSafe() {
			return
		}
		if err := cs.Link.Break(); err != nil {
			con.Note("break: "+err.Error(), true)
			return
		}
		time.Sleep(sysrqGap)
		k.q.Urgent([]byte{a.arg})
		con.Note("SysRq "+string(a.arg)+" sent", false)
		con.Mark("SysRq " + string(a.arg) + " sent")
	case actBMC:
		t, ok := cs.Link.(toggler)
		if !ok {
			msg := cs.NoToggle
			if msg == "" {
				msg = "no BMC prompt on IPMI SOL"
			}
			con.Note(msg, false)
			return
		}
		k.q.Clear()
		kind, at, err := t.Toggle()
		con.SetKind(kind, at)
		if err != nil {
			con.Note("bmc prompt: "+err.Error(), true)
			return
		}
		con.Note("now at "+kind, false)
		con.Mark("switched to " + kind)
	case actLiteral:
		k.q.Urgent([]byte{escapeByte})
	case actPower:
		if cs.Power == nil {
			return
		}
		con.Note("power cycling...", false)
		con.Mark("power cycle requested")
		guard(k.cleanup, func() {
			out, err := cs.Power()
			if err != nil {
				con.Note("power cycle failed: "+err.Error(), true)
				con.Mark("power cycle failed: " + err.Error())
				return
			}
			msg := "power cycle: " + strings.TrimSpace(out)
			con.Note(msg, false)
			con.Mark(msg)
		})
	case actDisconnect:
		con.Mark("disconnect")
		k.q.Clear()
		_ = cs.Link.Disconnect()
	case actAI:
		if cs.Shared == nil {
			con.Note("this console isn't shared with an AI tool", false)
			return
		}
		if err := cs.Shared.TogglePause(); err != nil {
			con.Note(err.Error(), true)
		}
	case actHelp:
		con.Print(helpText())
	}
}

// RestoreOnSignal restores the terminal and exits with 128+signal if one of sigs
// arrives (raw mode and no-echo would otherwise be left on). If cleanup is not
// nil it runs first (the console uses it to reset the scroll region and clear
// the bar). Call the returned func to stop watching.
func RestoreOnSignal(fd int, st *term.State, cleanup func(), sigs ...os.Signal) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	done := make(chan struct{})
	go func() {
		select {
		case s := <-ch:
			if cleanup != nil {
				cleanup()
			}
			term.Restore(fd, st)
			n := 1
			if sn, ok := s.(syscall.Signal); ok {
				n = int(sn)
			}
			os.Exit(128 + n)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
