package bmc

import (
	"io"
	"os"
	"sync/atomic"

	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/detect"
	"github.com/ssimpson89/oob/internal/sshx"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// sshLink drives an interactive ssh session.
type sshLink struct {
	sess         *ssh.Session
	stdin        io.WriteCloser
	disconnected atomic.Bool
}

func (s *sshLink) Send(b []byte) error { _, err := s.stdin.Write(b); return err }

// breakMillis is the length of the BREAK sent over ssh, long enough for a
// serial device to see it.
const breakMillis = 500

// Break sends the RFC 4335 "break" channel request.
func (s *sshLink) Break() error {
	_, err := s.sess.SendRequest("break", false, ssh.Marshal(struct{ Millis uint32 }{breakMillis}))
	return err
}

func (s *sshLink) Disconnect() error {
	s.disconnected.Store(true)
	return s.sess.Close()
}

func (s *sshLink) Resize(rows, cols int) error  { return s.sess.WindowChange(rows, cols) }
func (s *sshLink) FitSize(rows, cols int) error { return s.sess.WindowChange(rows, cols) }

// A window change types nothing at the remote, so following it is always safe.
func (s *sshLink) SizeIsFree() bool { return true }

// SSHConsole is the interactive ssh session with the bottom bar.
func SSHConsole(c *ssh.Client, name string, h config.Host) (int, error) {
	w, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 1, err
	}
	sess, stdin, stdout, err := startShell(c, console.RemoteRows(rows), w)
	if err != nil {
		return 1, err
	}
	defer sess.Close()
	sl := &sshLink{sess: sess, stdin: stdin}
	var l console.Link = sl
	disc := &sl.disconnected
	setup := console.Setup{Pacing: console.PacingMS(h.CharDelayMS, h.LineDelayMS), NoToggle: "no BMC console known for this host (set its type to ilo, idrac or supermicro)"}
	// On a vendor host the session starts at the BMC's CLI prompt, so c can
	// start the console there and return to the prompt.
	if sp, ok := cliSpecFor(h.Type); ok {
		cl := newCLILink(sl, sp, true, true)
		l, disc = cl, &cl.disconnected
		setup.AtPrompt = true
		sp.promptKind = "ssh"
		cl.sp = sp
	}
	done := make(chan console.Result, 1)
	go func() {
		err := sess.Wait()
		if disc.Load() {
			done <- console.Result{}
			return
		}
		code, err := sshx.ExitStatus(err, true)
		done <- console.Result{Code: code, Err: err}
	}()
	setup.Host, setup.Kind = name, "ssh"
	setup.Link, setup.Out, setup.Done = l, detect.NewDetector(name, h).WrapReader(stdout), done
	setup.Power = DriverFor(name, h).ConsolePower()
	return console.Run(setup)
}
