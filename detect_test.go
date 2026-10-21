package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectVendor(t *testing.T) {
	for _, c := range []struct {
		in  string
		typ string
		gen int
	}{
		{"Integrated Lights-Out 7\r\n", typeILO, 7},
		{"integrated lights-out", typeILO, 0},
		{"</>hpeiLO-> ", "", 0},
		{"/admin1-> ", typeIDRAC, 0},
		{"admin1-> ", typeIDRAC, 0},
		{"racadm>> ", typeIDRAC, 0},
		{"Welcome to iDRAC9", typeIDRAC, 0},
		{"SMASH-CLP Server Management Shell", typeSupermicro, 0},
		{"ATEN International", typeSupermicro, 0},
		{"/-> ", "", 0},
		{"Welcome to Dell iDRAC9\r\n", typeIDRAC, 0},
		{"see the idracadm docs", "", 0},
		{"not a prompt: admin1-> in the middle", "", 0},
		{"rate limiting latency", "", 0},
		{"OpenSSH banner", "", 0},
	} {
		typ, gen := detectVendor(c.in)
		if typ != c.typ || gen != c.gen {
			t.Errorf("%q: got %q/%d want %q/%d", c.in, typ, gen, c.typ, c.gen)
		}
	}
}

func TestDetector(t *testing.T) {
	type mark struct {
		name, typ string
		gen       int
	}
	var marked []mark
	swap(t, &markTypeFn, func(n, typ string, gen int) error { marked = append(marked, mark{n, typ, gen}); return nil })

	if newDetector("a", Host{Type: typeILO, ILOGen: 7}) != nil {
		t.Error("iLO with a known generation should not detect")
	}
	for _, ty := range []string{typeIPMI, typeIDRAC, typeSupermicro} {
		if newDetector("a", Host{Type: ty}) != nil {
			t.Errorf("typed host %s should not detect", ty)
		}
	}
	var nilDet *detector
	nilDet.Feed([]byte("Integrated Lights-Out")) // must not panic
	nilDet.Banner("x")

	d := newDetector("a", Host{})
	d.Feed([]byte("welcome\r\nIntegrated Lig"))
	d.Feed([]byte("hts-Out 7\r\n"))
	d.Feed([]byte("Integrated Lights-Out again"))
	d.Banner("Integrated Lights-Out 7")
	if len(marked) != 1 || marked[0] != (mark{"a", typeILO, 7}) {
		t.Errorf("marked %v, want one iLO gen 7 call for a", marked)
	}

	marked = nil
	d2 := newDetector("b", Host{})
	d2.Feed([]byte("OpenSSH banner\r\n"))
	d2.Banner("Authorized users only")
	d2.Feed([]byte("\x1b[0m/admin1-> "))
	if len(marked) != 1 || marked[0].typ != typeIDRAC {
		t.Errorf("idrac: %v", marked)
	}

	// The banner alone saves nothing; the first output after login does.
	marked = nil
	d3 := newDetector("c", Host{})
	d3.Banner("SMASH-CLP")
	if len(marked) != 0 {
		t.Errorf("banner alone saved %v", marked)
	}
	d3.Feed([]byte("login ok\r\n"))
	if len(marked) != 1 || marked[0].typ != typeSupermicro {
		t.Errorf("supermicro: %v", marked)
	}

	// The banner reaches a different detector for the same host (oob ssh).
	marked = nil
	newDetector("c2", Host{}).Banner("SMASH-CLP")
	newDetector("c2", Host{}).Feed(nil) // logged in, said nothing
	if len(marked) != 1 || marked[0] != (mark{"c2", typeSupermicro, 0}) {
		t.Errorf("banner handover: %v", marked)
	}

	// An iLO host missing its generation learns it, but ignores other vendors.
	marked = nil
	d4 := newDetector("d", Host{Type: typeILO})
	d4.Feed([]byte("/admin1-> "))
	d4.Feed([]byte("Integrated Lights-Out 5"))
	if len(marked) != 0 {
		t.Errorf("window already closed at the first prompt: %v", marked)
	}
	d4 = newDetector("d", Host{Type: typeILO})
	d4.Feed([]byte("Integrated Lights-Out 5"))
	if len(marked) != 1 || marked[0] != (mark{"d", typeILO, 5}) {
		t.Errorf("gen learning: %v", marked)
	}
}

