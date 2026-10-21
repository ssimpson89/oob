package main

import (
	"io"
	"os"
	"sync/atomic"

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

// sshConsole is the interactive ssh session with the bottom bar.
func sshConsole(c *ssh.Client, name string, h Host) (int, error) {
	w, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 1, err
	}
	sess, stdin, stdout, err := startShell(c, remoteRows(rows), w)
	if err != nil {
		return 1, err
	}
	defer sess.Close()
	sl := &sshLink{sess: sess, stdin: stdin}
	var l link = sl
	disc := &sl.disconnected
	setup := consoleSetup{noToggle: "no BMC console known for this host (set its type to ilo, idrac or supermicro)"}
	// On a vendor host the session starts at the BMC's CLI prompt, so c can
	// start the console there and return to the prompt.
	if sp, ok := cliSpecFor(h.Type); ok {
		cl := newCLILink(sl, sp, true, true)
		l, disc = cl, &cl.disconnected
		setup.atPrompt = true
		sp.promptKind = "ssh"
		cl.sp = sp
	}
	done := make(chan result, 1)
	go func() {
		err := sess.Wait()
		if disc.Load() {
			done <- result{}
			return
		}
		code, err := exitStatus(err, true)
		done <- result{code: code, err: err}
	}()
	setup.host, setup.kind = name, "ssh"
	setup.link, setup.out, setup.done = l, newDetector(name, h).wrapReader(stdout), done
	setup.power = driverFor(name, h).ConsolePower()
	return runConsole(setup)
}
