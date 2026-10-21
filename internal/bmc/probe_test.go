package bmc

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/detect"
	"golang.org/x/crypto/ssh"
)

// probeBMC is an in-process SSH server that behaves like a BMC: a pre-auth
// banner, a shell (no exec) that prints out and waits for "exit".
type probeBMC struct {
	port    int
	exited  atomic.Int32
	conns   atomic.Int32
	onShell func() // called when a shell starts, before output
}

func startProbeBMC(t *testing.T, banner, out, pass string) *probeBMC {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if string(p) == pass {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		BannerCallback: func(ssh.ConnMetadata) string { return banner },
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &probeBMC{port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.conns.Add(1)
			go f.serve(c, cfg, out)
		}
	}()
	return f
}

func (f *probeBMC) serve(c net.Conn, cfg *ssh.ServerConfig, out string) {
	defer c.Close()
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
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
					if f.onShell != nil {
						f.onShell()
					}
					io.WriteString(ch, out)
					buf := make([]byte, 64)
					n, _ := ch.Read(buf)
					if strings.HasPrefix(string(buf[:n]), "exit") {
						f.exited.Add(1)
					}
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ S uint32 }{0}))
					return
				default:
					r.Reply(false, nil) // exec is rejected, like iLO 7
				}
			}
		}()
	}
	sc.Close()
}

// probeEnv points the config and known_hosts at a temp dir, turns the agent
// off, saves host node01 (no type) pointing at port and returns the config path.
func probeEnv(t *testing.T, typ string, port int) (string, config.Host) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hosts.json")
	t.Setenv("OOB_CONFIG", p)
	t.Setenv("OOB_NO_AGENT", "1")
	t.Setenv("OOB_NO_PROBE", "")
	h := config.Host{IP: "127.0.0.1", Port: port, User: "admin", Pass: "pw", Type: typ}
	c := &config.Config{Hosts: map[string]config.Host{"node01": h}}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	return p, h
}