func TestDetectorWindow(t *testing.T) {
	var marked []string
	swap(t, &markTypeFn, func(n, typ string, gen int) error { marked = append(marked, typ); return nil })

	// A prompt that is not a vendor's closes the window; a later iDRAC mention is ignored.
	d := newDetector("w1", Host{})
	d.Feed([]byte("Last login: Tue\r\nuser@host:~$ "))
	d.Feed([]byte("cat notes\r\nthis box talks to iDRAC9 and /admin1-> too\r\n"))
	if len(marked) != 0 {
		t.Errorf("late output detected: %v", marked)
	}

	// Past 4KB of output without a prompt, nothing is detected either.
	d = newDetector("w2", Host{})
	d.Feed([]byte(strings.Repeat("x", detectWindow)))
	d.Feed([]byte("\r\nWelcome to iDRAC9\r\n"))
	if len(marked) != 0 {
		t.Errorf("output past the window detected: %v", marked)
	}

	// Within the window a match across reads still counts.
	d = newDetector("w3", Host{})
	d.Feed([]byte("banner text\r\nWelcome to iD"))
	d.Feed([]byte("RAC9\r\n"))
	if len(marked) != 1 || marked[0] != typeIDRAC {
		t.Errorf("split match: %v", marked)
	}
}

func TestMarkTypeGenOnTypedHost(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts.json")
	c := &Config{Hosts: map[string]Host{"a": {IP: "192.0.2.1", Type: typeILO}, "b": {IP: "192.0.2.2"}}}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	swap(t, &cfgFlag, p)
	clearNotices(t)
	if err := markType("a", typeILO, 6); err != nil {
		t.Fatal(err)
	}
	if err := markType("b", typeSupermicro, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := LoadConfig(p, nil)
	if got.Hosts["a"].ILOGen != 6 || got.Hosts["a"].Type != typeILO {
		t.Errorf("a: %+v", got.Hosts["a"])
	}
	if got.Hosts["b"].Type != typeSupermicro || got.Hosts["b"].ILOGen != 0 {
		t.Errorf("b: %+v", got.Hosts["b"])
	}
}

func TestMarkTypeKeepsConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts.json")
	c := &Config{
		Defaults: Host{User: "admin", Pass: "pw-default", Cipher: "17"},
		Hosts: map[string]Host{
			"a": {IP: "192.0.2.1", Notes: "n", Tags: []string{"x"}},
			"b": {IP: "192.0.2.2", Pass: "pw-b", Type: typeIPMI},
		},
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	swap(t, &cfgFlag, p)
	clearNotices(t)

	if err := markType("a", typeILO, 7); err != nil {
		t.Fatal(err)
	}
	if err := markType("b", typeILO, 7); err != nil { // already typed: untouched
		t.Fatal(err)
	}
	if err := markType("missing", typeILO, 0); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hosts["a"].Type != typeILO || got.Hosts["a"].ILOGen != 7 || got.Hosts["a"].Notes != "n" || got.Hosts["a"].IP != "192.0.2.1" || len(got.Hosts["a"].Tags) != 1 {
		t.Errorf("a: %+v", got.Hosts["a"])
	}
	if got.Hosts["b"].Type != typeIPMI || got.Hosts["b"].Pass != "pw-b" {
		t.Errorf("b: %+v", got.Hosts["b"])
	}
	if d := got.Defaults; d.User != "admin" || d.Pass != "pw-default" || d.Cipher != "17" {
		t.Errorf("defaults changed: %+v", got.Defaults)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	notices.Lock()
	defer notices.Unlock()
	if len(notices.msgs) != 1 || strings.Contains(strings.Join(notices.msgs, ""), "pw-") {
		t.Errorf("notices %q", notices.msgs)
	}
}
