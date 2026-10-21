package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // known_hosts hashed names are HMAC-SHA1
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

const connectTimeout = 15 * time.Second

var passwordPrompt = regexp.MustCompile(`(?i)password`)

// legacyConfig prefers modern algorithms but appends the insecure ones that older
// BMC firmware still requires.
func legacyConfig() ssh.Config {
	s, i := ssh.SupportedAlgorithms(), ssh.InsecureAlgorithms()
	return ssh.Config{
		KeyExchanges: append(s.KeyExchanges, i.KeyExchanges...),
		Ciphers:      append(s.Ciphers, i.Ciphers...),
		MACs:         append(s.MACs, i.MACs...),
	}
}

func legacyHostKeys() []string {
	return append(ssh.SupportedAlgorithms().HostKeys, ssh.InsecureAlgorithms().HostKeys...)
}

// hostKeyChangedError is a stored host key that the BMC no longer presents.
type hostKeyChangedError struct {
	addr string
	name string // the oob host name, when the caller knows it
}

func (e *hostKeyChangedError) Error() string {
	n := e.name
	if n == "" {
		n = "<name>"
	}
	return fmt.Sprintf("the SSH host key of %s differs from the one in known_hosts. If the BMC was reimaged, run `%s ssh %s --reset-hostkey`; otherwise someone may be impersonating the BMC", e.addr, AppName, n)
}

// nameConnErr puts the oob host name into a host key error's advice, so the
// command it suggests can be pasted. Other errors pass through. (The message of
// an error wrapped by the ssh package is fixed when it is built, so this makes
// a new error that still unwraps to the original.)
func nameConnErr(err error, name string) error {
	var he *hostKeyChangedError
	if errors.As(err, &he) && he.name == "" && name != "" {
		return &cleanedErr{msg: strings.ReplaceAll(err.Error(), "ssh <name> --reset-hostkey", "ssh "+name+" --reset-hostkey"), err: err}
	}
	return err
}

// sshHostKeyAlgs maps the key types stored for a host to the HostKeyAlgorithms
// to offer: only those, so a BMC cannot downgrade us to a key type we have
// never seen. An RSA key may be signed with any of the three RSA algorithms.
func sshHostKeyAlgs(types []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, t := range types {
		if t == ssh.KeyAlgoRSA {
			add(ssh.KeyAlgoRSASHA512)
			add(ssh.KeyAlgoRSASHA256)
			add(ssh.KeyAlgoRSA)
			continue
		}
		add(t)
	}
	return out
}

// storedKeyTypes returns the key types in known_hosts for addr (host:port).
// Hashed names are matched too. Wildcard patterns and markers (@cert-authority,
// @revoked) are ignored here; a mismatch against them still fails in the callback.
func storedKeyTypes(khPath, addr string) []string {
	f, err := os.Open(khPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	norm := knownhosts.Normalize(addr)
	var types []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "@") {
			continue
		}
		_, hosts, key, _, _, err := ssh.ParseKnownHosts([]byte(line))
		if err != nil {
			continue
		}
		for _, h := range hosts {
			if hostMatches(h, norm) {
				types = append(types, key.Type())
				break
			}
		}
	}
	return types
}

func hostMatches(pattern, norm string) bool {
	if rest, ok := strings.CutPrefix(pattern, "|1|"); ok {
		salt, hash, ok := strings.Cut(rest, "|")
		if !ok {
			return false
		}
		sb, err1 := base64.StdEncoding.DecodeString(salt)
		hb, err2 := base64.StdEncoding.DecodeString(hash)
		if err1 != nil || err2 != nil {
			return false
		}
		m := hmac.New(sha1.New, sb)
		m.Write([]byte(norm))
		return hmac.Equal(m.Sum(nil), hb)
	}
	return pattern == norm
}

// acceptNew implements accept-new semantics against our own known_hosts file:
// an unknown host is recorded, a known host must present a stored key. Any
// mismatch is an error, including a key of another type than the stored one:
// sshHostKeyAlgs already keeps a known host to its stored types, so seeing
// anything else means the key changed.
func acceptNew(khPath string) (ssh.HostKeyCallback, error) {
	if err := os.MkdirAll(filepath.Dir(khPath), 0o700); err != nil {
		return nil, err
	}
	if f, err := os.OpenFile(khPath, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
		f.Close()
	} else {
		return nil, err
	}
	cb, err := knownhosts.New(khPath)
	if err != nil {
		return nil, err
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)
		var ke *knownhosts.KeyError
		if err == nil || !errors.As(err, &ke) {
			return err
		}
		if len(ke.Want) > 0 {
			return &hostKeyChangedError{addr: hostname}
		}
		f, err := os.OpenFile(khPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(f, knownhosts.Line([]string{hostname}, key)); err != nil {
			f.Close()
			return fmt.Errorf("write %s: %w", khPath, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("write %s: %w", khPath, err)
		}
		fmt.Fprintf(os.Stderr, "Added %s host key %s to %s\n", key.Type(), ssh.FingerprintSHA256(key), khPath)
		return nil
	}, nil
}

// resetHostKey removes known_hosts lines for addr (host:port) and the Redfish
// TLS pins of the same host (they live in redfish_pins next to khPath), so one
// --reset-hostkey covers both after a BMC reimage.
func resetHostKey(khPath, addr string) error {
	if err := resetKnownHosts(khPath, addr); err != nil {
		return err
	}
	return resetHostPins(khPath, addr)
}

func resetKnownHosts(khPath, addr string) error {
	f, err := os.Open(khPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	norm := knownhosts.Normalize(addr)
	var keep []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		drop := false
		if len(fields) > 0 {
			for _, h := range strings.Split(fields[0], ",") {
				if hostMatches(h, norm) {
					drop = true
				}
			}
		}
		if !drop {
			keep = append(keep, sc.Text())
		}
	}
	f.Close()
	if err := sc.Err(); err != nil {
		return err
	}
	data := strings.Join(keep, "\n")
	if len(keep) > 0 {
		data += "\n"
	}
	return writeFileAtomic(khPath, []byte(data))
}

// knownHostsPath is oob's own known_hosts file, next to the config.
func knownHostsPath() (string, error) {
	p, err := cfgPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "known_hosts"), nil
}

