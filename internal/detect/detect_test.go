package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ssimpson89/oob/internal/config"
)

func TestDetectVendor(t *testing.T) {
	for _, c := range []struct {
		in  string
		typ string
		gen int
	}{
		{"Integrated Lights-Out 7\r\n", TypeILO, 7},
		{"integrated lights-out", TypeILO, 0},
		{"</>hpeiLO-> ", "", 0},
		{"/admin1-> ", TypeIDRAC, 0},
		{"admin1-> ", TypeIDRAC, 0},
		{"racadm>> ", TypeIDRAC, 0},
		{"Welcome to iDRAC9", TypeIDRAC, 0},
		{"SMASH-CLP Server Management Shell", TypeSupermicro, 0},
		{"ATEN International", TypeSupermicro, 0},
		{"/-> ", "", 0},
		{"Welcome to Dell iDRAC9\r\n", TypeIDRAC, 0},
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

	if NewDetector("a", config.Host{Type: TypeILO, ILOGen: 7}) != nil {
		t.Error("iLO with a known generation should not detect")
	}
	for _, ty := range []string{TypeIPMI, TypeIDRAC, TypeSupermicro} {
		if NewDetector("a", config.Host{Type: ty}) != nil {
			t.Errorf("typed host %s should not detect", ty)
		}
	}
	var nilDet *Detector
	nilDet.Feed([]byte("Integrated Lights-Out")) // must not panic
	nilDet.Banner("x")

	d := NewDetector("a", config.Host{})
	d.Feed([]byte("welcome\r\nIntegrated Lig"))
	d.Feed([]byte("hts-Out 7\r\n"))
	d.Feed([]byte("Integrated Lights-Out again"))
	d.Banner("Integrated Lights-Out 7")
	if len(marked) != 1 || marked[0] != (mark{"a", TypeILO, 7}) {
		t.Errorf("marked %v, want one iLO gen 7 call for a", marked)
	}

	marked = nil
	d2 := NewDetector("b", config.Host{})
	d2.Feed([]byte("OpenSSH banner\r\n"))
	d2.Banner("Authorized users only")
	d2.Feed([]byte("\x1b[0m/admin1-> "))
	if len(marked) != 1 || marked[0].typ != TypeIDRAC {
		t.Errorf("idrac: %v", marked)
	}

	// The banner alone saves nothing; the first output after login does.
	marked = nil
	d3 := NewDetector("c", config.Host{})
	d3.Banner("SMASH-CLP")
	if len(marked) != 0 {
		t.Errorf("banner alone saved %v", marked)
	}
	d3.Feed([]byte("login ok\r\n"))
	if len(marked) != 1 || marked[0].typ != TypeSupermicro {
		t.Errorf("supermicro: %v", marked)
	}

	// The banner reaches a different detector for the same host (oob ssh).
	marked = nil
	NewDetector("c2", config.Host{}).Banner("SMASH-CLP")
	NewDetector("c2", config.Host{}).Feed(nil) // logged in, said nothing
	if len(marked) != 1 || marked[0] != (mark{"c2", TypeSupermicro, 0}) {
		t.Errorf("banner handover: %v", marked)
	}

	// An iLO host missing its generation learns it, but ignores other vendors.
	marked = nil
	d4 := NewDetector("d", config.Host{Type: TypeILO})
	d4.Feed([]byte("/admin1-> "))
	d4.Feed([]byte("Integrated Lights-Out 5"))
	if len(marked) != 0 {
		t.Errorf("window already closed at the first prompt: %v", marked)
	}
	d4 = NewDetector("d", config.Host{Type: TypeILO})
	d4.Feed([]byte("Integrated Lights-Out 5"))
	if len(marked) != 1 || marked[0] != (mark{"d", TypeILO, 5}) {
		t.Errorf("gen learning: %v", marked)
	}
}

func TestDetectorWindow(t *testing.T) {
	var marked []string
	swap(t, &markTypeFn, func(n, typ string, gen int) error { marked = append(marked, typ); return nil })

	// A prompt that is not a vendor's closes the window; a later iDRAC mention is ignored.
	d := NewDetector("w1", config.Host{})
	d.Feed([]byte("Last login: Tue\r\nuser@host:~$ "))
	d.Feed([]byte("cat notes\r\nthis box talks to iDRAC9 and /admin1-> too\r\n"))
	if len(marked) != 0 {
		t.Errorf("late output detected: %v", marked)
	}

	// Past 4KB of output without a prompt, nothing is detected either.
	d = NewDetector("w2", config.Host{})
	d.Feed([]byte(strings.Repeat("x", detectWindow)))
	d.Feed([]byte("\r\nWelcome to iDRAC9\r\n"))
	if len(marked) != 0 {
		t.Errorf("output past the window detected: %v", marked)
	}

	// Within the window a match across reads still counts.
	d = NewDetector("w3", config.Host{})
	d.Feed([]byte("banner text\r\nWelcome to iD"))
	d.Feed([]byte("RAC9\r\n"))
	if len(marked) != 1 || marked[0] != TypeIDRAC {
		t.Errorf("split match: %v", marked)
	}
}

func TestMarkTypeGenOnTypedHost(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts.json")
	c := &config.Config{Hosts: map[string]config.Host{"a": {IP: "192.0.2.1", Type: TypeILO}, "b": {IP: "192.0.2.2"}}}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	swap(t, &config.Override, func() string { return p })
	clearNotices(t)
	if err := markType("a", TypeILO, 6); err != nil {
		t.Fatal(err)
	}
	if err := markType("b", TypeSupermicro, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := config.LoadConfig(p, nil)
	if got.Hosts["a"].ILOGen != 6 || got.Hosts["a"].Type != TypeILO {
		t.Errorf("a: %+v", got.Hosts["a"])
	}
	if got.Hosts["b"].Type != TypeSupermicro || got.Hosts["b"].ILOGen != 0 {
		t.Errorf("b: %+v", got.Hosts["b"])
	}
}

func TestMarkTypeKeepsConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts.json")
	c := &config.Config{
		Defaults: config.Host{User: "admin", Pass: "pw-default", Cipher: "17"},
		Hosts: map[string]config.Host{
			"a": {IP: "192.0.2.1", Notes: "n", Tags: []string{"x"}},
			"b": {IP: "192.0.2.2", Pass: "pw-b", Type: TypeIPMI},
		},
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	swap(t, &config.Override, func() string { return p })
	clearNotices(t)

	if err := markType("a", TypeILO, 7); err != nil {
		t.Fatal(err)
	}
	if err := markType("b", TypeILO, 7); err != nil { // already typed: untouched
		t.Fatal(err)
	}
	if err := markType("missing", TypeILO, 0); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadConfig(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hosts["a"].Type != TypeILO || got.Hosts["a"].ILOGen != 7 || got.Hosts["a"].Notes != "n" || got.Hosts["a"].IP != "192.0.2.1" || len(got.Hosts["a"].Tags) != 1 {
		t.Errorf("a: %+v", got.Hosts["a"])
	}
	if got.Hosts["b"].Type != TypeIPMI || got.Hosts["b"].Pass != "pw-b" {
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

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		name, banner, out string
		typ               string
		gen               int
	}{
		{"ilo banner", "Integrated Lights-Out 7\n", "", TypeILO, 7},
		{"ilo gen from banner, output without", "Integrated Lights-Out 6\n", "</>hpeiLO-> ", TypeILO, 6},
		{"ilo in output", "", "Integrated Lights-Out 5\r\n</>hpeiLO-> ", TypeILO, 5},
		{"idrac prompt", "", "\r\n/admin1-> ", TypeIDRAC, 0},
		{"smash", "", "SMASH-CLP shell", TypeSupermicro, 0},
		{"nothing", "Welcome\n", "user@node01:~$ ", "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			typ, gen := Classify(c.banner, c.out)
			if typ != c.typ || gen != c.gen {
				t.Errorf("got %q %d want %q %d", typ, gen, c.typ, c.gen)
			}
		})
	}
	if !WindowDone("x\r\n</>hpeiLO-> ") || WindowDone("Welcome to the BMC") {
		t.Error("WindowDone prompt detection")
	}
	if VendorLabel(TypeILO, 7) != "HPE iLO 7" || VendorLabel(TypeIDRAC, 0) != "Dell iDRAC" {
		t.Error("VendorLabel")
	}
}
