//go:build unix

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/ssimpson89/oob/internal/config"
)

// --force, --deactivate and --raw take the console's ownership lock and hold
// it for the operation: a second one, or an oob that shares the console, is
// kept out, and nothing can open the console in a gap after a check.
func TestSolDirectFlagsHoldTheConsoleLockForTheOperation(t *testing.T) {
	d, err := os.MkdirTemp("", "oob")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", d)
	t.Cleanup(func() { os.RemoveAll(d) })
	h := config.Host{IP: "192.0.2.10"}

	release, err := takeConsole("node01", h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := takeConsole("node01", h); err == nil || !strings.Contains(err.Error(), "open in another oob and shared") {
		t.Errorf("second takeConsole while held = %v, want the shared-console refusal", err)
	}
	release()
	release2, err := takeConsole("node01", h)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	release2()
}
