package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/bmc"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/detect"
)

// runOob executes the CLI against a throwaway config and returns stdout, stderr and the error.
func runOob(t *testing.T, cfg, stdin string, args ...string) (string, string, error) {
	t.Helper()
	root := newRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"-c", cfg}, args...))
	err := root.Execute()
	return out.String(), errb.String(), err
}

func tmpCfg(t *testing.T) string { return filepath.Join(t.TempDir(), "hosts.json") }

func mustLoad(t *testing.T, p string) *config.Config {
	t.Helper()
	c, err := config.LoadConfig(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAddRoundTripAndPartialUpdate(t *testing.T) {
	cfg := tmpCfg(t)
	out, errs, err := runOob(t, cfg, "", "add", "h1", "192.0.2.10", "--user", "admin", "--pass", "secret", "--type", "idrac", "--tag", "a", "--tag", "b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "added h1") || !strings.Contains(errs, "creating") {
		t.Errorf("out=%q err=%q", out, errs)
	}
	h := mustLoad(t, cfg).Hosts["h1"]
	if h.IP != "192.0.2.10" || h.User != "admin" || h.Pass != "secret" || h.Type != detect.TypeIDRAC || len(h.Tags) != 2 {
		t.Fatalf("round trip: %+v", h)
	}

	out, _, err = runOob(t, cfg, "", "add", "h1", "--notes", "rack 4")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "updated h1 (notes)") {
		t.Errorf("out=%q", out)
	}
	h = mustLoad(t, cfg).Hosts["h1"]
	if h.User != "admin" || h.Pass != "secret" || h.IP != "192.0.2.10" || h.Type != detect.TypeIDRAC || h.Notes != "rack 4" || len(h.Tags) != 2 {
		t.Fatalf("partial update clobbered fields: %+v", h)
	}

	out, _, err = runOob(t, cfg, "newpw\n", "add", "h1", "192.0.2.11", "--pass-stdin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "updated h1 (ip, pass)") {
		t.Errorf("out=%q", out)
	}
	h = mustLoad(t, cfg).Hosts["h1"]
	if h.Pass != "newpw" || h.IP != "192.0.2.11" || h.User != "admin" {
		t.Fatalf("%+v", h)
	}
}

func TestAddValidation(t *testing.T) {
	cfg := tmpCfg(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"add", "h1", "192.0.2.1", "--pass", "x", "--type", "bogus"}, "ilo, idrac, supermicro, ipmi"},
		{[]string{"add", "h1", "bad ip", "--pass", "x"}, "not an IP address or hostname"},
		{[]string{"add", "bad name", "192.0.2.1", "--pass", "x"}, "spaces"},
		{[]string{"add", "--pass", "x", "--", "-x", "192.0.2.1"}, "start with '-'"},
		{[]string{"add", "power", "192.0.2.1", "--pass", "x"}, "command name"},
		{[]string{"add", "newhost", "--pass", "x"}, "needs an ip"},
		{[]string{"add", "h1", "192.0.2.1", "--port", "abc"}, "port must be a number"},
		{[]string{"add"}, "usage: oob add <name> [ip]"},
	} {
		_, _, err := runOob(t, cfg, "", c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err=%v want %q", c.args, err, c.want)
		}
	}
	if _, err := os.Stat(cfg); err == nil {
		t.Error("failed adds must not create the config")
	}
}

