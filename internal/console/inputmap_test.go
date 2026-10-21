package console

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestMapVSPInput(t *testing.T) {
	const esc = "\x1b"
	for _, tc := range []struct {
		name, in, out string
		note          bool
	}{
		{"plain text", "ls -l\r", "ls -l\r", false},
		{"lone esc", esc, esc + esc, false},
		{"lone esc after text", "ab" + esc, "ab" + esc + esc, false},
		{"two esc keys", esc + esc, esc + esc + esc + esc, false},
		{"csi arrow", esc + "[A", "", true},
		{"csi with params", esc + "[1;5C", "", true},
		{"csi tilde", esc + "[3~", "", true},
		{"csi cut off", "a" + esc + "[1;", "a", true},
		{"ss3", esc + "OP", "", true},
		{"ss3 cut off", esc + "O", "", true},
		{"alt key", esc + "x", "", true},
		{"alt paren is never sent", esc + "(", "", true},
		{"text around an arrow", "a" + esc + "[Ab", "ab", true},
		{"focus in", esc + "[I", "", false},
		{"focus out", esc + "[O", "", false},
		{"focus then text", esc + "[Ix", "x", false},
		{"esc then arrow", esc + esc + "[A", esc + esc, true}, // the first is lone, the arrow is dropped
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, note := MapVSPInput([]byte(tc.in))
			if string(out) != tc.out {
				t.Errorf("out = %q, want %q", out, tc.out)
			}
			if (note != "") != tc.note {
				t.Errorf("note = %q, want note: %v", note, tc.note)
			}
			if note != "" && note != VSPKeysNote {
				t.Errorf("note = %q", note)
			}
			if bytes.Contains(out, []byte(esc+"(")) {
				t.Errorf("ESC ( produced: %q", out)
			}
		})
	}
}

func TestStripEsc(t *testing.T) {
	for in, want := range map[string]string{
		"echo hi":                 "echo hi",
		"a\x1b[31mred\x1b[0m b":   "ared b",
		"x\x1bOPy":                "xy",
		"p\x1bqr":                 "pr",
		"end\x1b":                 "end",
		"\x1b(vsp":                "vsp",
		"line1\nline2\x1b[200~ok": "line1\nline2ok",
	} {
		if got := StripEsc(in); got != want {
			t.Errorf("StripEsc(%q) = %q, want %q", in, got, want)
		}
	}
}

// vspLink is a fakeLink that maps input like an iLO VSP in the console.
type vspLink struct {
	fakeLink
}

func (*vspLink) MapInput(b []byte) ([]byte, string) { return MapVSPInput(b) }

func vspKeys(t *testing.T, l Link) (*consoleKeys, *console) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	con := newConsole(&bytes.Buffer{}, "h", "vsp", 24, 80)
	con.Start()
	t.Cleanup(func() { con.Close() })
	k := newKeys(&Setup{Link: l}, con, func() {}, make(chan struct{}))
	k.start(nil)
	return k, con
}

func barNote(con *console) string {
	con.mu.Lock()
	defer con.mu.Unlock()
	return con.st.note
}

func TestConsoleKeysOnAVSPLink(t *testing.T) {
	fl := &vspLink{}
	k, con := vspKeys(t, fl)
	sent := func() string { fl.mu.Lock(); defer fl.mu.Unlock(); return string(fl.sent) }
	wait := func(want string) {
		t.Helper()
		for i := 0; i < 200 && sent() != want; i++ {
			time.Sleep(5 * time.Millisecond)
		}
		if got := sent(); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}

	// The Esc key: held, then flushed alone, and typed as ESC ESC.
	k.Input([]byte("\x1b"))
	if !k.parser.Pending() {
		t.Fatal("lone ESC should be held")
	}
	k.Flush()
	wait("\x1b\x1b")
	if n := barNote(con); n != "" {
		t.Errorf("a lone ESC gave a note: %q", n)
	}

	// An arrow key is dropped, with the note.
	k.Input([]byte("a\x1b[Ab"))
	wait("\x1b\x1bab")
	if n := barNote(con); n != VSPKeysNote {
		t.Errorf("bar note = %q, want the VSP note", n)
	}

	// The note shows once per session: clear it, press another, and it stays clear.
	con.mu.Lock()
	con.st.note = ""
	con.mu.Unlock()
	k.Input([]byte("\x1bOP"))
	k.Input([]byte("\x1bxc"))
	wait("\x1b\x1babc")
	if n := barNote(con); n != "" {
		t.Errorf("the note showed twice: %q", n)
	}

	// A paste has its escape sequences stripped, and types the rest.
	k.Input([]byte("\x1b[200~echo \x1b[31mhi\x1b[201~"))
	wait("\x1b\x1babcecho hi")
}

func TestConsoleKeysOnANonVSPLinkAreUnchanged(t *testing.T) {
	fl := &fakeLink{}
	k, con := vspKeys(t, fl)
	k.Input([]byte("\x1b[Ax"))
	k.Input([]byte("\x1b"))
	k.Flush()
	for i := 0; i < 200 && len(fl.sentCopy()) < 5; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if got := string(fl.sentCopy()); got != "\x1b[Ax\x1b" {
		t.Errorf("sent %q, want the bytes as typed", got)
	}
	if n := barNote(con); strings.Contains(n, "VSP") {
		t.Errorf("note %q", n)
	}
}

func (f *fakeLink) sentCopy() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.sent...)
}
