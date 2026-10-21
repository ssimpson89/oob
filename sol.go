package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
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
	return s.Send([]byte(fmt.Sprintf("stty rows %d cols %d\r", rows, cols)))
}

func (s *solLink) SizeIsFree() bool { return false }

// powerCycleFn returns the function behind the console's p key: a separate
// ipmitool call, so it works while the SOL session stays up. Its text goes on
// the bar, so it is sanitized; a timeout says the result is unknown.
func powerCycleFn(name string, h Host) func() (string, error) {
	return func() (string, error) {
		out, err := ipmiOutput(h, consolePowerTimeout, "chassis", "power", "cycle")
		return ipmiPowerOutcome(name, out, err)
	}
}

// ipmiPowerOutcome shapes the result of a state-changing ipmitool power run
// whose output is shown on the bar.
func ipmiPowerOutcome(name, out string, err error) (string, error) {
	out = cleanRemoteLine(out)
	if err != nil {
		var te *timeoutErr
		if errors.As(err, &te) {
			te.unknown = true
			te.host = name
			return out, te
		}
		if out != "" {
			return out, fmt.Errorf("%w: %s", err, out)
		}
		return out, err
	}
	return out, nil
}

// solConsole runs SOL under a PTY with the bottom bar and returns ipmitool's
// exit code.
func solConsole(h Host, name string, power func() (string, error)) (int, error) {
	cmd, err := ipmiCmd(context.Background(), h, "-e", string([]byte{solEscape}), "sol", "activate")
	if err != nil {
		return 1, err
	}
	w, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 1, err
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(remoteRows(rows)), Cols: uint16(w)})
	if err != nil {
		return 1, fmt.Errorf("start ipmitool: %w", err)
	}
	defer ptmx.Close()

	done := make(chan result, 1)
	go func() {
		err := cmd.Wait()
		var ee *exec.ExitError
		switch {
		case err == nil:
			done <- result{}
		case errors.As(err, &ee):
			done <- result{code: exitCodeOf(ee)}
		default:
			done <- result{code: 1, err: err}
		}
	}()
	var tail tailBuf
	code, err := runConsole(consoleSetup{
		host: name, kind: "sol",
		link:       &solLink{ptmx: ptmx, cmd: cmd},
		out:        io.TeeReader(ptmx, &tail),
		done:       done,
		power:      power,
		connecting: "connecting to " + name + " via IPMI...",
	})
	// The terminal is restored now. Repeat any ipmitool error below the bar's
	// old row so it is still on screen after the console is torn down.
	if code != 0 {
		reportIpmiFailure(tail.String(), h)
	}
	return code, err
}
