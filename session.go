package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// link is the transport under the console: what the key menu acts on.
type link interface {
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

// result is how the transport ended.
type result struct {
	code int
	err  error
}

// consoleSetup describes one console session.
type consoleSetup struct {
	host  string
	kind  string // "sol" or "ssh"
	link  link
	out   io.Reader     // the remote's output
	done  <-chan result // receives once when the transport has ended
	power func() (string, error)
	// connecting, if set, is shown on the bar until the remote first outputs.
	connecting string
	// atPrompt: the session starts at the BMC's CLI prompt, not a console.
	atPrompt bool
	// noToggle is the bar note for the c key when link can't toggle; empty
	// means the IPMI SOL text.
	noToggle string
}

// useBar reports whether the bottom bar can run: stdin and stdout must both be
// terminals and tall enough.
func useBar() bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return false
	}
	_, h, err := term.GetSize(int(os.Stdout.Fd()))
	return err == nil && h >= minBarRows
}

// remoteRows is the height the remote sees: everything above the bar, or the
// whole window when it is too small for a bar.
func remoteRows(rows int) int {
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

// runConsole puts the terminal in raw mode, runs the bar and key menu around
// the transport, and restores everything on every exit path.
func runConsole(cs consoleSetup) (int, error) {
	fd, ofd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	w, h, err := term.GetSize(ofd)
	if err != nil {
		return 1, err
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return 1, err
	}
	con := newConsole(os.Stdout, cs.host, cs.kind, h, w)
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
	defer restoreOnSignal(fd, old, cleanup, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)()
	con.SetKind(cs.kind, cs.atPrompt)
	con.Start()
	if cs.connecting != "" {
		con.Sticky(cs.connecting)
	}

	stop := make(chan struct{}) // closed when runConsole returns
	defer close(stop)

	auto := cs.link.SizeIsFree()
	con.SetAuto(auto)

	outDone := make(chan struct{})
	guard(cleanup, func() {
		defer close(outDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := cs.out.Read(buf)
			con.Output(buf[:n])
			if o, ok := cs.link.(observer); ok && n > 0 {
				if kind, at, changed := o.Observe(buf[:n]); changed {
					con.SetKind(kind, at)
				}
			}
			if err != nil {
				return
			}
		}
	})

	keys := &consoleKeys{cs: &cs, con: con, cleanup: cleanup, stop: stop, auto: auto}

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
			_ = cs.link.Resize(remoteRows(nh), nw)
			if keys.autoOn() && !cs.link.SizeIsFree() {
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
					_ = cs.link.FitSize(remoteRows(r), c)
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

	res := <-cs.done
	select { // let the last output land before the bar goes away
	case <-outDone:
	case <-time.After(outDrain):
	}
	return res.code, res.err
}

// consoleKeys carries out what the user types: the key parser plus the actions
// it produces. It is separate from runConsole so tests can drive it with a fake
// link.
type consoleKeys struct {
	cs      *consoleSetup
	con     *console
	cleanup func()
	stop    <-chan struct{}
	parser  keyParser

	autoMu sync.Mutex // guards auto
	auto   bool
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
	if len(fwd) > 0 {
		if k.cs.link.Send(fwd) != nil {
			return false
		}
	}
	k.con.SetMode(k.parser.Mode())
	for _, a := range acts {
		k.do(a)
	}
	return true
}

// do carries out one action.
func (k *consoleKeys) do(a action) {
	cs, con := k.cs, k.con
	rows, cols := con.Size()
	switch a.act {
	case actSize:
		if err := cs.link.FitSize(remoteRows(rows), cols); err != nil {
			con.Note("size: "+err.Error(), true)
			return
		}
		con.Note(fmt.Sprintf("sent size %dx%d", remoteRows(rows), cols), false)
		con.Mark(fmt.Sprintf("size sent: %d rows, %d cols", remoteRows(rows), cols))
	case actAuto:
		if cs.link.SizeIsFree() {
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
		if err := cs.link.Break(); err != nil {
			con.Note("break: "+err.Error(), true)
			return
		}
		con.Note("BREAK sent", false)
		con.Mark("BREAK sent")
	case actSysrq:
		if err := cs.link.Break(); err != nil {
			con.Note("break: "+err.Error(), true)
			return
		}
		time.Sleep(sysrqGap)
		_ = cs.link.Send([]byte{a.arg})
		con.Note("SysRq "+string(a.arg)+" sent", false)
		con.Mark("SysRq " + string(a.arg) + " sent")
	case actBMC:
		t, ok := cs.link.(toggler)
		if !ok {
			msg := cs.noToggle
			if msg == "" {
				msg = "no BMC prompt on IPMI SOL"
			}
			con.Note(msg, false)
			return
		}
		kind, at, err := t.Toggle()
		con.SetKind(kind, at)
		if err != nil {
			con.Note("bmc prompt: "+err.Error(), true)
			return
		}
		con.Note("now at "+kind, false)
		con.Mark("switched to " + kind)
	case actLiteral:
		_ = cs.link.Send([]byte{escapeByte})
	case actPower:
		if cs.power == nil {
			return
		}
		con.Note("power cycling...", false)
		con.Mark("power cycle requested")
		guard(k.cleanup, func() {
			out, err := cs.power()
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
		_ = cs.link.Disconnect()
	case actHelp:
		con.Print(helpText())
	}
}
