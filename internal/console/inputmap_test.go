package console

import (
	"bytes"
	"strings"
	"sync/atomic"
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
		{"csi arrow", esc + "[A", "\x10", false},
		{"csi with params", esc + "[1;5C", "", true},
		{"delete is dropped", esc + "[3~", "", true},
		{"function key", esc + "[15~", "", true},
		{"csi cut off", "a" + esc + "[1;", "a", true},
		{"ss3", esc + "OP", "", true},
		{"ss3 cut off", esc + "O", "", true},
		{"esc then a printable key", esc + "x", esc + esc + "x", false},
		{"fast esc :wq", esc + ":wq\r", esc + esc + ":wq\r", false},
		{"alt paren is never sent", esc + "(", "", true},
		{"text around an arrow", "a" + esc + "[Ab", "a\x10b", false},
		{"text around a function key", "a" + esc + "OPb", "ab", true},
		{"focus in", esc + "[I", "", false},
		{"focus out", esc + "[O", "", false},
		{"focus then text", esc + "[Ix", "x", false},
		{"esc then arrow", esc + esc + "[A", esc + esc + "\x10", false}, // the first is lone
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, note := MapVSPInput([]byte(tc.in), false)
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

func TestMapVSPArrowsAsReadline(t *testing.T) {
	const esc = "\x1b"
	for _, tc := range []struct{ in, ctl string }{
		{"[A", "\x10"}, {"OA", "\x10"},
		{"[B", "\x0e"}, {"OB", "\x0e"},
		{"[C", "\x06"}, {"OC", "\x06"},
		{"[D", "\x02"}, {"OD", "\x02"},
		{"[H", "\x01"}, {"OH", "\x01"}, {"[1~", "\x01"}, {"[7~", "\x01"},
		{"[F", "\x05"}, {"OF", "\x05"}, {"[4~", "\x05"}, {"[8~", "\x05"},
	} {
		out, note := MapVSPInput([]byte(esc+tc.in), false)
		if string(out) != tc.ctl || note != "" {
			t.Errorf("shell %q: got %q %q, want %q and no note", tc.in, out, note, tc.ctl)
		}
		out, note = MapVSPInput([]byte(esc+tc.in), true)
		if len(out) != 0 || note != VSPKeysNote {
			t.Errorf("full screen %q: got %q %q, want it dropped with the note", tc.in, out, note)
		}
	}
	// Delete is never mapped, even at a shell (ctrl+d would end the shell).
	if out, note := MapVSPInput([]byte(esc+"[3~"), false); len(out) != 0 || note == "" {
		t.Errorf("delete: got %q %q", out, note)
	}
}

func TestAltTrackerAcrossSplitOutput(t *testing.T) {
	l := &vspLink{}
	var tr AltTracker
	for _, c := range []struct {
		chunk string
		want  bool
	}{
		{"prompt$ vim\r\n", false},
		{"\x1b[?10", false},
		{"49h~\r\n", true},
		{"\x1b[?25l", true},
		{"\x1b[?1049", true},
		{"l$ ", false},
		{"\x1b[?47h", true},
		{"\x1b[?47l", false},
		{"\x1b[?1047h", true},
		{"\x1b[?1047l", false},
	} {
		tr.Follow(l, []byte(c.chunk))
		if got := l.alt.Load(); got != c.want {
			t.Fatalf("after %q: alt = %v, want %v", c.chunk, got, c.want)
		}
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
	alt atomic.Bool
}

func (l *vspLink) MapInput(b []byte) ([]byte, string) { return MapVSPInput(b, l.alt.Load()) }
func (l *vspLink) SetAltScreen(on bool)               { l.alt.Store(on) }
func (*vspLink) Serial() bool                         { return true } // typing is paced, as on the real VSP

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

	// An arrow key at a shell is ctrl+p, with no note.
	k.Input([]byte("a\x1b[Ab"))
	wait("\x1b\x1ba\x10b")
	if n := barNote(con); n != "" {
		t.Errorf("an arrow key at a shell gave a note: %q", n)
	}

	// In a full-screen program it is dropped, with the note.
	fl.alt.Store(true)
	k.Input([]byte("\x1b[A"))
	k.Input([]byte("c"))
	wait("\x1b\x1ba\x10bc")
	if n := barNote(con); n != VSPKeysNote {
		t.Errorf("bar note = %q, want the VSP note", n)
	}
	fl.alt.Store(false)

	// The note shows once per session: clear it, press another, and it stays clear.
	con.mu.Lock()
	con.st.note = ""
	con.mu.Unlock()
	k.Input([]byte("\x1bOP"))
	k.Input([]byte("\x1b(d")) // ESC ( is dropped, never typed
	wait("\x1b\x1ba\x10bcd")
	if n := barNote(con); n != "" {
		t.Errorf("the note showed twice: %q", n)
	}

	// A paste has its escape sequences stripped, and types the rest.
	k.Input([]byte("\x1b[200~echo \x1b[31mhi\x1b[201~"))
	wait("\x1b\x1ba\x10bcdecho hi")

	// A fast Esc, :wq and Enter in one read is a lone ESC and the keys.
	k.Input([]byte("\x1b:wq\r"))
	wait("\x1b\x1ba\x10bcdecho hi\x1b\x1b:wq\r")
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

func TestInputFilterDropsAFragmentedKeyWhole(t *testing.T) {
	const esc = "\x1b"
	for _, tc := range []struct {
		name  string
		reads []string
		want  string
	}{
		{"ss3 split", []string{esc + "O", "A"}, "\x10"},
		{"csi split after the params", []string{esc + "[1;5", "C"}, ""},
		{"csi split after the bracket", []string{esc + "[", "A"}, "\x10"},
		{"split in three", []string{esc + "[1", ";5", "C"}, ""},
		{"text around a split key", []string{"a" + esc + "O", "Ab"}, "a\x10b"},
		{"two split keys", []string{esc + "[1", "A" + esc + "[", "Bc"}, "\x0ec"},
		{"whole keys are as before", []string{esc + "[A", "x" + esc + "OP"}, "\x10x"},
		{"lone esc is typed at once", []string{esc}, esc + esc},
		{"esc then text", []string{esc, "x"}, esc + esc + "x"},
		{"ctrl+c inside a half key still goes", []string{esc + "[1", "\x03"}, "\x03"},
		{"ctrl+c right after ESC O", []string{esc + "O", "\x03"}, "\x03"},
		{"plain text", []string{"ls\r"}, "ls\r"},
		{"focus report", []string{esc + "[", "I"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f InputFilter
			l := &vspLink{}
			var out []byte
			for _, r := range tc.reads {
				o, _ := f.Map(l, []byte(r))
				out = append(out, o...)
			}
			if string(out) != tc.want {
				t.Errorf("typed %q, want %q", out, tc.want)
			}
			if f.Pending() {
				t.Errorf("still holding %q", f.held)
			}
		})
	}
}

func TestInputFilterHoldsBoundedAndFlushesOnTimeout(t *testing.T) {
	var f InputFilter
	l := &vspLink{}
	if out, _ := f.Map(l, []byte("\x1b[1;5")); len(out) != 0 || !f.Pending() {
		t.Fatalf("out %q pending %v, want the unfinished key held", out, f.Pending())
	}
	// Nothing more comes: it is dropped whole, with the note, and typing goes on.
	if out, note := f.Flush(l); len(out) != 0 || note != VSPKeysNote || f.Pending() {
		t.Errorf("flush: %q %q pending %v", out, note, f.Pending())
	}
	if out, _ := f.Map(l, []byte("C")); string(out) != "C" {
		t.Errorf("the byte after a flushed fragment = %q, want it typed", out)
	}
	// A "sequence" that never finishes is not held without limit.
	junk := "\x1b[" + strings.Repeat("1;", 40)
	if out, _ := f.Map(l, []byte(junk)); len(out) != 0 || f.Pending() {
		t.Errorf("an over-long unfinished sequence: out %q, pending %v", out, f.Pending())
	}
	// A link that needs no mapping is never buffered.
	var g InputFilter
	if out, _ := g.Map(&fakeLink{}, []byte("\x1b[1;5")); string(out) != "\x1b[1;5" || g.Pending() {
		t.Errorf("non-VSP: %q pending %v", out, g.Pending())
	}
}

func TestConsoleKeysDropAFragmentedArrowWhole(t *testing.T) {
	fl := &vspLink{}
	k, con := vspKeys(t, fl)
	sent := func() string { fl.mu.Lock(); defer fl.mu.Unlock(); return string(fl.sent) }
	// A terminal wrote an arrow key as two pieces and the reads fell between them.
	k.Input([]byte("a\x1bO"))
	k.Input([]byte("Ab"))
	k.Input([]byte("\x1b[1;5"))
	k.Input([]byte("Cd"))
	for i := 0; i < 200 && sent() != "a\x10bd"; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sent(); got != "a\x10bd" {
		t.Fatalf("typed %q, want a ctrl+p b d (the halves of an arrow key must not be typed)", got)
	}
	if n := barNote(con); n != VSPKeysNote {
		t.Errorf("bar note = %q", n)
	}
	// The rest never comes: the loop's wait fires Flush, and what is held goes.
	k.Input([]byte("\x1b[1;"))
	if !k.filter.Pending() {
		t.Fatal("an unfinished key is not held")
	}
	k.Flush()
	k.Input([]byte("e"))
	for i := 0; i < 200 && sent() != "a\x10bde"; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sent(); got != "a\x10bde" {
		t.Errorf("typed %q, want a ctrl+p bde", got)
	}
}

// togglingLink is a VSP mapper that can stop mapping, like the iLO link at the
// BMC's own prompt.
type togglingLink struct {
	fakeLink
	off bool
}

func (l *togglingLink) MapInput(b []byte) ([]byte, string) {
	if l.off {
		return b, ""
	}
	return MapVSPInput(b, false)
}

func TestInputFilterHoldsNothingWhileTheLinkIsNotMapping(t *testing.T) {
	l := &togglingLink{off: true}
	var f InputFilter
	if out, _ := f.Map(l, []byte("\x1b[1;5")); string(out) != "\x1b[1;5" || f.Pending() {
		t.Errorf("unmapped link: out %q pending %v, want the bytes through and nothing held", out, f.Pending())
	}
	// Held while mapping, then the link stops mapping: the held keys are
	// forwarded, not dropped.
	l.off = false
	if out, _ := f.Map(l, []byte("\x1b[1;5")); len(out) != 0 || !f.Pending() {
		t.Fatalf("mapped link: out %q pending %v", out, f.Pending())
	}
	l.off = true
	if out, note := f.Flush(l); string(out) != "\x1b[1;5" || note != "" || f.Pending() {
		t.Errorf("flush after the mapping ended: out %q note %q", out, note)
	}
	l.off = false
	f.Map(l, []byte("\x1b["))
	l.off = true
	if out, _ := f.Map(l, []byte("A")); string(out) != "\x1b[A" || f.Pending() {
		t.Errorf("next input after the mapping ended: %q", out)
	}
}

func TestConsoleKeysForwardAHeldKeyWhenTheLinkStopsMapping(t *testing.T) {
	fl := &togglingLink{}
	k, _ := vspKeys(t, fl)
	k.Input([]byte("\x1b[1;"))
	if !k.filter.Pending() {
		t.Fatal("an unfinished key is not held")
	}
	fl.off = true
	k.Flush()
	for i := 0; i < 200 && len(fl.sentCopy()) < 4; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if got := string(fl.sentCopy()); got != "\x1b[1;" {
		t.Errorf("sent %q, want the held bytes forwarded", got)
	}
}

// stripLink is a link whose input is mapped elsewhere (a shared window's): not
// an InputMapper, but its pastes lose their escape sequences all the same.
type stripLink struct{ fakeLink }

func (*stripLink) StripsPaste() bool { return true }

func TestPasteToAPasteStrippingLinkLosesWholeEscapeSequences(t *testing.T) {
	fl := &stripLink{}
	k, _ := vspKeys(t, fl)
	k.Input([]byte("\x1b[200~echo \x1b[31mhi\x1b[0m\x1b[201~"))
	for i := 0; i < 200 && len(fl.sentCopy()) < len("echo hi"); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if got := string(fl.sentCopy()); got != "echo hi" {
		t.Errorf("sent %q, want %q", got, "echo hi")
	}
}

func TestEnterAfterALongPasteGoesLast(t *testing.T) {
	paste := strings.Repeat("pAssw0rd-", 34)[:300]
	for _, tc := range []struct {
		name  string
		reads []string
	}{
		{"bracketed", []string{"\x1b[200~" + paste[:100], paste[100:] + "\x1b[201~", "\r"}},
		{"bracketed, one read", []string{"\x1b[200~" + paste + "\x1b[201~", "\r"}},
		{"markers off, plain keystrokes", []string{paste[:100], paste[100:200], paste[200:], "\r"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fl := &vspLink{}
			ms := time.Millisecond
			con := newConsole(&bytes.Buffer{}, "h", "vsp", 24, 80)
			con.Start()
			defer con.Close()
			k := newKeys(&Setup{Link: fl, Pacing: Pacing{Char: &ms, Line: &ms}}, con, func() {}, make(chan struct{}))
			k.start(nil)
			for _, r := range tc.reads {
				k.Input([]byte(r))
			}
			if n := barNote(con); n != "Enter will be sent after the paste" {
				t.Errorf("bar note = %q, want the Enter note", n)
			}
			for i := 0; i < 600; i++ {
				fl.mu.Lock()
				n := len(fl.sent)
				fl.mu.Unlock()
				if n >= len(paste)+1 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if got := string(fl.sentCopy()); got != paste+"\r" {
				t.Errorf("sent %q, want the paste then CR", got)
			}
		})
	}
}
