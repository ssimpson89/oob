package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ssimpson89/oob/internal/app"
)

func TestSaveLoadRoundTripAndPerms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "hosts.json")
	c := &Config{Hosts: map[string]Host{"a": {IP: "192.0.2.1", User: "u", Tags: []string{"rack1"}}}}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", st.Mode().Perm())
	}
	d, _ := os.Stat(filepath.Dir(p))
	if d.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", d.Mode().Perm())
	}
	got, err := LoadConfig(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Hosts, c.Hosts) {
		t.Errorf("got %+v", got.Hosts)
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	c, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json"), nil)
	if err != nil || len(c.Hosts) != 0 {
		t.Fatalf("%v %v", c, err)
	}
}

func TestLoadWarnsOnLoosePerms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts.json")
	if err := os.WriteFile(p, []byte(`{"hosts":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var msg string
	if _, err := LoadConfig(p, func(s string) { msg = s }); err != nil {
		t.Fatal(err)
	}
	if msg == "" {
		t.Error("expected warning")
	}
	os.Chmod(p, 0o600)
	msg = ""
	LoadConfig(p, func(s string) { msg = s })
	if msg != "" {
		t.Errorf("unexpected warning %q", msg)
	}
}

func TestLoadBadJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts.json")
	os.WriteFile(p, []byte(`{`), 0o600)
	if _, err := LoadConfig(p, nil); err == nil {
		t.Error("expected error")
	}
}

func TestDefaultsMerge(t *testing.T) {
	c := &Config{
		Defaults: Host{User: "admin", Pass: "defpass", Cipher: "17"},
		Hosts: map[string]Host{
			"a": {IP: "192.0.2.1"},
			"b": {IP: "192.0.2.2", User: "root", Pass: "x", Port: 2222, Cipher: "3"},
		},
	}
	_, a, _ := c.Lookup("a")
	if a.User != "admin" || a.Pass != "defpass" || a.Cipher != "17" || a.Port != 22 {
		t.Errorf("a: %+v", a)
	}
	_, b, _ := c.Lookup("b")
	if b.User != "root" || b.Pass != "x" || b.Cipher != "3" || b.Port != 2222 {
		t.Errorf("b: %+v", b)
	}
}

func TestLookupPrefixAndAmbiguity(t *testing.T) {
	c := &Config{Hosts: map[string]Host{"node01": {}, "node02": {}, "stor1": {}}}
	if n, _, err := c.Lookup("stor"); err != nil || n != "stor1" {
		t.Errorf("%v %v", n, err)
	}
	if _, _, err := c.Lookup("node"); err == nil {
		t.Error("expected ambiguity")
	}
	if n, _, err := c.Lookup("node01"); err != nil || n != "node01" {
		t.Errorf("%v %v", n, err)
	}
	if _, _, err := c.Lookup("zzz"); err == nil {
		t.Error("expected not found")
	}
}

func TestConfigPath(t *testing.T) {
	t.Setenv("OOB_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/x")
	p, _ := ConfigPath("")
	if p != filepath.Join("/x", app.Name, "hosts.json") {
		t.Error(p)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/h")
	p, _ = ConfigPath("")
	if p != filepath.Join("/h", ".config", app.Name, "hosts.json") {
		t.Error(p)
	}
	if p, _ = ConfigPath("/o.json"); p != "/o.json" {
		t.Error(p)
	}
}

func TestLookupEmptyAndPrefix(t *testing.T) {
	c := &Config{Hosts: map[string]Host{"rack1-a": {IP: "1"}, "rack1-b": {IP: "2"}, "other": {IP: "3"}}}
	if _, _, err := c.Lookup(""); err == nil {
		t.Error("empty name should be rejected")
	}
	if n, _, err := c.Lookup("oth"); err != nil || n != "other" {
		t.Errorf("prefix: %q %v", n, err)
	}
	if _, _, err := c.Lookup("rack1"); err == nil {
		t.Error("ambiguous prefix should fail")
	}
	if n, _, err := c.Lookup("rack1-a"); err != nil || n != "rack1-a" {
		t.Errorf("exact: %q %v", n, err)
	}
}

func TestLookupExactAndSelect(t *testing.T) {
	c := &Config{
		Defaults: Host{User: "admin", Pass: "pw"},
		Hosts: map[string]Host{
			"node01": {IP: "192.0.2.1", Tags: []string{"rack1"}},
			"node02": {IP: "192.0.2.2", Tags: []string{"rack2"}},
			"login1": {IP: "192.0.2.3"},
		},
	}
	h, err := c.LookupExact("node01")
	if err != nil || h.IP != "192.0.2.1" || h.User != "admin" || h.Name != "node01" {
		t.Fatalf("LookupExact(node01) = %+v, %v", h, err)
	}
	if _, err := c.LookupExact("login"); err == nil {
		t.Error("LookupExact matched by prefix")
	}
	for sel, want := range map[string]string{
		"":          "login1 node01 node02",
		"node*":     "node01 node02",
		"tag:rack2": "node02",
		"LOGIN":     "login1",
		"nothing":   "",
	} {
		if got := strings.Join(c.Select(sel), " "); got != want {
			t.Errorf("Select(%q) = %q, want %q", sel, got, want)
		}
	}
}

func TestTypingDelaysComeFromTheFileAndDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts.json")
	doc := `{"defaults":{"line_delay_ms":300},"hosts":{"a":{"ip":"192.0.2.1","char_delay_ms":20},"b":{"ip":"192.0.2.2","char_delay_ms":0,"line_delay_ms":0}}}`
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, a, _ := c.Lookup("a")
	if a.CharDelayMS == nil || *a.CharDelayMS != 20 || a.LineDelayMS == nil || *a.LineDelayMS != 300 {
		t.Errorf("a = %v, %v; want its own 20 and the default 300", a.CharDelayMS, a.LineDelayMS)
	}
	_, b, _ := c.Lookup("b")
	if b.CharDelayMS == nil || *b.CharDelayMS != 0 || b.LineDelayMS == nil || *b.LineDelayMS != 0 {
		t.Errorf("b = %v, %v; an explicit 0 must stay 0, not fall back", b.CharDelayMS, b.LineDelayMS)
	}
	c.Defaults.CharDelayMS = nil
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), `"char_delay_ms": 0`) {
		t.Errorf("an explicit 0 was dropped on save: %s", raw)
	}
}
