package sshx

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ssimpson89/oob/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// writeTestKey writes a throwaway ed25519 private key (optionally protected)
// and returns its path and signer.
func writeTestKey(t *testing.T, mode os.FileMode, passphrase string) (string, ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var blk *pem.Block
	if passphrase == "" {
		blk, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "id_test")
	if err := os.WriteFile(p, pem.EncodeToMemory(blk), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return p, s
}

// authServer accepts the password "pw" and the given public keys, and records
// the order of the auth methods the client tried.
type authServer struct {
	mu      sync.Mutex
	methods []string
}

func (a *authServer) tried() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.methods...)
}

func newAuthServer(t *testing.T, authorized ...ssh.PublicKey) (*authServer, string, int) {
	t.Helper()
	as := &authServer{}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if string(p) == "pw" {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			for _, a := range authorized {
				if bytes.Equal(a.Marshal(), k.Marshal()) {
					return nil, nil
				}
			}
			return nil, errors.New("unknown key")
		},
		AuthLogCallback: func(_ ssh.ConnMetadata, method string, err error) {
			as.mu.Lock()
			defer as.mu.Unlock()
			if len(as.methods) == 0 || as.methods[len(as.methods)-1] != method {
				as.methods = append(as.methods, method)
			}
		},
	}
	cfg.AddHostKey(newEdSigner(t))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				go func() {
					for ch := range chans {
						ch.Reject(ssh.Prohibited, "no")
					}
				}()
				sc.Wait()
			}()
		}
	}()
	return as, "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

