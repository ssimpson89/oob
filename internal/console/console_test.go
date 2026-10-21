package console

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// ansiRE strips the escape sequences the bar and fakes emit.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[()][A-Za-z0-9]|\x1b[=>78]`)

func TestKeyParser(t *testing.T) {
	esc := string(rune(escapeByte))
	tests := []struct {
		name string
		in   []string // chunks
		fwd  string
		acts []action
		mode keyMode
	}{
		{"plain", []string{"ls\r"}, "ls\r", nil, modeConsole},
		{"size", []string{esc + "s"}, "", []action{{act: actSize}}, modeConsole},
		{"split chunks", []string{esc, "l"}, "", []action{{act: actLog}}, modeConsole},
		{"text around", []string{"a" + esc + "bz"}, "az", []action{{act: actBreak}}, modeConsole},
		{"auto", []string{esc + "a"}, "", []action{{act: actAuto}}, modeConsole},
		{"literal", []string{esc + "]"}, "", []action{{act: actLiteral}}, modeConsole},
		{"double escape", []string{esc + esc}, "", []action{{act: actLiteral}}, modeConsole},
		{"disconnect", []string{esc + "."}, "", []action{{act: actDisconnect}}, modeConsole},
		{"help", []string{esc + "?"}, "", []action{{act: actHelp}}, modeConsole},
		{"esc cancels", []string{esc + "\x1b"}, "", []action{{act: actCancel}}, modeConsole},
		{"unknown key cancels", []string{esc + "z"}, "", []action{{act: actCancel}}, modeConsole},
		{"pending command", []string{esc}, "", nil, modeCommand},
		{"arrow key swallowed", []string{esc + "\x1b[A" + "s"}, "", []action{{act: actSize}}, modeConsole},
		{"sysrq waits", []string{esc + "r"}, "", nil, modeSysrq},
		{"sysrq letter", []string{esc + "rb"}, "", []action{{actSysrq, 'b'}}, modeConsole},
		{"sysrq bad key", []string{esc + "r!"}, "", []action{{act: actCancel}}, modeConsole},
		{"sysrq esc", []string{esc + "r\x1b"}, "", []action{{act: actCancel}}, modeConsole},
		{"power asks", []string{esc + "p"}, "", nil, modeConfirm},
		{"power yes", []string{esc + "py"}, "", []action{{act: actPower}}, modeConsole},
		{"power no", []string{esc + "pn"}, "", []action{{act: actCancel}}, modeConsole},
		{"power other key", []string{esc + "px"}, "", []action{{act: actCancel}}, modeConsole},
		{"~. at line start after CR", []string{"\r~."}, "\r", []action{{act: actDisconnect}}, modeConsole},
		{"~. at session start", []string{"~."}, "", []action{{act: actDisconnect}}, modeConsole},
		{"~. split across reads", []string{"ls\n~", "."}, "ls\n", []action{{act: actDisconnect}}, modeConsole},
		{"~~ sends one", []string{"\n~~"}, "\n~", nil, modeConsole},
		{"~x sends both", []string{"~x"}, "~x", nil, modeConsole},
		{"~ then CR sends both", []string{"~\r~."}, "~\r", []action{{act: actDisconnect}}, modeConsole},
		{"mid-line ~. passes through", []string{"a~."}, "a~.", nil, modeConsole},
		{"~ is held until next byte", []string{"\r~"}, "\r", nil, modeConsole},
		{"~ then menu key sends ~", []string{"~" + esc + "s"}, "~", []action{{act: actSize}}, modeConsole},
		{"after ~~ line is mid-line", []string{"~~.x"}, "~.x", nil, modeConsole},
		{"ESC then CSI in separate reads is swallowed", []string{esc + "\x1b", "[A", "s"}, "", []action{{act: actSize}}, modeConsole},
		{"CSI split mid-sequence is swallowed", []string{esc + "\x1b[1", ";5A", "s"}, "", []action{{act: actSize}}, modeConsole},
		{"lone ESC in command mode cancels on flush", []string{esc + "\x1b"}, "", []action{{act: actCancel}}, modeConsole},
		{"lone ESC in console mode is forwarded on flush", []string{"\x1b"}, "\x1b", nil, modeConsole},
		{"arrow key forwarded", []string{"\x1b[A"}, "\x1b[A", nil, modeConsole},
		{"paste markers stripped", []string{"\x1b[200~hi\x1b[201~"}, "hi", nil, modeConsole},
		{"paste with ~. at line start", []string{"\r\x1b[200~~.\x1b[201~"}, "\r~.", nil, modeConsole},
		{"paste at session start with ~.", []string{"\x1b[200~~.rm\x1b[201~"}, "~.rm", nil, modeConsole},
		{"ctrl+] inside paste is literal", []string{"\x1b[200~a" + esc + "sb\x1b[201~"}, "a" + esc + "sb", nil, modeConsole},
		{"markers split every way", []string{"\x1b", "[2", "00~x", "~.\x1b[20", "1", "~", "\r~."}, "x~.\r", []action{{act: actDisconnect}}, modeConsole},
		{"keys work after paste", []string{"\x1b[200~x\x1b[201~" + esc + "s"}, "x", []action{{act: actSize}}, modeConsole},
		{"stray end marker stripped", []string{"a\x1b[201~b"}, "ab", nil, modeConsole},
		{"paste during command mode cancels it", []string{esc + "\x1b[200~s\x1b[201~"}, "s", []action{{act: actCancel}}, modeConsole},
		{"forwarding resumes", []string{esc + "s" + "ok"}, "ok", []action{{act: actSize}}, modeConsole},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p keyParser
			var fwd []byte
			var acts []action
			for _, c := range tc.in {
				f, a := p.Feed([]byte(c))
				fwd = append(fwd, f...)
				acts = append(acts, a...)
			}
			f, a := p.Flush() // a held ESC resolves when no more input comes
			fwd = append(fwd, f...)
			acts = append(acts, a...)
			if string(fwd) != tc.fwd {
				t.Errorf("fwd = %q, want %q", fwd, tc.fwd)
			}
			if !reflect.DeepEqual(acts, tc.acts) {
				t.Errorf("acts = %v, want %v", acts, tc.acts)
			}
			if p.Mode() != tc.mode {
				t.Errorf("mode = %v, want %v", p.Mode(), tc.mode)
			}
		})
	}
}

