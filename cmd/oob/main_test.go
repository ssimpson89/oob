package main

import (
	"testing"

	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/sshx"
)

// swap sets *p to v for the rest of the test and restores the old value when
// the test ends.
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// clearNotices empties the queued notes when the test ends.
func clearNotices(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		notices.Lock()
		notices.msgs = nil
		notices.Unlock()
	})
}

func TestArgOr(t *testing.T) {
	if got := argOr(nil, "x"); got != "x" {
		t.Errorf("got %q", got)
	}
	if got := argOr([]string{"y", "z"}, "x"); got != "y" {
		t.Errorf("got %q", got)
	}
}

func TestClampWorkers(t *testing.T) {
	for _, c := range []struct{ w, n, want int }{{0, 5, 1}, {-3, 5, 1}, {16, 3, 3}, {2, 5, 2}} {
		if got := clampWorkers(c.w, c.n); got != c.want {
			t.Errorf("clampWorkers(%d,%d)=%d want %d", c.w, c.n, got, c.want)
		}
	}
}

func TestRemoteCommand(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"show system1"}, "show system1"},
		{[]string{"ls", "-l", "/tmp"}, "ls -l /tmp"},
		{[]string{"echo", "a b", "it's", ";rm"}, `echo 'a b' 'it'\''s' ';rm'`},
		{[]string{"echo", ""}, "echo ''"},
	} {
		if got := sshx.RemoteCommand(c.in); got != c.want {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestFlagErrorNumeric(t *testing.T) {
	cfg := tmpCfg(t)
	_, _, err := runOob(t, cfg, "", "add", "h", "192.0.2.1", "--port", "abc")
	if err == nil || err.Error() != "port must be a number" {
		t.Errorf("err=%v", err)
	}
}

func TestValidIPOrHost(t *testing.T) {
	for s, want := range map[string]bool{"192.0.2.1": true, "2001:db8::1": true, "bmc-1.example.com": true, "a b": false, "": false, "-x": false, "host/x": false} {
		if got := validIPOrHost(s); got != want {
			t.Errorf("%q: %v", s, got)
		}
	}
}

func TestDestructiveActions(t *testing.T) {
	h := config.Host{IP: "192.0.2.1"}
	for _, c := range []struct {
		cmd  string
		rest []string
		want bool
	}{
		{"power", nil, false}, {"power", []string{"status"}, false}, {"power", []string{"on"}, false},
		{"power", []string{"off"}, true}, {"power", []string{"cycle"}, true},
		{"power", []string{"reset"}, true}, {"power", []string{"soft"}, true},
		{"sel", nil, false}, {"sel", []string{"list"}, false}, {"sel", []string{"clear"}, true},
		{"mcreset", nil, true}, {"nmi", nil, true},
		{"ipmi", []string{"mc", "info"}, false}, {"ipmi", []string{"chassis", "power", "off"}, true},
		{"", []string{"off"}, false}, {"boot", []string{"pxe"}, false},
	} {
		if got := isDestructive(c.cmd, c.rest); got != c.want {
			t.Errorf("isDestructive(%q, %v) = %v, want %v", c.cmd, c.rest, got, c.want)
		}
	}
	swap(t, &yesFlag, true)
	if err := confirmDestructive("power", "c1", h, []string{"off"}); err != nil {
		t.Errorf("--yes: %v", err)
	}
	swap(t, &yesFlag, false) // stdin is not a terminal under go test
	if err := confirmDestructive("power", "c1", h, []string{"off"}); err == nil {
		t.Error("destructive action confirmed without --yes or a terminal")
	}
	if err := confirmDestructive("power", "c1", h, []string{"status"}); err != nil {
		t.Errorf("status needs no confirmation: %v", err)
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