func TestImportCommandCSV(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Run("bom header blank", func(t *testing.T) {
		cfg := tmpCfg(t)
		f := write("ok.csv", "\xef\xbb\xbfName,ip,user\n\ncsv1,192.0.2.1,root,pw,2222,,note,x;y\n  \ncsv2,192.0.2.2\n")
		out, _, err := runOob(t, cfg, "", "import", f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "imported 2 (skipped 3 blank/header)") {
			t.Errorf("out=%q", out)
		}
		h := mustLoad(t, cfg).Hosts["csv1"]
		if h.Port != 2222 || len(h.Tags) != 2 || h.User != "root" {
			t.Errorf("%+v", h)
		}
	})
	t.Run("all or nothing", func(t *testing.T) {
		cfg := tmpCfg(t)
		f := write("bad.csv", "csv1,192.0.2.1\ngood,192.0.2.2\nnoport,192.0.2.3,u,p,abc\n,192.0.2.4\ncsv1,192.0.2.5\nsp ace,192.0.2.6\nnoip\n")
		_, _, err := runOob(t, cfg, "", "import", f)
		if err == nil {
			t.Fatal("want error")
		}
		for _, w := range []string{`row 3: port "abc" is not a number`, "row 4: blank name", "row 5: duplicate name csv1 (also row 1)", "row 6: name", "row 7: blank ip"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("missing %q in:\n%v", w, err)
			}
		}
		if _, err := os.Stat(cfg); err == nil {
			t.Error("config created despite errors")
		}
	})
	t.Run("existing skipped", func(t *testing.T) {
		cfg := tmpCfg(t)
		f := write("e.csv", "a,192.0.2.1\n")
		if _, _, err := runOob(t, cfg, "", "import", f); err != nil {
			t.Fatal(err)
		}
		out, _, err := runOob(t, cfg, "", "import", f)
		if err != nil || !strings.Contains(out, "skipped 1 existing: a") {
			t.Errorf("out=%q err=%v", out, err)
		}
	})
	t.Run("extra columns warn", func(t *testing.T) {
		cfg := tmpCfg(t)
		f := write("x.csv", "a,192.0.2.1,,,,,,,extra\n")
		_, errs, err := runOob(t, cfg, "", "import", f)
		if err != nil || !strings.Contains(errs, "warning: row 1") {
			t.Errorf("err=%v stderr=%q", err, errs)
		}
	})
}

func TestDidYouMean(t *testing.T) {
	cfg := tmpCfg(t)
	if _, _, err := runOob(t, cfg, "", "add", "web1", "192.0.2.1", "--pass", "x"); err != nil {
		t.Fatal(err)
	}
	_, _, err := runOob(t, cfg, "", "powr", "web1")
	if err == nil || !strings.Contains(err.Error(), `did you mean "power"`) {
		t.Errorf("err=%v", err)
	}
	_, _, err = runOob(t, cfg, "", "zzzzzzzz")
	if err == nil || strings.Contains(err.Error(), "did you mean") {
		t.Errorf("err=%v", err)
	}
}

func TestEditDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		d    int
	}{{"", "abc", 3}, {"power", "powr", 1}, {"kitten", "sitting", 3}, {"ls", "ls", 0}} {
		if got := editDistance(c.a, c.b); got != c.d {
			t.Errorf("%s,%s=%d want %d", c.a, c.b, got, c.d)
		}
	}
}

func TestMissingExplicitConfig(t *testing.T) {
	cfg := tmpCfg(t)
	_, _, err := runOob(t, cfg, "", "ls")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err=%v", err)
	}
	if _, _, err := runOob(t, cfg, "", "rm", "x"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("rm: err=%v", err)
	}
}

func TestRmAndLsFilters(t *testing.T) {
	cfg := tmpCfg(t)
	for _, a := range [][]string{
		{"add", "Web1", "192.0.2.1", "--pass", "x", "--tag", "Prod"},
		{"add", "db1", "192.0.2.2", "--pass", "x"},
	} {
		if _, _, err := runOob(t, cfg, "", a...); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		sel  string
		want string
		not  string
	}{{"web*", "Web1", "db1"}, {"WEB", "Web1", "db1"}, {"tag:prod", "Web1", "db1"}, {"DB1", "db1", "Web1"}} {
		out, _, err := runOob(t, cfg, "", "ls", c.sel)
		if err != nil || !strings.Contains(out, c.want) || strings.Contains(out, c.not) {
			t.Errorf("ls %s: %q %v", c.sel, out, err)
		}
	}
	if _, _, err := runOob(t, cfg, "", "rm", "web"); err == nil {
		t.Error("rm must need the exact name")
	}
	out, _, err := runOob(t, cfg, "", "rm", "Web1")
	if err != nil || !strings.Contains(out, "removed Web1") {
		t.Errorf("out=%q err=%v", out, err)
	}
}

func TestDestructiveNeedsConfirmation(t *testing.T) {
	cfg := tmpCfg(t)
	if _, _, err := runOob(t, cfg, "", "add", "web1", "192.0.2.1", "--pass", "x"); err != nil {
		t.Fatal(err)
	}
	// Non-TTY stdin and no --yes: refuse before touching the network.
	_, _, err := runOob(t, cfg, "", "power", "web1", "off")
	if err == nil || !strings.Contains(err.Error(), "confirmation required") {
		t.Errorf("err=%v", err)
	}
	// --yes with a prefix match is refused for destructive actions.
	_, _, err = runOob(t, cfg, "", "power", "web", "off", "--yes")
	if err == nil || !strings.Contains(err.Error(), "exact host name") {
		t.Errorf("err=%v", err)
	}
	_, _, err = runOob(t, cfg, "", "nmi", "web", "--yes")
	if err == nil || !strings.Contains(err.Error(), "exact host name") {
		t.Errorf("err=%v", err)
	}
}