// plain strips the SGR codes from a rendered bar.
func plain(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

func TestBarFitsEveryWidth(t *testing.T) {
	states := []barState{
		{host: "node01", kind: "sol"},
		{host: "node01", kind: "sol", logging: true, note: "BREAK sent"},
		{host: "node01", kind: "sol", mode: modeCommand},
		{host: "node01", kind: "sol", mode: modeCommand, logging: true, auto: true},
		{host: "node01", kind: "sol", mode: modeSysrq},
		{host: "node01", kind: "sol", mode: modeConfirm},
	}
	for _, st := range states {
		for w := 1; w <= 200; w++ {
			got := plain(barLine(st, w))
			if n := utf8.RuneCountInString(got); n != w {
				t.Fatalf("mode %v width %d: bar is %d cells: %q", st.mode, w, n, got)
			}
		}
	}
}

func TestCommandBarDropsHints(t *testing.T) {
	st := barState{host: "h", kind: "sol", mode: modeCommand}
	wide := plain(barLine(st, 200))
	for _, want := range []string{"size", "auto-size", "log", "break", "r+key", "power cycle", "send ^]", "disconnect", "help", "esc back"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide bar missing %q: %q", want, wide)
		}
	}
	if strings.Contains(wide, "more") {
		t.Errorf("wide bar should not say more: %q", wide)
	}
	narrow := plain(barLine(st, 50))
	if !strings.Contains(narrow, "more") || !strings.Contains(narrow, "disconnect") || !strings.Contains(narrow, "esc") {
		t.Errorf("narrow bar lost always-keep keys: %q", narrow)
	}
	if strings.Contains(narrow, "send ^]") {
		t.Errorf("lowest-keep hint should go first: %q", narrow)
	}
	if !strings.Contains(plain(barLine(barState{host: "h", mode: modeCommand, logging: true}, 200)), "stop log") {
		t.Error("logging bar should offer stop log")
	}
}

