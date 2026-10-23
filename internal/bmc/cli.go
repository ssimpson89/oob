package bmc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/detect"
	"github.com/ssimpson89/oob/internal/share"
	"github.com/ssimpson89/oob/internal/sshx"
	"golang.org/x/crypto/ssh"
)

// The generic SSH CLI console shared by the vendor drivers: an interactive
// shell on the BMC's SSH CLI (iLO, iDRAC, Supermicro SMASH-CLP), an expect-style
// session to read prompts, and the console link that moves between the BMC's
// CLI prompt and its serial console. Each vendor file supplies its cliSpec and
// parses its own responses.

const (
	cliPromptTimeout = 20 * time.Second
	cliCmdTimeout    = 20 * time.Second
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[()][A-Za-z0-9]|\x1b[=>78]`)

// cliSession is an expect-style helper over an interactive BMC CLI stream (iLO,
// iDRAC, Supermicro). After the console takes over it is also the reader for
// the remote's output.
type cliSession struct {
	w       io.Writer
	ch      chan []byte
	quit    chan struct{}
	rerr    error // set before ch is closed
	pending []byte
	onOut   func([]byte)   // sees everything read while expecting
	prompt  *regexp.Regexp // what ends a response
	label   string         // the vendor CLI, for messages ("iLO CLI")
	closer  func()
	once    sync.Once
}

func newCLISession(r io.Reader, w io.Writer, sp cliSpec) *cliSession {
	s := &cliSession{w: w, ch: make(chan []byte, 16), quit: make(chan struct{}), prompt: sp.prompt, label: sp.label}
	go func() {
		defer close(s.ch)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b := append([]byte(nil), buf[:n]...)
				select {
				case s.ch <- b:
				case <-s.quit:
					return
				}
			}
			if err != nil {
				s.rerr = err
				return
			}
		}
	}()
	return s
}

// Close stops the reader and closes the underlying connection.
func (s *cliSession) Close() {
	s.once.Do(func() {
		close(s.quit)
		if s.closer != nil {
			s.closer()
		}
	})
}

// Read hands the remote's output over after expect is done.
func (s *cliSession) Read(p []byte) (int, error) {
	if len(s.pending) == 0 {
		b, ok := <-s.ch
		if !ok {
			if s.rerr == nil || errors.Is(s.rerr, io.EOF) {
				return 0, io.EOF
			}
			return 0, s.rerr
		}
		s.pending = b
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// expectPrompt reads until the CLI prompt ends the text read so far and returns
// that text (including the prompt).
func (s *cliSession) expectPrompt(timeout time.Duration) (string, error) {
	var buf strings.Builder
	tm := time.NewTimer(timeout)
	defer tm.Stop()
	if len(s.pending) > 0 {
		buf.Write(s.pending)
		s.pending = nil
	}
	for {
		if s.prompt.MatchString(ansiRE.ReplaceAllString(buf.String(), "")) {
			return buf.String(), nil
		}
		select {
		case b, ok := <-s.ch:
			if !ok {
				if s.rerr != nil && !errors.Is(s.rerr, io.EOF) {
					return buf.String(), fmt.Errorf("%s connection lost: %w", s.label, s.rerr)
				}
				return buf.String(), fmt.Errorf("%s closed the connection before showing a prompt", s.label)
			}
			if s.onOut != nil {
				s.onOut(b)
			}
			buf.Write(b)
		case <-tm.C:
			return buf.String(), fmt.Errorf("timed out waiting for the %s prompt", s.label)
		}
	}
}

// awaitConsole is called after the command that starts the console. It reads
// until the console says it started (started), or the CLI prompt comes back,
// which means the console didn't start (on iLO: "Virtual Serial Port is
// already in use"). Without either within timeout it assumes the console is
// up. Everything read stays pending, so the console still shows it.
func (s *cliSession) awaitConsole(started *regexp.Regexp, timeout time.Duration) error {
	var buf []byte
	defer func() { s.pending = append(buf, s.pending...) }()
	tm := time.NewTimer(timeout)
	defer tm.Stop()
	for {
		text := ansiRE.ReplaceAllString(string(buf), "")
		if started.MatchString(text) {
			return nil
		}
		if s.prompt.MatchString(text) {
			return fmt.Errorf("the serial console didn't start: %s", firstLine(console.CleanRemote(text)))
		}
		select {
		case b, ok := <-s.ch:
			if !ok {
				return nil // the console's own read reports the closed connection
			}
			buf = append(buf, b...)
		case <-tm.C:
			return nil
		}
	}
}

var inUseRE = regexp.MustCompile(`(?i)(virtual serial port|serial port|console)[^\n]*(in use|busy)[^\n]*`)

// firstLine is the first non-empty line of s that isn't the start command's
// banner, trimmed, for an error message.
func firstLine(s string) string {
	// The BMC's own reason, when it gives one (output left over from an
	// earlier session can sit in front of it on the same line).
	if m := inUseRE.FindString(s); m != "" {
		return strings.TrimSpace(m)
	}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(strings.ToLower(l), "starting") && !strings.HasPrefix(strings.ToLower(l), "press") && !strings.HasPrefix(l, "vsp") {
			return l
		}
	}
	return "the BMC returned to its prompt"
}

// Send types s at the CLI.
func (s *cliSession) Send(str string) error {
	_, err := io.WriteString(s.w, str)
	return err
}

// startShell opens an interactive shell with a PTY. The caller owns sess.
func startShell(c *ssh.Client, rows, cols int) (*ssh.Session, io.WriteCloser, io.Reader, error) {
	sess, err := c.NewSession()
	if err != nil {
		return nil, nil, nil, err
	}
	sess.Stderr = os.Stderr
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, nil, nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, nil, nil, err
	}
	termName := os.Getenv("TERM")
	if termName == "" {
		termName = "xterm-256color"
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	if err := sess.RequestPty(termName, rows, cols, modes); err != nil {
		sess.Close()
		return nil, nil, nil, errors.New("request pty: " + err.Error())
	}
	if err := sess.Shell(); err != nil {
		sess.Close()
		return nil, nil, nil, err
	}
	return sess, stdin, stdout, nil
}

// cliDial connects, opens an interactive CLI and waits for the first prompt
// (matched by sp.prompt). The returned session owns the SSH client.
// timeout (0: the default) limits the connect and noPrompt keeps the login from
// asking for a passphrase, for runs without a user at the keyboard.
func cliDial(name string, h config.Host, sp cliSpec, rows, cols int, timeout time.Duration, noPrompt bool) (*cliSession, *ssh.Session, io.WriteCloser, error) {
	kh, err := sshx.KnownHostsPath()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := sshx.CheckCreds(name, h); err != nil {
		return nil, nil, nil, err
	}
	det := detect.NewDetector(name, h)
	cl, err := sshx.ConnectWith(h, kh, sshx.Options{Banner: det.Banner, Timeout: timeout, NoPrompt: noPrompt})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s (%s): %w", name, h.IP, sshx.NameConnErr(err, name))
	}
	sess, stdin, stdout, err := startShell(cl, rows, cols)
	if err != nil {
		cl.Close()
		return nil, nil, nil, fmt.Errorf("%s (%s): %w", name, h.IP, err)
	}
	s := newCLISession(stdout, stdin, sp)
	s.onOut = det.Feed
	s.closer = func() { sess.Close(); cl.Close() }
	if _, err := s.expectPrompt(cliPromptTimeout); err != nil {
		s.Close()
		return nil, nil, nil, fmt.Errorf("%s (%s): %w", name, h.IP, err)
	}
	return s, sess, stdin, nil
}

// cliLink drives a vendor's serial console inside an interactive SSH CLI
// session (iLO VSP, iDRAC "console com2", Supermicro SMASH SOL). It also tracks
// whether the session is at the console or at the BMC's own CLI prompt, and
// the c key toggles between the two without dropping the connection.
//
// The state is a best guess: the link knows what it last sent. If the user
// types the escape or "vsp" by hand it can drift. For iLO and iDRAC, whose
// prompts are unambiguous, a prompt seen in the output moves it back to
// "at prompt" (Observe). Supermicro's prompt pattern (">", "#") would also
// match a Linux shell on the host, so there it is only used to wait.
type cliLink struct {
	*sshLink
	sp    cliSpec
	plain bool // "oob ssh": window size and BREAK behave as on a plain ssh session
	// closeSess ends the SSH session (the fallback if exit doesn't hang up).
	closeSess func() error

	mu       sync.Mutex
	atPrompt bool
	tail     []byte
	seen     chan struct{} // signalled when the prompt pattern matches the output
	alt      atomic.Bool   // the remote is in a full-screen program (see SetAltScreen)
}

func newCLILink(sl *sshLink, sp cliSpec, atPrompt, plain bool) *cliLink {
	l := &cliLink{sshLink: sl, sp: sp, atPrompt: atPrompt, plain: plain, seen: make(chan struct{}, 1)}
	l.closeSess = func() error { return l.sess.Close() }
	return l
}

// These consoles have no BREAK; the console shows the note on the bar.
func (l *cliLink) Break() error {
	if l.plain {
		return l.sshLink.Break()
	}
	return errors.New(l.sp.breakMsg)
}

// Disconnect leaves the console if it is in it (the console's escape), then
// sends "exit" to log out. At the CLI prompt it only sends "exit".
// disconnectPromptWait is how long Disconnect waits for the BMC prompt after
// the console escape before giving up and hanging up.
var disconnectPromptWait = 3 * time.Second

func (l *cliLink) Disconnect() error {
	l.disconnected.Store(true)
	l.mu.Lock()
	at := l.atPrompt
	l.mu.Unlock()
	if !at {
		// Only type "exit" once the BMC's own prompt is back. If the escape
		// didn't take, "exit" would land on the server's console instead (at
		// a login prompt it becomes a username), so just hang up.
		l.expect()
		if l.Send(l.sp.esc) != nil || !l.waitPrompt(disconnectPromptWait) {
			return l.closeSess()
		}
	}
	if l.Send([]byte("exit\r")) != nil {
		return l.closeSess()
	}
	// If the BMC doesn't hang up, don't leave the user stuck.
	time.AfterFunc(2*time.Second, func() { _ = l.closeSess() })
	return nil
}

// Hangup closes the session without sending the console escape or "exit".
func (l *cliLink) Hangup() error {
	l.disconnected.Store(true)
	return l.closeSess()
}

// Like SOL, the remote shell only learns the size if it is typed at it.
func (l *cliLink) FitSize(rows, cols int) error {
	if l.plain {
		return l.sshLink.FitSize(rows, cols)
	}
	return l.Send([]byte(sttyLine(rows, cols) + "\r"))
}

// SizeLine is the command FitSize types, when it types one: the console
// sends it through its typing queue instead of calling FitSize. A plain ssh
// session sends a window change and types nothing.
func (l *cliLink) SizeLine(rows, cols int) (string, bool) {
	if l.plain {
		return "", false
	}
	return sttyLine(rows, cols), true
}

// Serial reports whether what is typed goes to a serial console (iLO VSP,
// iDRAC com2, Supermicro SOL), where a paste has to be paced. At the BMC's own
// CLI prompt it is an ssh session with flow control, so it isn't.
func (l *cliLink) Serial() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.atPrompt
}

// MapInput fits what is typed to a console that reads ESC as a two-byte
// escape (iLO VSP), in the console only: at the BMC's own prompt, on a plain
// ssh session and on every other vendor the bytes go through as they are.
// The console's own escape (Toggle, Disconnect) goes through Send, not here,
// so it is the only way ESC ( is ever sent.
func (l *cliLink) MapInput(b []byte) ([]byte, string) {
	if !l.sp.vspEsc || l.plain {
		return b, ""
	}
	l.mu.Lock()
	at := l.atPrompt
	l.mu.Unlock()
	if at {
		return b, ""
	}
	return console.MapVSPInput(b, l.alt.Load())
}

// SetAltScreen records whether the remote is in a full-screen program, where
// the VSP can't be given arrow keys. See MapInput.
func (l *cliLink) SetAltScreen(on bool) { l.alt.Store(on) }

func (l *cliLink) SizeIsFree() bool { return l.plain }

// stateLocked returns the bar label for where the session is now. The caller
// holds l.mu.
func (l *cliLink) stateLocked() (kind string, atPrompt bool) {
	if l.atPrompt {
		return l.sp.promptKind, true
	}
	return l.sp.kind, false
}

// Observe sees the remote's output. It remembers the tail so it can tell when
// the prompt has appeared. The two directions of a resync are not symmetric:
// a prompt in the output moves the state to "at prompt" only when the spec's
// resync flag says its prompt is unambiguous (iLO, iDRAC), and output matching
// spec.entered moves it back to "in console" only for a console that announces
// itself (iLO's "Virtual Serial Port Active", typed by hand or by us). iDRAC
// and Supermicro print nothing recognizable when the console starts, so a
// session that drifted there stays "at prompt" until the c key is used.
func (l *cliLink) Observe(b []byte) (kind string, atPrompt, changed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tail = append(l.tail, b...)
	if len(l.tail) > 256 {
		l.tail = append([]byte(nil), l.tail[len(l.tail)-256:]...)
	}
	if l.sp.prompt != nil && l.sp.prompt.MatchString(ansiRE.ReplaceAllString(string(l.tail), "")) {
		select {
		case l.seen <- struct{}{}:
		default:
		}
		if l.sp.resync && !l.atPrompt {
			l.atPrompt, changed = true, true
		}
	} else if l.sp.entered != nil && l.atPrompt && l.sp.entered.MatchString(ansiRE.ReplaceAllString(string(l.tail), "")) {
		l.atPrompt, changed = false, true
		l.tail = nil
	}
	kind, atPrompt = l.stateLocked()
	return kind, atPrompt, changed
}

// expect forgets earlier output; waitPrompt then waits for a new prompt.
func (l *cliLink) expect() {
	l.mu.Lock()
	l.tail = nil
	l.mu.Unlock()
	select {
	case <-l.seen:
	default:
	}
}

func (l *cliLink) waitPrompt(d time.Duration) bool {
	select {
	case <-l.seen:
		return true
	case <-time.After(d):
		return false
	}
}

// Toggle switches between the console and the BMC's CLI prompt and returns the
// new bar label. Going to the prompt waits for the prompt to show, and stays
// in the console state if it doesn't.
func (l *cliLink) Toggle() (string, bool, error) {
	l.mu.Lock()
	at := l.atPrompt
	l.mu.Unlock()
	if !at {
		l.expect()
		if err := l.Send(l.sp.esc); err != nil {
			return l.sp.kind, false, err
		}
		if !l.waitPrompt(3 * time.Second) {
			return l.sp.kind, false, errors.New("no prompt after the console escape; still in the console")
		}
	} else {
		for i, c := range l.sp.start {
			l.expect()
			if err := l.Send([]byte(c + "\r")); err != nil {
				return l.sp.promptKind, true, err
			}
			if i < len(l.sp.start)-1 {
				l.waitPrompt(2 * time.Second) // best effort; go on regardless
			}
		}
	}
	l.mu.Lock()
	l.atPrompt = !at
	kind, p := l.stateLocked()
	l.mu.Unlock()
	return kind, p, nil
}

// cliSpec describes how to reach a serial console through a BMC's SSH CLI.
type cliSpec struct {
	kind       string         // bar label while in the console
	promptKind string         // bar label while at the CLI prompt
	prompt     *regexp.Regexp // CLI prompt
	resync     bool           // a match of prompt in the output means "at the prompt" for sure
	entered    *regexp.Regexp // optional: output that means the console just started (e.g. `vsp` typed by hand)
	start      []string       // commands typed at the prompt; the last one starts the console
	esc        []byte         // console escape sequence: console -> CLI prompt
	breakMsg   string
	// vspEsc: the console reads ESC with the byte after it (iLO VSP), so input
	// is mapped. See MapInput.
	vspEsc bool
	label  string // for the progress line
}

// consoleStartWait is how long cliOpen waits for a console to say it started.
var consoleStartWait = 5 * time.Second

// cliOpen opens the CLI over SSH, starts the console and returns it as a
// console transport. It needs no terminal of its own: rows and cols are the
// size the remote is told. Both the console bar and a headless front end use it.
func cliOpen(name string, h config.Host, sp cliSpec, rows, cols int) (*ConsoleTransport, error) {
	s, sess, stdin, err := cliDial(name, h, sp, rows, cols, 0, false)
	if err != nil {
		return nil, err
	}
	for i, c := range sp.start {
		if err := s.Send(c + "\r"); err != nil {
			s.Close()
			return nil, err
		}
		if i < len(sp.start)-1 {
			if _, err := s.expectPrompt(cliCmdTimeout); err != nil {
				s.Close()
				return nil, err
			}
		}
	}
	// A console that announces itself (iLO VSP) can be refused, for example
	// when another session already holds it; the BMC then shows its prompt
	// again. Fail clearly instead of leaving the user at the BMC prompt.
	if sp.entered != nil {
		if err := s.awaitConsole(sp.entered, consoleStartWait); err != nil {
			s.Close()
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	l := newCLILink(&sshLink{sess: sess, stdin: stdin}, sp, false, false)
	done := make(chan console.Result, 1)
	go func() {
		err := sess.Wait()
		if l.disconnected.Load() {
			done <- console.Result{}
			return
		}
		code, err := sshx.ExitStatus(err, true)
		done <- console.Result{Code: code, Err: err}
	}()
	return &ConsoleTransport{
		Kind: sp.kind, Link: l, Out: newSkipEcho(s, sp.start[len(sp.start)-1]), Done: done,
		Toggle: l.Toggle, Observe: l.Observe, Close: s.Close,
		Pacing: console.PacingMS(h.CharDelayMS, h.LineDelayMS),
	}, nil
}

// cliConsole opens the CLI, starts the console and hands over to the bar. The
// console is shared: if another oob already has it open (an iLO allows one VSP
// session, so a second connection couldn't work), this joins it instead.
func cliConsole(name string, h config.Host, sp cliSpec, power func() (string, error)) (int, error) {
	return share.RunConsole(h, name, share.Console{
		Power: power,
		Open: func(rows, cols int) (*ConsoleTransport, error) {
			app.Progressf("connecting to %s via SSH (%s)...", name, sp.label)
			return cliOpen(name, h, sp, rows, cols)
		},
	})
}

// cliSpecFor is the console spec for a host type, for toggling inside oob ssh.
func cliSpecFor(typ string) (cliSpec, bool) {
	switch typ {
	case detect.TypeILO:
		return iloSpec, true
	case detect.TypeIDRAC:
		return idracSpec, true
	case detect.TypeSupermicro:
		return smcSpec, true
	}
	return cliSpec{}, false
}

// skipEcho hides the remote's echo of the command that started the console.
// That command is typed before the console screen is set up, so its echo
// ("vsp" and a line break) would arrive after the bar is drawn and land where
// the cursor happens to be, next to or over the "connecting" line. Only a
// leading echo is removed; output that does not start with the command is
// passed through untouched.
type skipEcho struct {
	r     io.Reader
	cmd   []byte
	state int // 0 deciding, 1 eating the line break after the echo, 2 done
	buf   []byte
}

func newSkipEcho(r io.Reader, cmd string) *skipEcho {
	return &skipEcho{r: r, cmd: []byte(cmd)}
}

func (e *skipEcho) Read(p []byte) (int, error) {
	tmp := make([]byte, len(p))
	for {
		if len(e.buf) > 0 && e.state != 0 {
			n := copy(p, e.buf)
			e.buf = e.buf[n:]
			return n, nil
		}
		if e.state == 2 {
			return e.r.Read(p)
		}
		n, err := e.r.Read(tmp)
		e.buf = append(e.buf, tmp[:n]...)
		switch e.state {
		case 0:
			switch {
			case bytes.HasPrefix(e.buf, e.cmd):
				e.buf = e.buf[len(e.cmd):]
				e.state = 1
			case bytes.HasPrefix(e.cmd, e.buf):
				// still a possible echo: wait for more
				if err != nil {
					e.state = 2
				}
				continue
			default:
				e.state = 2
			}
			fallthrough
		case 1:
			if e.state == 1 {
				e.buf = bytes.TrimLeft(e.buf, "\r\n")
				if len(e.buf) > 0 {
					e.state = 2
				}
			}
		}
		if len(e.buf) == 0 && err != nil {
			return 0, err
		}
	}
}
