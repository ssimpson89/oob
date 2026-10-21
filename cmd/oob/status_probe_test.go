package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/config"
	"golang.org/x/crypto/ssh"
)

// gatedBMC starts an in-process SSH server that prints an iDRAC prompt from its
// shell, but only after every server sharing gate has a shell open. If the
// probes ran one after another, none would ever get its prompt.
func gatedBMC(t *testing.T, gate *sync.WaitGroup) int {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if string(p) == "pw" {
			return nil, nil
		}
		return nil, errors.New("denied")
	}}
	cfg.AddHostKey(signer)
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
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, chReqs, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer ch.Close()
						for r := range chReqs {
							switch r.Type {
							case "pty-req":
								r.Reply(true, nil)
							case "shell":
								r.Reply(true, nil)
								gate.Done()
								waitTimeout(gate, 5*time.Second)
								io.WriteString(ch, "\r\n/admin1-> ")
								buf := make([]byte, 16)
								ch.Read(buf)
								return
							default:
								r.Reply(false, nil)
							}
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

func TestStatusProbesConcurrentlyAndQuietly(t *testing.T) {
	const n = 4
	var gate sync.WaitGroup
	gate.Add(n)
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hosts.json")
	swap(t, &cfgFlag, "")
	t.Setenv("OOB_CONFIG", cfgFile)
	t.Setenv("OOB_NO_AGENT", "1")
	t.Setenv("OOB_NO_PROBE", "")
	c := &config.Config{Hosts: map[string]config.Host{}}
	for i := 1; i <= n; i++ {
		c.Hosts["node0"+strconv.Itoa(i)] = config.Host{IP: "127.0.0.1", Port: gatedBMC(t, &gate), User: "admin", Pass: "pw"}
	}
	if err := c.Save(cfgFile); err != nil {
		t.Fatal(err)
	}
	// Collect stderr; the real one is put back afterwards.
	errF, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	swap(t, &os.Stderr, errF)
	swap(t, &os.Stdout, func() *os.File { f, _ := os.Create(filepath.Join(dir, "stdout")); return f }())
	t.Cleanup(func() { setQuietNotes(false) })

	cmd := statusCmd()
	cmd.SetArgs([]string{"--workers", "4", "--timeout", "20s"})
	start := time.Now()
	_ = cmd.Execute() // the iDRAC status itself fails against this fake; that is not under test
	if time.Since(start) > 4*time.Second {
		t.Errorf("status took %v: probes were not concurrent", time.Since(start))
	}
	b, _ := os.ReadFile(errF.Name())
	for _, bad := range []string{"detected", "vendor detection", "note:"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("status was not quiet, stderr has %q:\n%s", bad, b)
		}
	}
	got, err := config.LoadConfig(cfgFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range got.Hosts {
		if h.Type != "idrac" {
			t.Errorf("%s: type %q, want idrac saved", name, h.Type)
		}
	}
}