func TestShortKey(t *testing.T) {
	if shortKey("ctrl+]") != "^]" || shortKey("esc") != "esc" {
		t.Error("shortKey")
	}
}

func TestEscTracker(t *testing.T) {
	var tr escTracker
	for _, tc := range []struct {
		chunk string
		idle  bool
	}{
		{"hello", true},
		{"\x1b[3", false},
		{"1m", true},
		{"\x1b]0;title", false},
		{"\x07", true},
		{"\x1b]0;t\x1b", false},
		{"\\", true},
		{"\xe2\x82", false},
		{"\xac", true},
		{"\x1b(", false},
		{"B", true},
	} {
		tr.Feed([]byte(tc.chunk))
		if tr.Idle() != tc.idle {
			t.Fatalf("after %q: idle = %v, want %v", tc.chunk, tr.Idle(), tc.idle)
		}
	}
}

func TestClobbers(t *testing.T) {
	for in, want := range map[string]bool{
		"plain text\r\n":        false,
		"\x1b[31mred\x1b[0m":    false,
		"\x1b[1;24r":            true,
		"\x1b[r":                true,
		"\x1b[?1049h":           true,
		"\x1b[?1049l":           true,
		"\x1b[?25l\x1b[?1047h":  true,
		"\x1b[?25l":             false,
		"\x1b[?7r":              false,
		"\x1bc":                 true,
		"\x1b[2J":               true,
		"\x1b[3J":               true,
		"\x1b[J":                false,
		"\x1b[!p":               true,
		"abc\x1b[H\x1b[2;3Hxyz": false,
	} {
		if got := clobbers([]byte(in)); got != want {
			t.Errorf("clobbers(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestHistoryRing(t *testing.T) {
	h := newHistory(3)
	h.Feed([]byte("a\nb\r\nc"))
	if got := h.Lines(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("lines = %q", got)
	}
	if h.Partial() != "c" {
		t.Errorf("partial = %q", h.Partial())
	}
	h.Feed([]byte("\nd\ne\nf\n"))
	if got := h.Lines(); !reflect.DeepEqual(got, []string{"d", "e", "f"}) {
		t.Fatalf("after wrap lines = %q", got)
	}
	if h.Len() != 3 {
		t.Errorf("len = %d", h.Len())
	}
}

func TestHistoryCapIsTenThousand(t *testing.T) {
	h := newHistory(historyLinesMax)
	for i := 0; i < historyLinesMax+5; i++ {
		h.Feed([]byte("x\n"))
	}
	if h.Len() != 10000 {
		t.Errorf("len = %d, want 10000", h.Len())
	}
}

func TestHistoryStripsANSI(t *testing.T) {
	h := newHistory(10)
	h.Feed([]byte("\x1b[1;31mred\x1b[0m text\r\nabcd\b\bXY\r\n"))
	if got := h.Lines(); !reflect.DeepEqual(got, []string{"red text", "abXY"}) {
		t.Errorf("lines = %q", got)
	}
}

func TestLogWriterIncludesEarlier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "node01.log")
	l, err := openLog(path, "hdr", []string{"one", "two"}, "login: ")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write([]byte("root\r\n\x1b[32mok\x1b[0m\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := l.Mark("BREAK sent"); err != nil {
		t.Fatal(err)
	}
	if err := l.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"# hdr\n", "one\ntwo\n", "logging turned on", "login: root\nok\n", "BREAK sent ---\ntail\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("log has escape sequences:\n%q", got)
	}
	if strings.Index(got, "one\n") > strings.Index(got, "logging turned on") {
		t.Errorf("earlier output should come before the marker")
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestLogPathXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	ts := time.Date(2026, 10, 21, 9, 8, 7, 0, time.UTC)
	got, err := logPath("node 01/x", ts)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/xdg/state/oob/logs/node-01-x-20261021-090807.log"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	got, _ = logPath("n", ts)
	if want := "/home/u/.local/state/oob/logs/n-20261021-090807.log"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestConsoleRegionAndRestore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	c := newConsole(&out, "node01", "sol", 24, 80)
	c.Start()
	if s := out.String(); !strings.Contains(s, "\x1b[1;23r") || !strings.Contains(s, "\x1b[24;1H") {
		t.Fatalf("start: no region or bar paint: %q", s)
	}
	out.Reset()

	c.Output([]byte("plain output\r\n"))
	if strings.Contains(out.String(), "\x1b[1;23r") {
		t.Errorf("repainted without need: %q", out.String())
	}
	out.Reset()

	c.Output([]byte("\x1b[r")) // remote resets the region
	if !strings.Contains(out.String(), "\x1b[1;23r") {
		t.Errorf("region not re-applied after reset: %q", out.String())
	}
	out.Reset()

	c.Output([]byte("\x1b[3")) // cut mid-sequence: must not paint
	c.Output([]byte("1m\x1b[2J"))
	s := out.String()
	if i := strings.Index(s, "\x1b[24;1H"); i < strings.Index(s, "1m\x1b[2J") {
		t.Errorf("bar painted inside an escape sequence: %q", s)
	}
	out.Reset()

	c.ToggleLog()
	if !c.st.logging || len(c.logs) != 1 {
		t.Fatalf("log not started: %+v", c.st)
	}
	c.Output([]byte("after\r\n"))
	logs := c.Close()
	tail := out.String()
	if !strings.Contains(tail, "\x1b[r") || !strings.Contains(tail, "\x1b[24;1H\x1b[2K") {
		t.Errorf("close did not restore region and clear bar: %q", tail)
	}
	out.Reset()
	c.Close()
	c.Output([]byte("x"))
	if out.Len() != 0 {
		t.Errorf("wrote after close: %q", out.String())
	}
	b, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "plain output") || !strings.Contains(string(b), "after") {
		t.Errorf("log lacks earlier or later output:\n%s", b)
	}
}

func TestStickyNoteClearsOnFirstOutput(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf, "h", "sol", 24, 80)
	c.Start()
	defer c.Close()
	c.Sticky("connecting to h via IPMI...")
	if !strings.Contains(buf.String(), "connecting to h via IPMI...") {
		t.Fatal("note not painted on the bar")
	}
	time.Sleep(50 * time.Millisecond)
	c.mu.Lock()
	still := c.st.note
	c.mu.Unlock()
	if still == "" {
		t.Fatal("sticky note vanished before any output")
	}
	c.Output([]byte("[SOL Session operational]\r\n"))
	c.mu.Lock()
	cleared := c.st.note == "" && !c.sticky
	c.mu.Unlock()
	if !cleared {
		t.Error("note should clear on first output")
	}
}

