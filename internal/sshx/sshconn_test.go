package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ssimpson89/oob/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func newEdSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newRSASigner(t *testing.T) ssh.Signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// sshTestServer accepts the password "pw" and serves only the given host keys.
func sshTestServer(t *testing.T, keys ...ssh.Signer) (host string, port int) {
	t.Helper()
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if string(p) == "pw" {
			return nil, nil
		}
		return nil, errors.New("denied")
	}}
	for _, k := range keys {
		cfg.AddHostKey(k)
	}
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
	a := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func knownLine(host string, port int, key ssh.PublicKey) string {
	return knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))}, key) + "\n"
}

func TestSSHHostKeyPinnedToStoredType(t *testing.T) {
	rsaKey, edKey := newRSASigner(t), newEdSigner(t)
	connect := func(t *testing.T, host string, port int, kh string) (*ssh.Client, error) {
		return ConnectWith(config.Host{IP: host, Port: port, User: "u", Pass: "pw", Name: "bmc1"}, kh, Options{})
	}

	t.Run("known RSA host offering only ed25519 is rejected", func(t *testing.T) {
		host, port := sshTestServer(t, edKey)
		kh := filepath.Join(t.TempDir(), "known_hosts")
		os.WriteFile(kh, []byte(knownLine(host, port, rsaKey.PublicKey())), 0o600)
		cl, err := connect(t, host, port, kh)
		if err == nil {
			cl.Close()
			t.Fatal("connected although the stored RSA key type was not offered")
		}
		b, _ := os.ReadFile(kh)
		if strings.Contains(string(b), "ssh-ed25519") {
			t.Error("the downgraded key was recorded in known_hosts")
		}
	})

	t.Run("known RSA host serving RSA and ed25519 uses RSA", func(t *testing.T) {
		host, port := sshTestServer(t, edKey, rsaKey)
		kh := filepath.Join(t.TempDir(), "known_hosts")
		os.WriteFile(kh, []byte(knownLine(host, port, rsaKey.PublicKey())), 0o600)
		cl, err := connect(t, host, port, kh)
		if err != nil {
			t.Fatal(err)
		}
		cl.Close()
	})

	t.Run("changed key of the stored type fails with reset advice", func(t *testing.T) {
		host, port := sshTestServer(t, edKey)
		kh := filepath.Join(t.TempDir(), "known_hosts")
		os.WriteFile(kh, []byte(knownLine(host, port, newEdSigner(t).PublicKey())), 0o600)
		_, err := connect(t, host, port, kh)
		var he *hostKeyChangedError
		if err == nil || !errors.As(err, &he) {
			t.Fatalf("want hostKeyChangedError, got %v", err)
		}
		if !strings.Contains(err.Error(), "--reset-hostkey") {
			t.Errorf("no reset advice: %v", err)
		}
		if err2 := NameConnErr(err, "bmc1"); !strings.Contains(err2.Error(), "oob ssh bmc1 --reset-hostkey") {
			t.Errorf("name not filled in: %v", err2)
		}
	})

	t.Run("unknown host is accepted and recorded", func(t *testing.T) {
		host, port := sshTestServer(t, edKey)
		kh := filepath.Join(t.TempDir(), "known_hosts")
		cl, err := connect(t, host, port, kh)
		if err != nil {
			t.Fatal(err)
		}
		cl.Close()
		if got := storedKeyTypes(kh, net.JoinHostPort(host, strconv.Itoa(port))); len(got) != 1 || got[0] != "ssh-ed25519" {
			t.Errorf("stored types %v", got)
		}
		// The next connection to it is held to that type.
		cl, err = connect(t, host, port, kh)
		if err != nil {
			t.Fatal(err)
		}
		cl.Close()
	})
}

func TestSSHHostKeyAlgs(t *testing.T) {
	got := sshHostKeyAlgs([]string{"ssh-rsa", "ssh-ed25519", "ssh-rsa", "ssh-dss"})
	want := []string{"rsa-sha2-512", "rsa-sha2-256", "ssh-rsa", "ssh-ed25519", "ssh-dss"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestStoredKeyTypesHashed(t *testing.T) {
	key := newEdSigner(t).PublicKey()
	addr := "192.0.2.4:2222"
	kh := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{knownhosts.HashHostname(knownhosts.Normalize(addr))}, key) + "\n"
	os.WriteFile(kh, []byte(line), 0o600)
	if got := storedKeyTypes(kh, addr); len(got) != 1 {
		t.Errorf("hashed entry not found: %v", got)
	}
	if got := storedKeyTypes(kh, "192.0.2.5:2222"); len(got) != 0 {
		t.Errorf("other host matched: %v", got)
	}
	// And --reset-hostkey removes it (the Redfish pins part is tested in bmc).
	if err := ResetKnownHosts(kh, addr); err != nil {
		t.Fatal(err)
	}
	if got := storedKeyTypes(kh, addr); len(got) != 0 {
		t.Errorf("entry survived reset: %v", got)
	}
}