// startAgent serves an in-process ssh-agent holding the given keys and points
// SSH_AUTH_SOCK at it.
func startAgent(t *testing.T, keys ...any) {
	t.Helper()
	kr := agent.NewKeyring()
	for _, k := range keys {
		if err := kr.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.MkdirTemp("", "a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { agent.ServeAgent(kr, c); c.Close() }()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)
}

func noAgentEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("OOB_NO_AGENT", "")
}

func dialTest(t *testing.T, h config.Host, port int) error {
	t.Helper()
	h.IP, h.Port, h.User, h.Name = "127.0.0.1", port, "u", "bmc1"
	cl, err := ConnectOpts(h, filepath.Join(t.TempDir(), "known_hosts"), nil)
	if err == nil {
		cl.Close()
	}
	return err
}

func TestAuthKeySetUsesKeyOnlyThenPassword(t *testing.T) {
	noAgentEnv(t)
	path, signer := writeTestKey(t, 0o600, "")
	t.Run("authorized key, no password", func(t *testing.T) {
		as, _, port := newAuthServer(t, signer.PublicKey())
		if err := dialTest(t, config.Host{Key: path}, port); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(as.tried(), ","); got != "none,publickey" {
			t.Errorf("methods = %s", got)
		}
	})
	t.Run("unauthorized key falls back to password", func(t *testing.T) {
		as, _, port := newAuthServer(t)
		if err := dialTest(t, config.Host{Key: path, Pass: "pw"}, port); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(as.tried(), ","); got != "none,publickey,password" {
			t.Errorf("methods = %s", got)
		}
	})
	t.Run("key set: agent keys are not offered", func(t *testing.T) {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		startAgent(t, priv)
		sp, err := ssh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		// the server would accept the agent's key, but the host names another key
		_, _, port := newAuthServer(t, sp.PublicKey())
		if err := dialTest(t, config.Host{Key: path}, port); err == nil {
			t.Fatal("logged in with the agent key although a key file was set")
		}
	})
}

func TestAuthAgentOnly(t *testing.T) {
	noAgentEnv(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	startAgent(t, priv)
	sp, _ := ssh.NewSignerFromKey(priv)
	as, _, port := newAuthServer(t, sp.PublicKey())
	if err := dialTest(t, config.Host{}, port); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(as.tried(), ","); got != "none,publickey" {
		t.Errorf("methods = %s", got)
	}

	t.Run("OOB_NO_AGENT turns it off", func(t *testing.T) {
		t.Setenv("OOB_NO_AGENT", "1")
		as, _, port := newAuthServer(t, sp.PublicKey())
		if err := dialTest(t, config.Host{Pass: "pw"}, port); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(as.tried(), ","); got != "none,password" {
			t.Errorf("methods = %s", got)
		}
	})
}

func TestAuthPasswordOnly(t *testing.T) {
	noAgentEnv(t)
	as, _, port := newAuthServer(t)
	if err := dialTest(t, config.Host{Pass: "pw"}, port); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(as.tried(), ","); got != "none,password" {
		t.Errorf("methods = %s", got)
	}
}

func TestAuthNothingToTry(t *testing.T) {
	noAgentEnv(t)
	err := CheckCreds("bmc1", config.Host{User: "u"})
	if err == nil || !strings.Contains(err.Error(), "no way to log in") {
		t.Errorf("CheckCreds = %v", err)
	}
	if err := CheckCreds("bmc1", config.Host{User: "u", Key: "/x"}); err != nil {
		t.Errorf("key set: %v", err)
	}
	if err := CheckCreds("bmc1", config.Host{User: "u", Pass: "p"}); err != nil {
		t.Errorf("password set: %v", err)
	}
	if err := CheckCreds("bmc1", config.Host{Pass: "p"}); err == nil {
		t.Error("no user accepted")
	}
	startAgent(t)
	if err := CheckCreds("bmc1", config.Host{User: "u"}); err != nil {
		t.Errorf("agent reachable: %v", err)
	}
	noAgentEnv(t)
	if _, _, err := sshAuthMethods(config.Host{}); err == nil {
		t.Error("sshAuthMethods with nothing succeeded")
	}
}

func TestKeyPassphrase(t *testing.T) {
	path, signer := writeTestKey(t, 0o600, "s3cret")
	orig := keyPassphrase
	t.Cleanup(func() { keyPassphrase = orig })

	t.Run("no terminal", func(t *testing.T) {
		keyPassphrase = func(p string) ([]byte, error) { return orig(p) }
		// stdin is not a terminal under go test
		_, err := loadKeySigner(path)
		if err == nil || !strings.Contains(err.Error(), "ssh-agent") || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("right passphrase", func(t *testing.T) {
		keyPassphrase = func(string) ([]byte, error) { return []byte("s3cret"), nil }
		s, err := loadKeySigner(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(s.PublicKey().Marshal(), signer.PublicKey().Marshal()) {
			t.Error("wrong key loaded")
		}
	})
	t.Run("wrong passphrase", func(t *testing.T) {
		keyPassphrase = func(string) ([]byte, error) { return []byte("nope"), nil }
		_, err := loadKeySigner(path)
		if err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestKeyPermsRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660} {
		path, _ := writeTestKey(t, mode, "")
		_, err := loadKeySigner(path)
		if err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: err = %v", mode, err)
		}
	}
	path, _ := writeTestKey(t, 0o400, "")
	if _, err := loadKeySigner(path); err != nil {
		t.Errorf("0400: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "junk")
	os.WriteFile(bad, []byte("not a key"), 0o600)
	if _, err := loadKeySigner(bad); err == nil || strings.Contains(err.Error(), "not a key") {
		t.Errorf("junk: %v", err)
	}
	if _, err := loadKeySigner(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestKeyHomeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, _ := writeTestKey(t, 0o600, "")
	b, _ := os.ReadFile(path)
	os.WriteFile(filepath.Join(home, "k"), b, 0o600)
	if _, err := loadKeySigner("~/k"); err != nil {
		t.Fatal(err)
	}
}

func TestKeyMerge(t *testing.T) {
	c := &config.Config{Defaults: config.Host{Key: "~/d"}, Hosts: map[string]config.Host{"a": {IP: "1.1.1.1"}, "b": {IP: "2.2.2.2", Key: "~/b"}}}
	if _, h, _ := c.Lookup("a"); h.Key != "~/d" {
		t.Errorf("a key = %q", h.Key)
	}
	if _, h, _ := c.Lookup("b"); h.Key != "~/b" {
		t.Errorf("b key = %q", h.Key)
	}
}