func TestParserBMCKey(t *testing.T) {
	var p keyParser
	fwd, acts := p.Feed([]byte{escapeByte, 'c'})
	if len(fwd) != 0 || len(acts) != 1 || acts[0].act != actBMC || p.Mode() != modeConsole {
		t.Fatalf("got fwd=%q acts=%v mode=%v", fwd, acts, p.Mode())
	}
}

func TestBarBMCLabels(t *testing.T) {
	plain := func(s string) string { return ansiRE.ReplaceAllString(s, "") }
	st := barState{host: "h", kind: "vsp", mode: modeCommand}
	if got := plain(barLine(st, 200)); !strings.Contains(got, "c bmc prompt") {
		t.Errorf("console state: %q", got)
	}
	st.atPrompt, st.kind = true, "ilo cli"
	if got := plain(barLine(st, 200)); !strings.Contains(got, "c console") || strings.Contains(got, "bmc prompt") {
		t.Errorf("prompt state: %q", got)
	}
	st.mode = modeConsole
	if got := plain(barLine(st, 80)); !strings.Contains(got, "ilo cli") {
		t.Errorf("kind label: %q", got)
	}
	if !strings.Contains(helpText(), "BMC's own CLI prompt") {
		t.Error("help lacks c")
	}
}

func TestEscTrackerStuckSequences(t *testing.T) {
	var tr escTracker
	tr.Feed([]byte("\x1bPnoise with no terminator"))
	if tr.Idle() {
		t.Fatal("DCS should be open")
	}
	tr.Feed([]byte("\x18"))
	if !tr.Idle() {
		t.Error("CAN should abort the string")
	}
	tr.Feed([]byte("\x1b[12;"))
	tr.Feed([]byte("\x1a"))
	if !tr.Idle() {
		t.Error("SUB should abort the CSI")
	}
	tr.Feed([]byte("\x1b]0;" + strings.Repeat("x", seqMax+10)))
	if !tr.Idle() {
		t.Error("an overlong string should count as ended")
	}
	tr.Feed([]byte("\x1b["))
	tr.Feed([]byte(strings.Repeat("1", seqMax+10)))
	if !tr.Idle() {
		t.Error("an overlong CSI should count as ended")
	}
	tr.Feed([]byte("\x1bP"))
	tr.Reset()
	if !tr.Idle() {
		t.Error("Reset should go idle")
	}
}

