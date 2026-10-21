package bmc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/config"
)

// fakeIpmitool installs a shell script named ipmitool that runs body.
func fakeIpmitool(t *testing.T, body string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "ipmitool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
}

func TestCaptureCollectsOutputAndNoStdin(t *testing.T) {
	// The script reports whether stdin is empty: ipmitool must not inherit ours.
	fakeIpmitool(t, `echo "Chassis Power is on"; echo "warn: from stderr" >&2; if [ -z "$(cat)" ]; then echo stdin-empty; fi`)
	h := config.Host{IP: "192.0.2.1", User: "admin", Pass: "pw", Name: "node01"}
	out, err := Capture(context.Background(), func() error {
		return (&ipmiDriver{"node01", h}).Power("status")
	})
	if err != nil {
		t.Fatalf("Power: %v (out %q)", err, out)
	}
	for _, want := range []string{"Chassis Power is on", "warn: from stderr", "stdin-empty"} {
		if !strings.Contains(out, want) {
			t.Errorf("capture lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, "pw") && strings.Contains(out, "IPMI_PASSWORD") {
		t.Errorf("password leaked: %q", out)
	}
}

func TestCaptureKillsIpmitoolWhenContextEnds(t *testing.T) {
	fakeIpmitool(t, `sleep 30`)
	h := config.Host{IP: "192.0.2.1", User: "admin", Pass: "pw", Name: "node01"}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _ = Capture(ctx, func() error { return (&ipmiDriver{"node01", h}).Power("status") })
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Capture took %v; ipmitool was not stopped", d)
	}
}

func TestCaptureBoundsOutputAndDoesNotLeak(t *testing.T) {
	out, _ := Capture(context.Background(), func() error {
		Stdout().Write([]byte("START\n"))
		for range 8 {
			Stdout().Write([]byte(strings.Repeat("x", captureMax/2)))
		}
		Stdout().Write([]byte("\nEND"))
		return nil
	})
	if len(out) > captureMax+100 || !strings.Contains(out, "truncated") {
		t.Errorf("output not bounded: %d bytes", len(out))
	}
	if !strings.HasPrefix(out, "START") || !strings.HasSuffix(out, "END") {
		t.Errorf("should keep the start and the end of the output: %q ... %q", out[:10], out[len(out)-10:])
	}
	if capturing.Load() != nil {
		t.Error("capture state left set after Capture returned")
	}
	if Stdout() != os.Stdout {
		t.Error("Stdout() is not os.Stdout outside a Capture")
	}
}

func TestCaptureWaitsItsTurnAndHonorsContext(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	go Capture(context.Background(), func() error { close(started); <-release; return nil })
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Capture(ctx, func() error { t.Error("ran while another capture was active"); return nil }); err == nil {
		t.Error("second Capture should give up when its context ends")
	}
	close(release)
}

func TestOpenConsoleSOLRunsWithoutATerminal(t *testing.T) {
	fakeIpmitool(t, `echo "node01 login:"; read line; echo "got:$line"`)
	h := config.Host{IP: "192.0.2.1", User: "admin", Pass: "pw", Name: "node01"}
	tr, err := (&ipmiDriver{"node01", h}).OpenConsole(OpenConsoleOptions{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if err := tr.Link.Send([]byte("root\r")); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	buf := make([]byte, 256)
	deadline := time.After(10 * time.Second)
	read := make(chan int)
	go func() {
		for {
			n, err := tr.Out.Read(buf)
			got.Write(buf[:n])
			if err != nil || strings.Contains(got.String(), "got:root") {
				close(read)
				return
			}
		}
	}()
	select {
	case <-read:
	case <-deadline:
		t.Fatalf("no answer from the console; got %q", got.String())
	}
	select {
	case r := <-tr.Done:
		if r.Code != 0 {
			t.Errorf("exit code %d", r.Code)
		}
	case <-time.After(5 * time.Second):
		t.Error("Done never fired")
	}
	if tr.Tail == nil || !strings.Contains(tr.Tail(), "login:") {
		t.Error("Tail should hold the console output")
	}
}

func TestSupermicroOpenConsoleOnlyClearsASessionWithForce(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	fakeIpmitool(t, `echo "$@" >> `+log+`; case "$*" in *"sol activate"*) echo "[SOL Session operational]"; sleep 1;; esac`)
	h := config.Host{IP: "192.0.2.1", User: "admin", Pass: "pw", Name: "node01", Cipher: "17"}
	for _, force := range []bool{false, true} {
		os.Remove(log)
		tr, err := newSupermicroDriver("node01", h).OpenConsole(OpenConsoleOptions{Rows: 24, Cols: 80, Force: force})
		if err != nil {
			t.Fatal(err)
		}
		var b []byte
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if b, _ = os.ReadFile(log); strings.Contains(string(b), "sol activate") {
				break
			}
		}
		tr.Close()
		if got := strings.Contains(string(b), "sol deactivate"); got != force {
			t.Errorf("force=%v: deactivate = %v; ipmitool calls:\n%s", force, got, b)
		}
		if !strings.Contains(string(b), "sol activate") {
			t.Errorf("force=%v: SOL never started:\n%s", force, b)
		}
	}
}