// sshConnect dials the BMC with ssh-agent or key auth, then password and
// keyboard-interactive auth (see sshAuthMethods).
func sshConnect(h Host, khPath string) (*ssh.Client, error) {
	return sshConnectOpts(h, khPath, nil)
}

// sshConnectOpts is sshConnect with a callback for the server's pre-auth banner.
func sshConnectOpts(h Host, khPath string, banner func(string)) (*ssh.Client, error) {
	cl, err := sshConnectRaw(h, khPath, banner)
	return cl, cleanErr(err)
}

func sshConnectRaw(h Host, khPath string, banner func(string)) (*ssh.Client, error) {
	cb, err := acceptNew(khPath)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(h.IP, strconv.Itoa(h.Port))
	// A host with stored keys is offered only those key types; an unknown host
	// gets the full list and is accepted on first use.
	hostKeys := legacyHostKeys()
	if stored := storedKeyTypes(khPath, addr); len(stored) > 0 {
		hostKeys = sshHostKeyAlgs(stored)
	}
	auth, closeAuth, err := sshAuthMethods(h)
	if err != nil {
		return nil, err
	}
	defer closeAuth()
	cfg := &ssh.ClientConfig{
		Config:            legacyConfig(),
		User:              h.User,
		Auth:              auth,
		HostKeyCallback:   cb,
		HostKeyAlgorithms: hostKeys,
	}
	// The pre-auth banner is untrusted until the login works, so it is held and
	// handed over only after authentication succeeded.
	var banners []string
	if banner != nil {
		cfg.BannerCallback = func(msg string) error { banners = append(banners, msg); return nil }
	}
	// The timeout covers TCP connect and the SSH handshake, not just the dial.
	who := h.Name
	if who == "" {
		who = h.IP
	}
	started := time.Now()
	slow := func(err error) error {
		if isTimeout(err) {
			return &timeoutErr{host: who, via: "SSH", after: time.Since(started), cause: err}
		}
		return err
	}
	conn, err := net.DialTimeout("tcp", addr, connectTimeout)
	if err != nil {
		return nil, slow(err)
	}
	if err := conn.SetDeadline(time.Now().Add(connectTimeout)); err != nil {
		conn.Close()
		return nil, err
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, slow(err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		sc.Close()
		return nil, err
	}
	for _, b := range banners {
		banner(b)
	}
	return ssh.NewClient(sc, chans, reqs), nil
}

// restoreOnSignal restores the terminal and exits with 128+signal if one of sigs
// arrives (raw mode and no-echo would otherwise be left on). If cleanup is not
// nil it runs first (the console uses it to reset the scroll region and clear
// the bar). Call the returned func to stop watching.
func restoreOnSignal(fd int, st *term.State, cleanup func(), sigs ...os.Signal) func() {
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

// remoteCommand joins args into one remote command line. Multiple args are each
// shell-quoted so they survive the remote shell intact; a single arg is sent
// as-is, since it is already a full command line (e.g. "show system1").
func remoteCommand(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote quotes s for a POSIX shell, leaving plain words untouched.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sshSession runs an interactive shell (no cmd) with a PTY, or cmd without one.
// It returns the remote exit status.
func sshSession(c *ssh.Client, cmd, name string, h Host) (int, error) {
	if cmd == "" && useBar() {
		return sshConsole(c, name, h)
	}
	sess, err := c.NewSession()
	if err != nil {
		return 1, err
	}
	defer sess.Close()
	det := newDetector(name, h)
	sess.Stderr = os.Stderr
	sess.Stdout = det.wrapWriter(os.Stdout)
	sess.Stdin = os.Stdin

	fd := int(os.Stdin.Fd())
	interactive := cmd == ""
	if interactive {
		if !term.IsTerminal(fd) {
			return 1, errors.New("stdin is not a terminal; pass a command to run instead")
		}
		w, hgt, err := term.GetSize(fd)
		if err != nil {
			w, hgt = 80, 24
		}
		termName := os.Getenv("TERM")
		if termName == "" {
			termName = "xterm-256color"
		}
		modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
		if err := sess.RequestPty(termName, hgt, w, modes); err != nil {
			return 1, fmt.Errorf("request pty: %w", err)
		}
		old, err := term.MakeRaw(fd)
		if err != nil {
			return 1, err
		}
		defer term.Restore(fd, old)
		defer restoreOnSignal(fd, old, nil, syscall.SIGTERM, syscall.SIGHUP)()

		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				if w, h, err := term.GetSize(fd); err == nil {
					_ = sess.WindowChange(h, w)
				}
			}
		}()
		err = sess.Shell()
		if err == nil {
			err = sess.Wait()
		}
		return exitStatus(err, true)
	}
	return exitStatus(sess.Run(cmd), false)
}

// exitStatus maps a session error to an exit code. A server that closes without
// a status is common on BMCs: fine for a shell, but a failure for a command.
func exitStatus(err error, interactive bool) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus(), nil
	}
	var em *ssh.ExitMissingError
	if errors.As(err, &em) {
		if interactive {
			return 0, nil
		}
		return 255, nil
	}
	return 1, err
}