func TestBarRepaintsAfterStuckSequence(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out, "h", "sol", 24, 80)
	c.Start()
	defer c.Close()
	out.Reset()
	c.Output([]byte("\x1bPgarbage"))
	out.Reset()
	c.SetSize(30, 100)
	if out.Len() != 0 {
		t.Fatalf("painted inside a sequence: %q", out.String())
	}
	now := time.Now()
	c.tickOnce(now) // not quiet long enough yet
	if out.Len() != 0 {
		t.Fatalf("gave up on the sequence too soon: %q", out.String())
	}
	c.tickOnce(now.Add(stuckSeqQuiet + time.Second))
	s := out.String()
	if !strings.Contains(s, "\x1b[1;29r") || !strings.Contains(s, "\x1b[30;1H") {
		t.Errorf("region and bar not written after idle: %q", s)
	}
}

func TestSmallWindowDropsBar(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out, "h", "sol", 24, 80)
	c.Start()
	defer c.Close()
	out.Reset()
	c.SetSize(1, 80)
	s := out.String()
	if strings.Contains(s, "\x1b[1;0r") {
		t.Fatalf("bad region for 1 row: %q", s)
	}
	if !strings.Contains(s, "\x1b[r") || strings.Contains(s, "\x1b[?7l") {
		t.Errorf("region not cleared or bar painted: %q", s)
	}
	out.Reset()
	c.Note("hello", false)
	c.SetMode(modeCommand)
	c.Output([]byte("x"))
	if strings.Contains(out.String(), "\x1b[?7l") || strings.Contains(out.String(), "\x1b[1;0r") {
		t.Errorf("painted a bar in a 1-row window: %q", out.String())
	}
	out.Reset()
	c.SetSize(24, 80)
	s = out.String()
	if !strings.Contains(s, "\x1b[1;23r") || !strings.Contains(s, "\x1b[24;1H") {
		t.Errorf("region and bar not back after growing: %q", s)
	}
	if strings.Contains(s, "\x1b[1;1H\x1b[2K") {
		t.Errorf("blanked a row that never held the bar: %q", s)
	}
}

func TestBracketedPasteAtStartAndClose(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out, "h", "sol", 24, 80)
	c.Start()
	if !strings.Contains(out.String(), "\x1b[?2004h") {
		t.Errorf("start did not enable bracketed paste: %q", out.String())
	}
	out.Reset()
	c.Close()
	if !strings.Contains(out.String(), "\x1b[?2004l") {
		t.Errorf("close did not disable bracketed paste: %q", out.String())
	}
}

func TestNoteStripsControls(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out, "h", "sol", 24, 80)
	c.Start()
	defer c.Close()
	c.Note("bad\x1b[2Jnews\x07\u009b", true)
	if strings.Contains(out.String(), "\x1b[2Jnews") || strings.Contains(out.String(), "\x07") {
		t.Errorf("control characters reached the terminal: %q", out.String())
	}
}

