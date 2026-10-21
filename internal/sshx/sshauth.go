package sshx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

// SSH authentication. The order is:
//
//   - a host with a `key` file offers only that key (the agent is not used, so
//     a full agent cannot burn through the BMC's limited login attempts),
//     then the password if one is set;
//   - otherwise the ssh-agent's keys (SSH_AUTH_SOCK, unless OOB_NO_AGENT=1),
//     then the password if one is set.
//
// Password and keyboard-interactive are always last, so a BMC that rejects
// keys still gets the password.

// agentDisabled reports whether OOB_NO_AGENT turns the ssh-agent off.
func agentDisabled() bool {
	v := os.Getenv("OOB_NO_AGENT")
	return v != "" && v != "0"
}

// dialAgent connects to the ssh-agent, or returns nil if there is none.
func dialAgent() net.Conn {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" || agentDisabled() {
		return nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil
	}
	return conn
}

// agentUsable is true when an ssh-agent is reachable.
func AgentUsable() bool {
	c := dialAgent()
	if c == nil {
		return false
	}
	c.Close()
	return true
}

// checkSSHCreds fails fast when SSH has a user but nothing to log in with.
// It is for the SSH-only paths: IPMI and Redfish use checkCreds.
func CheckCreds(who string, h config.Host) error {
	if h.User == "" {
		return config.NoUserError(who)
	}
	if h.Pass == "" && h.Key == "" && !AgentUsable() {
		return fmt.Errorf("%s: no way to log in over SSH: set a password, a key (oob add --key) or load a key into ssh-agent", who)
	}
	return nil
}

// expandHome expands a leading ~ or ~/ in a path.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

// keyPassphrase asks for a key's passphrase. It is a variable so tests can
// replace it.
var keyPassphrase = func(path string) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, fmt.Errorf("key %s is protected by a passphrase and there is no terminal to ask on: load it into ssh-agent (ssh-add) and remove the key setting, or use an unprotected key", path)
	}
	fmt.Fprintf(os.Stderr, "Passphrase for %s: ", path)
	stop := func() {}
	if st, err := term.GetState(fd); err == nil {
		stop = console.RestoreOnSignal(fd, st, nil, syscall.SIGINT)
	}
	b, err := term.ReadPassword(fd)
	stop()
	fmt.Fprintln(os.Stderr)
	return b, err
}

// loadKeySigner reads a private key file. It refuses a file that group or
// other can read (as OpenSSH does) and asks once for a passphrase if needed.
// Nothing about the key is ever printed.
func loadKeySigner(path string) (ssh.Signer, error) {
	p, err := expandHome(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("ssh key: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("ssh key %s is not a regular file", p)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("ssh key %s has mode %04o, readable by group or others; refusing to use it (fix with: chmod 600 %s)", p, st.Mode().Perm(), p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("ssh key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		pw, perr := keyPassphrase(p)
		if perr != nil {
			return nil, perr
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(b, pw)
		clear(pw)
		if err != nil {
			return nil, fmt.Errorf("ssh key %s: wrong passphrase or unusable key", p)
		}
		return signer, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ssh key %s: not a usable private key", p)
	}
	return signer, nil
}

// sshAuthMethods builds the auth methods for h in the order described above.
// The returned func releases the agent connection.
func sshAuthMethods(h config.Host) ([]ssh.AuthMethod, func(), error) {
	var methods []ssh.AuthMethod
	closeFn := func() {}
	switch {
	case h.Key != "":
		s, err := loadKeySigner(h.Key)
		if err != nil {
			return nil, nil, err
		}
		methods = append(methods, ssh.PublicKeys(s))
	default:
		if conn := dialAgent(); conn != nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
			closeFn = func() { conn.Close() }
		}
	}
	if h.Pass != "" {
		methods = append(methods, ssh.Password(h.Pass), kbdPassword(h.Pass))
	}
	if len(methods) == 0 {
		return nil, nil, errors.New("no way to log in over SSH: set a password, a key (oob add --key) or load a key into ssh-agent")
	}
	return methods, closeFn, nil
}

// kbdPassword answers password-like keyboard-interactive prompts.
func kbdPassword(pass string) ssh.AuthMethod {
	return ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
		ans := make([]string, len(qs))
		for i, q := range qs {
			if !passwordPrompt.MatchString(q) {
				return nil, fmt.Errorf("unexpected keyboard-interactive prompt %q", q)
			}
			ans[i] = pass
		}
		return ans, nil
	})
}