func TestFriendlyArgErrorsAndCompletion(t *testing.T) {
	cfg := tmpCfg(t)
	_, _, err := runOob(t, cfg, "", "ipmi", "web1")
	if err == nil || !strings.Contains(err.Error(), "usage: oob ipmi <name> -- <ipmitool args...>") {
		t.Errorf("err=%v", err)
	}
	_, _, err = runOob(t, cfg, "", "completion", "tcsh")
	if err == nil {
		t.Error("unsupported shell must error")
	}
	out, _, err := runOob(t, cfg, "", "completion", "bash")
	if err != nil || !strings.Contains(out, "bash completion") {
		t.Errorf("bash completion: err=%v len=%d", err, len(out))
	}
}

func TestHostCompletionListsAll(t *testing.T) {
	cfg := tmpCfg(t)
	for _, n := range []string{"bad", "csv1", "csv2", "good"} {
		if _, _, err := runOob(t, cfg, "", "add", n, "192.0.2.1", "--pass", "x", "--notes", "multi\nline\tnote"); err != nil {
			t.Fatal(err)
		}
	}
	out, _, _ := runOob(t, cfg, "", "__complete", "power", "")
	for _, n := range []string{"bad\t", "csv1\t", "csv2\t", "good\t"} {
		if !strings.Contains(out, n) {
			t.Errorf("completion missing %q:\n%s", n, out)
		}
	}
	if strings.Contains(out, "line\tnote") {
		t.Errorf("control characters leaked:\n%s", out)
	}
}

func TestPermWarningNotBeforeParseError(t *testing.T) {
	cfg := tmpCfg(t)
	if err := os.WriteFile(cfg, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, w, _ := os.Pipe()
	swap(t, &os.Stderr, w)
	_, _, err := runOob(t, cfg, "", "ls")
	w.Close()
	var b bytes.Buffer
	b.ReadFrom(r)
	if err == nil || strings.Contains(b.String(), "mode") {
		t.Errorf("err=%v stderr=%q", err, b.String())
	}
}

func TestSelect(t *testing.T) {
	c := &config.Config{Hosts: map[string]config.Host{
		"web1": {Tags: []string{"prod"}}, "web2": {}, "db1": {Tags: []string{"prod", "db"}},
	}}
	cases := map[string][]string{
		"":         {"db1", "web1", "web2"},
		"tag:prod": {"db1", "web1"},
		"web*":     {"web1", "web2"},
		"db":       {"db1"},
		"WEB*":     {"web1", "web2"},
		"TAG:Prod": {"db1", "web1"},
	}
	for sel, want := range cases {
		if got := selectHosts(c, sel); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v want %v", sel, got, want)
		}
	}
}

func TestAddKeyFlagMergeSemantics(t *testing.T) {
	cfg := tmpCfg(t)
	// --key on a new host skips the password prompt (stdin is not a terminal here)
	if _, _, err := runOob(t, cfg, "", "add", "h1", "192.0.2.10", "--user", "admin", "--key", "~/.ssh/id_x"); err != nil {
		t.Fatal(err)
	}
	if h := mustLoad(t, cfg).Hosts["h1"]; h.Key != "~/.ssh/id_x" || h.Pass != "" {
		t.Fatalf("after add: %+v", h)
	}
	// a later add without --key leaves it alone
	if _, _, err := runOob(t, cfg, "", "add", "h1", "--notes", "n"); err != nil {
		t.Fatal(err)
	}
	if h := mustLoad(t, cfg).Hosts["h1"]; h.Key != "~/.ssh/id_x" {
		t.Fatalf("key lost: %+v", h)
	}
	// --key "" clears it
	if _, _, err := runOob(t, cfg, "", "add", "h1", "--key", "", "--pass", "x"); err != nil {
		t.Fatal(err)
	}
	if h := mustLoad(t, cfg).Hosts["h1"]; h.Key != "" {
		t.Fatalf("key not cleared: %+v", h)
	}
}