func TestLogRestartSameSecond(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	c := newConsole(&out, "h", "sol", 24, 80)
	c.Start()
	defer c.Close()
	for i := 0; i < 3; i++ {
		c.ToggleLog()
		c.ToggleLog()
	}
	// Different seconds are possible; the paths must be distinct either way.
	seen := map[string]bool{}
	for _, p := range c.logs {
		if seen[p] {
			t.Errorf("path recorded twice: %s", p)
		}
		seen[p] = true
	}
	if len(c.logs) != 3 {
		t.Errorf("logs = %v, want 3 distinct files", c.logs)
	}
	if got := uniqueLogPath(c.logs[0]); got == c.logs[0] {
		t.Errorf("uniqueLogPath returned an existing file: %s", got)
	}
	if strings.Contains(barLine(barState{host: "h", kind: "sol", logging: true}, 80), "\u25cf") {
		t.Error("log marker should be ASCII")
	}
}

// fakeLink records what the console does to the transport.
type fakeLink struct {
	mu                  sync.Mutex
	sent                []byte
	breaks, disconnects int
	fits                [][2]int
}

func (f *fakeLink) Send(b []byte) error {
	f.mu.Lock()
	f.sent = append(f.sent, b...)
	f.mu.Unlock()
	return nil
}
func (f *fakeLink) Break() error          { f.mu.Lock(); f.breaks++; f.mu.Unlock(); return nil }
func (f *fakeLink) Disconnect() error     { f.mu.Lock(); f.disconnects++; f.mu.Unlock(); return nil }
func (f *fakeLink) Resize(r, c int) error { return nil }
func (f *fakeLink) FitSize(r, c int) error {
	f.mu.Lock()
	f.fits = append(f.fits, [2]int{r, c})
	f.mu.Unlock()
	return nil
}
func (f *fakeLink) SizeIsFree() bool { return false }

func TestConsoleKeyActions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	con := newConsole(&out, "h", "sol", 24, 80)
	con.Start()
	defer con.Close()
	fl := &fakeLink{}
	powered := make(chan struct{})
	cs := &Setup{Link: fl, Power: func() (string, error) { close(powered); return "ok\n", nil }}
	stop := make(chan struct{})
	k := &consoleKeys{cs: cs, con: con, cleanup: func() {}, stop: stop}
	esc := string(rune(escapeByte))

	k.Input([]byte(esc + "s"))
	if len(fl.fits) != 1 || fl.fits[0] != [2]int{23, 80} {
		t.Errorf("size: fits = %v", fl.fits)
	}
	k.Input([]byte(esc + "a"))
	if !k.autoOn() {
		t.Error("auto-size should be on")
	}
	k.Input([]byte(esc + "l"))
	if len(con.logs) != 1 {
		t.Error("log not started")
	}
	k.Input([]byte(esc + "l"))
	con.mu.Lock()
	logging := con.st.logging
	con.mu.Unlock()
	if logging {
		t.Error("log not stopped")
	}
	k.Input([]byte(esc + "b"))
	if fl.breaks != 1 {
		t.Errorf("breaks = %d", fl.breaks)
	}

	k.Input([]byte(esc + "p"))
	select {
	case <-powered:
		t.Fatal("power cycled before confirmation")
	case <-time.After(50 * time.Millisecond):
	}
	k.Input([]byte("n"))
	k.Input([]byte(esc + "py"))
	select {
	case <-powered:
	case <-time.After(2 * time.Second):
		t.Fatal("power cycle not run after y")
	}

	k.Input([]byte(esc + "."))
	if fl.disconnects != 1 {
		t.Errorf("disconnects = %d", fl.disconnects)
	}
	k.Input([]byte("~"))
	k.Input([]byte("."))
	if fl.disconnects != 2 {
		t.Errorf("~. disconnects = %d", fl.disconnects)
	}

	// ESC then [A in command mode must not reach the remote.
	fl.sent = nil
	k.Input([]byte(esc + "\x1b"))
	if !k.parser.Pending() {
		t.Error("lone ESC should be held")
	}
	k.Input([]byte("[A"))
	k.Input([]byte("x")) // still in command mode: x is an unknown key, a cancel
	if len(fl.sent) != 0 || k.parser.Mode() != modeConsole {
		t.Errorf("sent %q mode %v, want nothing sent and back in console mode", fl.sent, k.parser.Mode())
	}

	close(stop)
	fl.sent = nil
	if k.Input([]byte("y")) || len(fl.sent) != 0 {
		t.Error("input after stop must not be sent")
	}
}
