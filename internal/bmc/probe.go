package bmc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/detect"
	"github.com/ssimpson89/oob/internal/sshx"
	"golang.org/x/crypto/ssh"
)

// Vendor probe: a host with no type is looked at once over SSH (banner and the
// first output of a shell), so the first sol, power or boot already uses the
// right driver. Failures save nothing and the command carries on as generic
// IPMI; the next run probes again.
const (
	probeDialTimeout = 3 * time.Second
	probeReadTimeout = 5 * time.Second
)

// probeFn does the probe; tests replace it.
var probeFn = probeVendor

// probeOut is where the probe prints its one-line results (nil: stderr).
var probeOut io.Writer

func probeW() io.Writer {
	if probeOut != nil {
		return probeOut
	}
	return os.Stderr
}

// ProbeVendor logs in to the BMC over SSH like `oob ssh` does (same auth and
// host key pinning), opens an interactive shell (some BMCs reject exec), reads
// the pre-auth banner and the output up to the first prompt, and returns what
// the vendor detector makes of it. typ is "" when nothing was recognized. It
// saves nothing.
func ProbeVendor(name string, h config.Host) (typ string, gen int, err error) {
	return probeVendor(name, h, probeDialTimeout, probeReadTimeout)
}

func probeVendor(name string, h config.Host, dial, read time.Duration) (string, int, error) {
	h.Name = name
	kh, err := sshx.KnownHostsPath()
	if err != nil {
		return "", 0, err
	}
	var banner strings.Builder
	cl, err := sshx.ConnectTimeout(h, kh, func(s string) { banner.WriteString(s) }, dial)
	if err != nil {
		return "", 0, err
	}
	defer cl.Close()
	out, err := readFirstOutput(cl, banner.String(), read)
	typ, gen := detect.Classify(banner.String(), out)
	if typ != "" {
		return typ, gen, nil
	}
	return "", 0, err
}

// readFirstOutput opens a shell and reads until the vendor is clear, the first
// prompt shows, detect.Window bytes arrived, or read has passed. It then says
// goodbye and closes the session. A read problem is returned only for the
// caller to use if nothing was detected.
func readFirstOutput(cl *ssh.Client, banner string, read time.Duration) (string, error) {
	sess, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	modes := ssh.TerminalModes{ssh.ECHO: 0, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	if err := sess.RequestPty("xterm", 24, 80, modes); err != nil {
		return "", fmt.Errorf("request pty: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return "", err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return "", err
	}
	if err := sess.Shell(); err != nil {
		return "", err
	}
	chunks := make(chan []byte)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(chunks)
		buf := make([]byte, 1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				select {
				case chunks <- append([]byte(nil), buf[:n]...):
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(read)
	defer timer.Stop()
	var out []byte
loop:
	for {
		if typ, gen := detect.Classify(banner, string(out)); typ != "" && (typ != detect.TypeILO || gen > 0) {
			break
		}
		if detect.WindowDone(string(out)) {
			break
		}
		select {
		case b, ok := <-chunks:
			if !ok {
				break loop
			}
			out = append(out, b...)
		case <-timer.C:
			break loop
		}
	}
	if len(out) > detect.Window {
		out = out[:detect.Window]
	}
	_, _ = stdin.Write([]byte("exit\n"))
	return string(out), nil
}

// probeDisabled reports whether OOB_NO_PROBE turns the vendor probe off.
func probeDisabled() bool {
	v := os.Getenv("OOB_NO_PROBE")
	return v != "" && v != "0"
}

// quietProbeError is true for a failure that only says SSH is not reachable
// (refused, timed out, no route): IPMI will say the same, so it needs no note.
func quietProbeError(err error) bool {
	var te *console.TimeoutError
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.As(err, &te) || console.IsTimeout(err)
}

// Resolve returns h with its vendor filled in. For a host with no type it
// probes once over SSH and, on a hit, saves the type (and iLO generation) and
// says so on stderr. Hosts that have a type (including "ipmi"), OOB_NO_PROBE=1,
// and hosts that cannot log in over SSH are returned unchanged. budget, when
// above zero, is the time the caller has for this host overall; the probe uses
// at most half of it. Notes are left out when notes are quiet (many hosts).
func Resolve(name string, h config.Host, budget time.Duration) config.Host {
	if h.Type != "" || probeDisabled() {
		return h
	}
	h.Name = name
	if sshx.CheckCreds(name, h) != nil {
		return h // nothing to log in with; the IPMI path reports its own problems
	}
	dial, read := probeDialTimeout, probeReadTimeout
	if budget > 0 {
		dial, read = min(dial, budget/4), min(read, budget/4)
	}
	typ, gen, err := probeFn(name, h, dial, read)
	if err != nil {
		if !quietProbeError(err) && !app.Quiet() {
			app.Notef(probeW(), "vendor detection over SSH skipped for %s: %v", name, err)
		}
		return h
	}
	if typ == "" {
		return h
	}
	set, serr := detect.SaveType(name, typ, gen)
	if !app.Quiet() {
		switch {
		case serr != nil:
			app.Notef(probeW(), "detected %s on %s but could not save the type: %v", detect.VendorLabel(typ, gen), name, serr)
		case set:
			fmt.Fprintf(probeW(), "detected %s on %s; saved type %s\n", detect.VendorLabel(typ, gen), name, typ)
		}
	}
	h.Type = typ
	if typ == detect.TypeILO && gen > 0 {
		h.ILOGen = gen
	}
	return h
}

// DriverForProbed is DriverFor after Resolve: the driver for a host whose
// vendor may still be unknown.
func DriverForProbed(name string, h config.Host) Driver {
	return DriverFor(name, Resolve(name, h, 0))
}
