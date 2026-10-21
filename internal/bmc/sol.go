package bmc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/share"
	"golang.org/x/term"
)

// solEscape is the SOL escape character handed to ipmitool with -e. ipmitool
// documents `-e <sol_escape_char>` (default ~). A control character that
// nobody types after a newline means a stray "~." in a log paste or a
// remote shell can't end the session. The menu sends the real sequences.
const solEscape byte = 0x1e // ctrl+^

// solLink drives an `ipmitool sol activate` running under a PTY.
type solLink struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

func (s *solLink) Send(b []byte) error { _, err := s.ptmx.Write(b); return err }

// ipmitool only recognises its escape right after a newline, so each escape
// sequence starts with a CR (which the remote console sees as Enter).
func (s *solLink) escape(c byte) error { return s.Send([]byte{'\r', solEscape, c}) }

func (s *solLink) Break() error { return s.escape('B') }

func (s *solLink) Disconnect() error {
	err := s.escape('.')
	// If ipmitool doesn't take the hint, don't leave the user stuck.
	time.AfterFunc(3*time.Second, func() {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
	return err
}

func (s *solLink) Resize(rows, cols int) error {
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

func (s *solLink) FitSize(rows, cols int) error {
	line, _ := s.SizeLine(rows, cols)
	return s.Send([]byte(line + "\r"))
}

// SizeLine is the command FitSize types. The console sends it through its
// typing queue instead of calling FitSize.
func (s *solLink) SizeLine(rows, cols int) (string, bool) {
	return sttyLine(rows, cols), true
}

// Serial reports that SOL is a serial line: a paste has to be paced.
func (s *solLink) Serial() bool { return true }

// sttyLine is the command that tells a remote shell its window size.
func sttyLine(rows, cols int) string { return fmt.Sprintf("stty rows %d cols %d", rows, cols) }

func (s *solLink) SizeIsFree() bool { return false }

// powerCycleFn returns the function behind the console's p key: a separate
// ipmitool call, so it works while the SOL session stays up. Its text goes on
// the bar, so it is sanitized; a timeout says the result is unknown.
func powerCycleFn(name string, h config.Host) func() (string, error) {
	return func() (string, error) {
		out, err := ipmiOutput(h, consolePowerTimeout, "chassis", "power", "cycle")
		return ipmiPowerOutcome(name, out, err)
	}
}

// ipmiPowerOutcome shapes the result of a state-changing ipmitool power run
// whose output is shown on the bar.
func ipmiPowerOutcome(name, out string, err error) (string, error) {
	out = console.CleanRemoteLine(out)
	if err != nil {
		var te *console.TimeoutError
		if errors.As(err, &te) {
			te.Unknown = true
			te.Host = name
			return out, te
		}
		if out != "" {
			return out, fmt.Errorf("%w: %s", err, out)
		}
		return out, err
	}
	return out, nil
}

// openSOL starts `ipmitool sol activate` under a PTY of the given size and
// returns it as a console transport. It needs no terminal of its own, so both
// the console bar and a headless front end use it.
func openSOL(h config.Host, rows, cols int) (*ConsoleTransport, error) {
	cmd, err := ipmiCmd(context.Background(), h, "-e", string([]byte{solEscape}), "sol", "activate")
	if err != nil {
		return nil, err
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, fmt.Errorf("start ipmitool: %w", err)
	}
	done := make(chan console.Result, 1)
	// refused is set when ipmitool itself gave up with a non-zero exit status
	// (for instance "SOL payload already active": someone else holds the
	// session). This transport then never owned a session, and Release must not
	// deactivate the other holder's. A kill by Close shows as a signal, not a
	// status, so it doesn't count.
	var refused atomic.Bool
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		err := cmd.Wait()
		var ee *exec.ExitError
		switch {
		case err == nil:
			done <- console.Result{}
		case errors.As(err, &ee):
			if ee.ExitCode() > 0 {
				refused.Store(true)
			}
			done <- console.Result{Code: exitCodeOf(ee)}
		default:
			done <- console.Result{Code: 1, Err: err}
		}
	}()
	tail := &tailBuf{}
	return &ConsoleTransport{
		Kind:   "sol",
		Pacing: console.PacingMS(h.CharDelayMS, h.LineDelayMS),
		Link:   &solLink{ptmx: ptmx, cmd: cmd},
		Out:    io.TeeReader(ptmx, tail),
		Done:   done,
		Tail:   tail.String,
		Close: func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = ptmx.Close()
		},
		Release: func() {
			// Close has killed the client by now; let the waiter record how it
			// ended before deciding whether the session was ours.
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
			}
			if refused.Load() {
				return
			}
			_, _ = ipmiOutput(h, ipmiQuickTimeout, "sol", "deactivate")
		},
	}, nil
}

// solConsole runs SOL under a PTY with the bottom bar and returns ipmitool's
// exit code. The console is shared: if another oob (an MCP server, or another
// window) already has it, this joins it; otherwise this opens it and others
// can join.
func solConsole(h config.Host, name string, power func() (string, error)) (int, error) {
	w, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 1, err
	}
	c, err := share.Open(context.Background(), share.Options{
		Host: name, NoLinger: true,
		Open: func() (*console.Transport, error) { return openSOL(h, console.RemoteRows(rows), w) },
	})
	if err != nil {
		return 1, err
	}
	defer c.Close()
	connecting := "connecting to " + name + " via IPMI..."
	if c.Joined() {
		connecting = ""
	}
	code, err := share.RunWindow(c, name, power, connecting)
	// The terminal is restored now. Repeat any ipmitool error below the bar's
	// old row so it is still on screen after the console is torn down.
	if code != 0 && c.Hosting() {
		reportIpmiFailure(c.Tail(), h)
	}
	return code, err
}
