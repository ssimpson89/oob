package main

import (
	"strings"
	"testing"
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
		if got := remoteCommand(c.in); got != c.want {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
}

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
	h := Host{IP: "192.0.2.1"}
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

func TestRejectSOLFlags(t *testing.T) {
	if err := rejectSOLFlags(solOpts{}, "x"); err != nil {
		t.Error(err)
	}
	for _, o := range []solOpts{{force: true}, {deact: true}, {raw: true}} {
		if err := rejectSOLFlags(o, "an iLO uses VSP"); err == nil || !strings.Contains(err.Error(), "an iLO uses VSP") {
			t.Errorf("%+v: %v", o, err)
		}
	}
}