func savedType(t *testing.T, p string) (string, int) {
	t.Helper()
	c, err := config.LoadConfig(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Hosts["node01"].Type, c.Hosts["node01"].ILOGen
}

func captureProbeOut(t *testing.T) *bytes.Buffer {
	var b bytes.Buffer
	swap[io.Writer](t, &probeOut, &b)
	return &b
}

func TestProbeILOBannerSavesTypeAndUsesILODriver(t *testing.T) {
	srv := startProbeBMC(t, "Integrated Lights-Out 7\r\n", "</>hpeiLO-> ", "pw")
	p, h := probeEnv(t, "", srv.port)
	out := captureProbeOut(t)
	d := DriverForProbed("node01", h)
	if got := typeName(d); got != "*bmc.iloDriver" {
		t.Errorf("driver %s", got)
	}
	if typ, gen := savedType(t, p); typ != "ilo" || gen != 7 {
		t.Errorf("saved %q gen %d", typ, gen)
	}
	if want := "detected HPE iLO 7 on node01; saved type ilo\n"; out.String() != want {
		t.Errorf("output %q want %q", out.String(), want)
	}
	waitFor(t, func() bool { return srv.exited.Load() == 1 }, "exit sent")
}

func TestProbeIDRACPrompt(t *testing.T) {
	srv := startProbeBMC(t, "", "\r\n/admin1-> ", "pw")
	p, h := probeEnv(t, "", srv.port)
	out := captureProbeOut(t)
	if got := typeName(DriverForProbed("node01", h)); got != "*bmc.idracDriver" {
		t.Errorf("driver %s", got)
	}
	if typ, _ := savedType(t, p); typ != "idrac" {
		t.Errorf("saved %q", typ)
	}
	if !strings.Contains(out.String(), "detected Dell iDRAC on node01; saved type idrac") {
		t.Errorf("output %q", out.String())
	}
}

func TestProbeNothingDetected(t *testing.T) {
	srv := startProbeBMC(t, "Welcome\r\n", "[admin@node01 ~]$ ", "pw")
	p, h := probeEnv(t, "", srv.port)
	out := captureProbeOut(t)
	if got := typeName(DriverForProbed("node01", h)); got != "*bmc.ipmiDriver" {
		t.Errorf("driver %s", got)
	}
	if typ, _ := savedType(t, p); typ != "" {
		t.Errorf("saved %q", typ)
	}
	if out.Len() != 0 {
		t.Errorf("output %q", out.String())
	}
}

func TestProbeDialFailureIsQuiet(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens: connection refused
	p, h := probeEnv(t, "", port)
	out := captureProbeOut(t)
	if got := typeName(DriverForProbed("node01", h)); got != "*bmc.ipmiDriver" {
		t.Errorf("driver %s", got)
	}
	if typ, _ := savedType(t, p); typ != "" {
		t.Errorf("saved %q", typ)
	}
	if out.Len() != 0 {
		t.Errorf("noisy: %q", out.String())
	}
}

func TestProbeAuthFailureNotes(t *testing.T) {
	srv := startProbeBMC(t, "Integrated Lights-Out 7\r\n", "</>hpeiLO-> ", "other")
	p, h := probeEnv(t, "", srv.port)
	out := captureProbeOut(t)
	if got := typeName(DriverForProbed("node01", h)); got != "*bmc.ipmiDriver" {
		t.Errorf("driver %s", got)
	}
	if typ, _ := savedType(t, p); typ != "" {
		t.Errorf("saved %q", typ)
	}
	if !strings.Contains(out.String(), "vendor detection over SSH skipped for node01") {
		t.Errorf("no note: %q", out.String())
	}
	// Quiet notes (status) silence it.
	out.Reset()
	swap(t, &app.Quiet, func() bool { return true })
	Resolve("node01", h, 0)
	if out.Len() != 0 {
		t.Errorf("quiet mode printed %q", out.String())
	}
}

func TestProbeSkipped(t *testing.T) {
	var calls atomic.Int32
	swap(t, &probeFn, func(string, config.Host, time.Duration, time.Duration) (string, int, error) {
		calls.Add(1)
		return detect.TypeILO, 7, nil
	})
	_, h := probeEnv(t, "", 22)
	h.Type = detect.TypeIPMI
	if got := Resolve("node01", h, 0); got.Type != detect.TypeIPMI {
		t.Errorf("type %q", got.Type)
	}
	h.Type = detect.TypeIDRAC
	Resolve("node01", h, 0)
	h.Type = ""
	t.Setenv("OOB_NO_PROBE", "1")
	if got := Resolve("node01", h, 0); got.Type != "" {
		t.Errorf("probed despite OOB_NO_PROBE: %q", got.Type)
	}
	if calls.Load() != 0 {
		t.Fatalf("probe ran %d times", calls.Load())
	}
	t.Setenv("OOB_NO_PROBE", "0")
	captureProbeOut(t)
	if got := Resolve("node01", h, 0); got.Type != detect.TypeILO || got.ILOGen != 7 || calls.Load() != 1 {
		t.Errorf("OOB_NO_PROBE=0 should probe: %+v calls %d", got, calls.Load())
	}
}

func TestProbeBudgetShrinksTimeouts(t *testing.T) {
	var dial, read time.Duration
	swap(t, &probeFn, func(_ string, _ config.Host, d, r time.Duration) (string, int, error) {
		dial, read = d, r
		return "", 0, nil
	})
	_, h := probeEnv(t, "", 22)
	Resolve("node01", h, 2*time.Second)
	if dial != 500*time.Millisecond || read != 500*time.Millisecond {
		t.Errorf("dial %v read %v", dial, read)
	}
	Resolve("node01", h, 0)
	if dial != probeDialTimeout || read != probeReadTimeout {
		t.Errorf("defaults: dial %v read %v", dial, read)
	}
}

func TestProbeConcurrentResolve(t *testing.T) {
	const n = 4
	var gate sync.WaitGroup
	gate.Add(n)
	swap(t, &probeFn, func(string, config.Host, time.Duration, time.Duration) (string, int, error) {
		gate.Done()
		gate.Wait() // only returns when all n probes run at once
		return "", 0, nil
	})
	_, h := probeEnv(t, "", 22)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() { defer wg.Done(); Resolve("node01", h, 0) }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("probes did not run concurrently")
	}
}

func waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
