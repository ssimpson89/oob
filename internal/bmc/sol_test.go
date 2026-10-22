package bmc

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
)

func newTestSOLLink(t *testing.T) (*solLink, *os.File, *exec.Cmd) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); r.Close() })
	return &solLink{ptmx: w, cmd: cmd}, r, cmd
}

// Hangup must put nothing on the wire (Disconnect's escape starts with a CR
// that would submit the remote line) and must end the client.
func TestSOLHangupTypesNothing(t *testing.T) {
	l, r, cmd := newTestSOLLink(t)
	if err := l.Hangup(); err != nil {
		t.Fatal(err)
	}
	_ = l.ptmx.Close()
	b, _ := io.ReadAll(r)
	if len(b) != 0 {
		t.Errorf("Hangup wrote %q, want nothing", b)
	}
	if err := cmd.Wait(); err == nil {
		t.Error("client still exited cleanly, want it killed")
	}
	if !l.hungUp.Load() {
		t.Error("hungUp not set, Release would not run")
	}
}

func TestSOLDisconnectSendsEscape(t *testing.T) {
	l, r, _ := newTestSOLLink(t)
	_ = l.Disconnect()
	_ = l.ptmx.Close()
	b, _ := io.ReadAll(r)
	if string(b) != "\r\x1e." {
		t.Errorf("Disconnect wrote %q", b)
	}
}

func TestSOLExitCodeAfterHangup(t *testing.T) {
	killed := func() *exec.ExitError {
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Process.Kill()
		var ee *exec.ExitError
		if !errors.As(cmd.Wait(), &ee) {
			t.Fatal("kill was not an exit error")
		}
		return ee
	}()
	if got := solExitCode(killed, true); got != 0 {
		t.Errorf("hangup kill: code %d, want 0", got)
	}
	if got := solExitCode(killed, false); got != 1 {
		t.Errorf("kill without hangup: code %d, want 1", got)
	}
	failed := func() *exec.ExitError {
		var ee *exec.ExitError
		if !errors.As(exec.Command("sh", "-c", "exit 3").Run(), &ee) {
			t.Fatal("no exit error")
		}
		return ee
	}()
	if got := solExitCode(failed, true); got != 3 {
		t.Errorf("real error after hangup: code %d, want 3", got)
	}
}
