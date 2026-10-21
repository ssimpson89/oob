package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFindIpmitoolMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if p, err := findIpmitool(); err == nil {
		t.Skipf("ipmitool present at a fallback location: %s", p)
	} else if !errors.Is(err, errIpmitoolMissing) || !strings.Contains(err.Error(), "install ipmitool") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInstallHint(t *testing.T) {
	for goos, want := range map[string]string{
		"darwin":  "brew install ipmitool",
		"linux":   "apt install ipmitool",
		"freebsd": "dnf install ipmitool",
	} {
		if got := installHint(goos); !strings.Contains(got, want) {
			t.Errorf("%s: %q missing %q", goos, got, want)
		}
	}
	if !strings.Contains(installHint("linux"), "dnf install ipmitool") {
		t.Error("linux hint missing dnf")
	}
}

func TestRiskyIpmiArgs(t *testing.T) {
	for _, c := range []struct {
		args  []string
		risky bool
	}{
		{[]string{"chassis", "power", "off"}, true},
		{[]string{"chassis", "power", "cycle"}, true},
		{[]string{"chassis", "power", "reset"}, true},
		{[]string{"chassis", "power", "soft"}, true},
		{[]string{"chassis", "power", "diag"}, true},
		{[]string{"-v", "-C", "3", "chassis", "power", "off"}, true},
		{[]string{"CHASSIS POWER OFF"}, true},
		{[]string{"power", "cycle"}, true},
		{[]string{"mc", "reset", "cold"}, true},
		{[]string{"bmc", "reset", "warm"}, true},
		{[]string{"sel", "clear"}, true},
		{[]string{"raw", "0x06", "0x02"}, true},
		{[]string{"chassis", "power", "status"}, false},
		{[]string{"chassis", "power", "on"}, false},
		{[]string{"sel", "list"}, false},
		{[]string{"sdr"}, false},
		{[]string{"mc", "info"}, false},
		{[]string{"lan", "print", "1"}, false},
	} {
		if what, got := riskyIpmiArgs(c.args); got != c.risky || (got && what == "") {
			t.Errorf("%v: got %q %v, want risky=%v", c.args, what, got, c.risky)
		}
	}
}

func TestIpmiMessagesUseHostName(t *testing.T) {
	h := Host{Name: "node01", IP: "192.0.2.7"}
	out := "Error: Unable to establish IPMI v2 / RMCP+ session\n"
	if hint := ipmiHint(out, h); strings.Contains(hint, "<name>") {
		t.Errorf("placeholder left in hint: %s", hint)
	} else if !strings.Contains(hint, "oob ssh node01") {
		t.Errorf("hint does not name the host: %s", hint)
	}
	if hint := ipmiHint(out, Host{}); !strings.Contains(hint, "<name>") {
		t.Errorf("unnamed host keeps the placeholder: %s", hint)
	}
	t.Setenv("PATH", "/nonexistent")
	if _, err := findIpmitool(); err != nil {
		t.Skip("no ipmitool to build a command with")
	}
	if _, err := ipmiCmd(context.Background(), Host{Name: "node01", IP: "192.0.2.7"}, "mc", "info"); err == nil || !strings.Contains(err.Error(), "node01:") || strings.Contains(err.Error(), "192.0.2.7") {
		t.Errorf("credential error should name the host: %v", err)
	}
}

func TestErrorLinesSanitized(t *testing.T) {
	got := errorLines("noise\r\nError: bad \x1b]0;x\x07thing\x1b[2J\r\n")
	if len(got) != 1 || got[0] != "Error: bad thing" {
		t.Errorf("%q", got)
	}
}

func TestIpmiPowerTimeoutIsUnknown(t *testing.T) {
	err := powerResult(Host{Name: "node01"}, 1, "Error: timed out waiting for response", nil)
	if err == nil || !strings.Contains(err.Error(), "result unknown; check `oob power node01`") {
		t.Errorf("%v", err)
	}
	if err := powerResult(Host{Name: "node01"}, 1, "Error: Unauthorized name", nil); err == nil || strings.Contains(err.Error(), "unknown") {
		t.Errorf("an ordinary failure is not an unknown result: %v", err)
	}
	if _, err := ipmiPowerOutcome("node01", "", &timeoutErr{after: 30 * time.Second}); err == nil || !strings.Contains(err.Error(), "result unknown") {
		t.Errorf("console power timeout: %v", err)
	}
}

func TestIpmiHint(t *testing.T) {
	out := "Error: Unable to establish IPMI v2 / RMCP+ session\n"
	h := ipmiHint(out, Host{})
	for _, want := range []string{"IPMI/DCMI over LAN", "`cipher`", "type"} {
		if !strings.Contains(h, want) {
			t.Errorf("hint lacks %q: %s", want, h)
		}
	}
	if h := ipmiHint(out, Host{Type: "ipmi"}); strings.Contains(h, "detects it") || h == "" {
		t.Errorf("typed host hint: %q", h)
	}
	if ipmiHint("Error: Unauthorized name\n", Host{}) != "" {
		t.Error("hint on an unrelated error")
	}
	got := errorLines("\x1b[0mnoise\r\nError: Unable to establish IPMI v2 / RMCP+ session\r\n")
	if len(got) != 1 || !strings.HasPrefix(got[0], "Error: Unable") {
		t.Errorf("%q", got)
	}
}

func TestIpmiHintPerVendor(t *testing.T) {
	out := "Error: Unable to establish IPMI v2 / RMCP+ session\n"
	if h := ipmiHint(out, Host{Type: typeILO}); !strings.Contains(h, "iLO Settings > Access > IPMI/DCMI over LAN") || !strings.Contains(h, "iLO 7") {
		t.Error(h)
	}
	if h := ipmiHint(out, Host{Type: typeIDRAC}); !strings.Contains(h, "iDRAC") || strings.Contains(h, "iLO") {
		t.Error(h)
	}
	if strings.Contains(ipmiHint(out, Host{}), "Security > Access Settings") {
		t.Error("old iLO path still present")
	}
}

func TestTailBuf(t *testing.T) {
	var tb tailBuf
	tb.Write([]byte(strings.Repeat("a", tailMax)))
	tb.Write([]byte("end"))
	if s := tb.String(); len(s) != tailMax || !strings.HasSuffix(s, "end") {
		t.Errorf("len %d", len(s))
	}
}

// The BMC password goes to ipmitool through IPMI_PASSWORD (-E), never argv,
// where any local user could read it with ps.
func TestIpmiCmdArgvHasNoPassword(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ipmitool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	const pw = "s3cret-pw-Q9"
	h := Host{Name: "node01", IP: "192.0.2.7", User: "admin", Pass: pw, Cipher: "17"}
	for _, args := range [][]string{{"mc", "info"}, {"-e", "^", "sol", "activate"}, {"chassis", "power", "status"}} {
		cmd, err := ipmiCmd(context.Background(), h, args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range cmd.Args {
			if strings.Contains(a, pw) {
				t.Errorf("password in argv: %q", cmd.Args)
			}
		}
		if !slices.Contains(cmd.Args, "-E") || slices.Contains(cmd.Args, "-P") {
			t.Errorf("want -E and no -P: %q", cmd.Args)
		}
		if !slices.Contains(cmd.Env, "IPMI_PASSWORD="+pw) {
			t.Error("IPMI_PASSWORD missing from the environment")
		}
	}
}