func TestEditHostFields(t *testing.T) {
	cfg := tmpCfg(t)
	if _, _, err := runOob(t, cfg, "", "add", "node01", "192.0.2.10", "--user", "admin", "--pass", "secret"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runOob(t, cfg, "", "edit", "node01", "--note", "rack 4")
	if err != nil || !strings.Contains(out, "updated node01 (notes)") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	out, _, err = runOob(t, cfg, "", "edit", "node01", "--ip", "192.0.2.20", "--notes", "moved")
	if err != nil || !strings.Contains(out, "updated node01 (ip, notes)") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	h := mustLoad(t, cfg).Hosts["node01"]
	if h.Notes != "moved" || h.IP != "192.0.2.20" || h.User != "admin" || h.Pass != "secret" {
		t.Fatalf("edit clobbered fields: %+v", h)
	}
	// edit never creates a host, so a typo can't add a stray entry.
	if _, _, err := runOob(t, cfg, "", "edit", "nod01", "--note", "x"); err == nil || !strings.Contains(err.Error(), "no such host") {
		t.Fatalf("edit of a missing host: %v", err)
	}
	if _, ok := mustLoad(t, cfg).Hosts["nod01"]; ok {
		t.Fatal("edit created a host")
	}
	// Field flags without a name are an error, not an editor launch.
	if _, _, err := runOob(t, cfg, "", "edit", "--note", "x"); err == nil || !strings.Contains(err.Error(), "need a host name") {
		t.Fatalf("edit without name: %v", err)
	}
}

func TestHelpGroups(t *testing.T) {
	root := newRoot()
	for _, c := range root.Commands() {
		if c.GroupID == "" {
			t.Errorf("command %q has no help group", c.Name())
		}
	}
}

func TestEmptyAddressRejected(t *testing.T) {
	cfg := tmpCfg(t)
	if _, _, err := runOob(t, cfg, "", "add", "node01", "192.0.2.10", "--user", "admin", "--pass", "secret"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"edit", "node01", "--ip", ""},
		{"add", "node01", "", "--pass", "x"},
	} {
		if _, _, err := runOob(t, cfg, "", args...); err == nil {
			t.Errorf("%v: want an error for an empty address", args)
		}
	}
	if h := mustLoad(t, cfg).Hosts["node01"]; h.IP != "192.0.2.10" || h.Pass != "secret" {
		t.Fatalf("host changed: %+v", h)
	}
}

func TestEditWithGlobalFlagsOpensEditor(t *testing.T) {
	cfg := tmpCfg(t)
	if _, _, err := runOob(t, cfg, "", "add", "node01", "192.0.2.10", "--user", "admin", "--pass", "secret"); err != nil {
		t.Fatal(err)
	}
	// runOob always passes -c; with -y too, plain edit must still reach the
	// editor (here `true`, which exits 0) rather than demand a host name.
	t.Setenv("VISUAL", "true")
	if _, _, err := runOob(t, cfg, "", "-y", "edit"); err != nil {
		t.Fatalf("edit with -c/-y: %v", err)
	}
}

type fakeStatus struct{ got *time.Duration }

func (f fakeStatus) PowerStatus(d time.Duration) bmc.StatusResult {
	*f.got = d
	return bmc.StatusResult{Text: "on"}
}

func TestPowerStatusPassesOnlyRemainingDeadline(t *testing.T) {
	var got time.Duration
	called := false
	swap(t, &driverFor, func(string, config.Host) interface {
		PowerStatus(time.Duration) bmc.StatusResult
	} {
		called = true
		return fakeStatus{&got}
	})
	swap(t, &resolveHost, func(_ string, h config.Host, _ time.Duration) config.Host { return h })
	if r := powerStatus("node01", config.Host{IP: "192.0.2.1", Type: "ipmi"}, 100*time.Millisecond); r.Failed || !called {
		t.Fatalf("result %+v called %v", r, called)
	}
	if got <= 0 || got > 100*time.Millisecond {
		t.Errorf("typed host got %v, want at most 100ms", got)
	}
}

func TestPowerStatusExhaustedBudgetTimesOut(t *testing.T) {
	called := false
	swap(t, &driverFor, func(string, config.Host) interface {
		PowerStatus(time.Duration) bmc.StatusResult
	} {
		called = true
		return fakeStatus{new(time.Duration)}
	})
	swap(t, &resolveHost, func(_ string, h config.Host, _ time.Duration) config.Host {
		time.Sleep(60 * time.Millisecond) // the probe used the whole budget
		return h
	})
	r := powerStatus("node01", config.Host{IP: "192.0.2.1"}, 50*time.Millisecond)
	if called || !r.Failed || !strings.Contains(r.Text, "timed out") {
		t.Errorf("result %+v, PowerStatus called %v", r, called)
	}
}
