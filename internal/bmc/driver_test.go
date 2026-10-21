package bmc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ssimpson89/oob/internal/config"
)

func TestBootArgs(t *testing.T) {
	for _, c := range []struct {
		legacy, persistent bool
		want               string
	}{
		{false, false, "options=efiboot"},
		{false, true, "options=persistent,efiboot"},
		{true, true, "options=persistent"},
		{true, false, ""},
	} {
		a := bootArgs("pxe", c.legacy, c.persistent)
		got := ""
		if len(a) > 3 {
			got = a[3]
		}
		if got != c.want {
			t.Errorf("%+v: got %q want %q", c, got, c.want)
		}
	}
}

func TestRejectSOLFlags(t *testing.T) {
	if err := rejectSOLFlags(SOLOptions{}, "x"); err != nil {
		t.Error(err)
	}
	for _, o := range []SOLOptions{{Force: true}, {Deact: true}, {Raw: true}} {
		if err := rejectSOLFlags(o, "an iLO uses VSP"); err == nil || !strings.Contains(err.Error(), "an iLO uses VSP") {
			t.Errorf("%+v: %v", o, err)
		}
	}
}

func TestStatusFail(t *testing.T) {
	if r := statusFail("Error: Unable to establish\x1b[1m"); !r.Failed || r.Text != "ERROR: Unable to establish" {
		t.Errorf("%+v", r)
	}
	if r := statusFail("ERROR: Error: x"); r.Text != "ERROR: x" {
		t.Errorf("%+v", r)
	}
}

func TestCheckCredsNeedsPassword(t *testing.T) {
	// No agent, so only the key is on offer.
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("OOB_NO_AGENT", "")
	err := checkCreds("bmc1", config.Host{User: "u", Key: "/x"})
	if err == nil || !strings.Contains(err.Error(), "IPMI/Redfish need the BMC password even when you use an SSH key") {
		t.Errorf("checkCreds = %v", err)
	}
}

func TestResetHostKeyKeepsOtherPins(t *testing.T) {
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(pinsPath(kh), []byte("192.0.2.4:443 abc\n192.0.2.9:443 def\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ResetHostKey(kh, "192.0.2.4:22"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(pinsPath(kh))
	if string(b) != "192.0.2.9:443 def\n" {
		t.Errorf("pins after reset: %q", b)
	}
}
