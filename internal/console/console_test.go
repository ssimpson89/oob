package console

import (
	"bytes"
	"errors"
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
		{"text around", []string{"a" + esc + "bz"}, "az", []action{{act: actBreak, at: 1}}, modeConsole},
		{"auto", []string{esc + "a"}, "", []action{{act: actAuto}}, modeConsole},
		{"literal", []string{esc + "]"}, "", []action{{act: actLiteral}}, modeConsole},
		{"double escape", []string{esc + esc}, "", []action{{act: actLiteral}}, modeConsole},
		{"disconnect", []string{esc + "."}, "", []action{{act: actDisconnect}}, modeConsole},
		{"help", []string{esc + "?"}, "", []action{{act: actHelp}}, modeConsole},
		{"esc clears the queue", []string{esc + "\x1b"}, "", []action{{act: actStop}}, modeConsole},
		{"x clears the queue", []string{esc + "x"}, "", []action{{act: actStop}}, modeConsole},
		{"actions record where they fall among forwarded bytes", []string{"a" + esc + "xbc" + esc + "sd"}, "abcd", []action{{act: actStop, at: 1}, {act: actSize, at: 3}}, modeConsole},
		{"w is no longer a key", []string{esc + "w"}, "", []action{{act: actCancel}}, modeConsole},
		{"unknown key cancels", []string{esc + "z"}, "", []action{{act: actCancel}}, modeConsole},
		{"pending command", []string{esc}, "", nil, modeCommand},
		{"arrow key swallowed", []string{esc + "\x1b[A" + "s"}, "", []action{{act: actSize}}, modeConsole},
		{"sysrq waits", []string{esc + "r"}, "", nil, modeSysrq},
		{"sysrq letter", []string{esc + "rb"}, "", []action{{act: actSysrq, arg: 'b'}}, modeConsole},
		{"sysrq bad key", []string{esc + "r!"}, "", []action{{act: actCancel}}, modeConsole},
		{"sysrq esc", []string{esc + "r\x1b"}, "", []action{{act: actCancel}}, modeConsole},
		{"power asks", []string{esc + "p"}, "", nil, modeConfirm},
		{"power yes", []string{esc + "py"}, "", []action{{act: actPower}}, modeConsole},
		{"power no", []string{esc + "pn"}, "", []action{{act: actCancel}}, modeConsole},
		{"power other key", []string{esc + "px"}, "", []action{{act: actCancel}}, modeConsole},
		{"~. at line start after CR", []string{"\r~."}, "\r", []action{{act: actDisconnect, at: 1}}, modeConsole},
		{"~. at session start", []string{"~."}, "", []action{{act: actDisconnect}}, modeConsole},
		{"~. split across reads", []string{"ls\n~", "."}, "ls\n", []action{{act: actDisconnect}}, modeConsole},
		{"~~ sends one", []string{"\n~~"}, "\n~", nil, modeConsole},
		{"~x sends both", []string{"~x"}, "~x", nil, modeConsole},
		{"~ then CR sends both", []string{"~\r~."}, "~\r", []action{{act: actDisconnect, at: 2}}, modeConsole},
		{"mid-line ~. passes through", []string{"a~."}, "a~.", nil, modeConsole},
		{"~ is held until next byte", []string{"\r~"}, "\r", nil, modeConsole},
		{"~ then menu key sends ~", []string{"~" + esc + "s"}, "~", []action{{act: actSize, at: 1}}, modeConsole},
		{"after ~~ line is mid-line", []string{"~~.x"}, "~.x", nil, modeConsole},
		{"ESC then CSI in separate reads is swallowed", []string{esc + "\x1b", "[A", "s"}, "", []action{{act: actSize}}, modeConsole},
		{"CSI split mid-sequence is swallowed", []string{esc + "\x1b[1", ";5A", "s"}, "", []action{{act: actSize}}, modeConsole},
		{"lone ESC in command mode stops on flush", []string{esc + "\x1b"}, "", []action{{act: actStop}}, modeConsole},
		{"lone ESC in console mode is forwarded on flush", []string{"\x1b"}, "\x1b", nil, modeConsole},
		{"arrow key forwarded", []string{"\x1b[A"}, "\x1b[A", nil, modeConsole},
		{"paste becomes one action", []string{"\x1b[200~hi\x1b[201~"}, "", []action{{act: actPaste, text: "hi"}}, modeConsole},
		{"paste split across reads", []string{"\x1b[200~a\r", "b\nc", "\x1b[201~"}, "", []action{{act: actPaste, text: "a\rb\nc"}}, modeConsole},
		{"empty paste is nothing", []string{"\x1b[200~\x1b[201~"}, "", nil, modeConsole},
		{"typing before and after a paste", []string{"a\x1b[200~p\x1b[201~b"}, "ab", []action{{act: actPaste, text: "p", at: 1}}, modeConsole},
		{"paste with ~. at line start", []string{"\r\x1b[200~~.\x1b[201~"}, "\r", []action{{act: actPaste, text: "~.", at: 1}}, modeConsole},
		{"paste at session start with ~.", []string{"\x1b[200~~.rm\x1b[201~"}, "", []action{{act: actPaste, text: "~.rm"}}, modeConsole},
		{"ctrl+] inside paste is literal", []string{"\x1b[200~a" + esc + "sb\x1b[201~"}, "", []action{{act: actPaste, text: "a" + esc + "sb"}}, modeConsole},
		{"markers split every way", []string{"\x1b", "[2", "00~x", "~.\x1b[20", "1", "~", "\r~."}, "\r", []action{{act: actPaste, text: "x~."}, {act: actDisconnect, at: 1}}, modeConsole},
		{"keys work after paste", []string{"\x1b[200~x\x1b[201~" + esc + "s"}, "", []action{{act: actPaste, text: "x"}, {act: actSize}}, modeConsole},
		{"stray end marker stripped", []string{"a\x1b[201~b"}, "ab", nil, modeConsole},
		{"paste during command mode cancels it", []string{esc + "\x1b[200~s\x1b[201~"}, "", []action{{act: actCancel}, {act: actPaste, text: "s"}}, modeConsole},
		{"forwarding resumes", []string{esc + "s" + "ok"}, "ok", []action{{act: actSize}}, modeConsole},
		{"focus in forwarded intact", []string{"\x1b[I"}, "\x1b[I", nil, modeConsole},
		{"focus out forwarded intact", []string{"\x1b[O"}, "\x1b[O", nil, modeConsole},
		{"focus report doesn't end line start", []string{"\r\x1b[O~."}, "\r\x1b[O", []action{{act: actDisconnect, at: 4}}, modeConsole},
		{"focus report at session start keeps line start", []string{"\x1b[I~."}, "\x1b[I", []action{{act: actDisconnect, at: 3}}, modeConsole},
		{"focus report split across reads", []string{"\r\x1b", "[O~."}, "\r\x1b[O", []action{{act: actDisconnect, at: 3}}, modeConsole},
		{"focus report between text", []string{"ab\x1b[Ocd"}, "ab\x1b[Ocd", nil, modeConsole},
		{"focus report mid-line leaves ~ literal", []string{"a\x1b[O~."}, "a\x1b[O~.", nil, modeConsole},
		{"focus report after a held ~ keeps order", []string{"\r~\x1b[O."}, "\r~\x1b[O.", nil, modeConsole},
		{"focus report in command mode swallowed", []string{esc + "\x1b[Os"}, "", []action{{act: actSize}}, modeConsole},
		{"SS3 key forwarded intact", []string{"\x1bOP", "\x1bOA"}, "\x1bOP\x1bOA", nil, modeConsole},
		{"SS3 split across reads forwarded in order", []string{"\x1b", "OP"}, "\x1bOP", nil, modeConsole},
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
	if shortKey != "^]" {
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
		"\x1b[J":                true,
		"\x1b[0J":               true,
		"\x1b[1J":               false,
		"\x1b[H\x1b[J":          true,
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
	k := newKeys(cs, con, func() {}, stop)
	k.start(nil)
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

type hangLink struct {
	fakeLink
	hangups int
}

func (h *hangLink) Hangup() error { h.mu.Lock(); h.hangups++; h.mu.Unlock(); return nil }

// Disconnect over a dirty line must not type: IPMI SOL's escape starts with a
// CR, which would run a truncated command ("reboot" out of "reboot --help").
func TestDisconnectWithPendingInputHangsUp(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	con := newConsole(&out, "h", "sol", 24, 80)
	con.Start()
	defer con.Close()
	hl := &hangLink{}
	k := newKeys(&Setup{Link: hl}, con, func() {}, make(chan struct{}))
	k.start(nil)
	esc := string(rune(escapeByte))

	k.Input([]byte("reboot"))
	deadline := time.Now().Add(2 * time.Second)
	for !k.lineDirty.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	k.Input([]byte(esc + "."))
	if hl.hangups != 1 || hl.disconnects != 0 {
		t.Errorf("dirty line: hangups=%d disconnects=%d, want 1 and 0", hl.hangups, hl.disconnects)
	}

	// A clean line keeps the normal disconnect.
	hl2 := &hangLink{}
	k2 := newKeys(&Setup{Link: hl2}, con, func() {}, make(chan struct{}))
	k2.start(nil)
	k2.Input([]byte(esc + "."))
	if hl2.hangups != 0 || hl2.disconnects != 1 {
		t.Errorf("clean line: hangups=%d disconnects=%d, want 0 and 1", hl2.hangups, hl2.disconnects)
	}
}

// blockLink's Send blocks until the link is closed (Hangup), like a BMC that
// stopped accepting input.
type blockLink struct {
	hangLink
	closed  chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (b *blockLink) Send(p []byte) error {
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return errors.New("link closed")
}

func (b *blockLink) Hangup() error {
	_ = b.hangLink.Hangup()
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

// Disconnect must not wait for a writer stuck in Send: it stops the queue and
// closes the transport itself.
func TestDisconnectDoesNotWaitOnBlockedSend(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	con := newConsole(&out, "h", "sol", 24, 80)
	con.Start()
	defer con.Close()
	bl := &blockLink{closed: make(chan struct{}), entered: make(chan struct{})}
	k := newKeys(&Setup{Link: bl}, con, func() {}, make(chan struct{}))
	k.start(nil)
	esc := string(rune(escapeByte))

	k.Input([]byte("reboot\r"))
	select {
	case <-bl.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Send never reached the link")
	}
	done := make(chan struct{})
	go func() { k.Input([]byte(esc + ".")); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect waited on the blocked writer")
	}
	select {
	case <-bl.closed:
	default:
		t.Error("transport was not closed")
	}
	bl.mu.Lock()
	defer bl.mu.Unlock()
	if bl.hangups != 1 {
		t.Errorf("hangups = %d, want 1", bl.hangups)
	}
}

// A clean-line Disconnect that itself blocks is bounded: after linkWait
// the link is hung up.
func TestDisconnectHangsUpWhenLinkDisconnectBlocks(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := linkWait
	linkWait = 50 * time.Millisecond
	defer func() { linkWait = old }()
	var out bytes.Buffer
	con := newConsole(&out, "h", "sol", 24, 80)
	con.Start()
	defer con.Close()
	dl := &stuckDisconnect{closed: make(chan struct{})}
	k := newKeys(&Setup{Link: dl}, con, func() {}, make(chan struct{}))
	k.start(nil)
	k.Input([]byte(string(rune(escapeByte)) + "."))
	select {
	case <-dl.closed:
	default:
		t.Error("transport was not closed")
	}
}

type stuckDisconnect struct {
	hangLink
	closed chan struct{}
}

func (s *stuckDisconnect) Disconnect() error { <-s.closed; return nil }
func (s *stuckDisconnect) Hangup() error     { close(s.closed); return nil }

// A switch to the BMC prompt that never returns doesn't hold the input loop: it
// is bounded, and the link is hung up.
func TestToggleHangsUpWhenTheLinkDoesNotAnswer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := linkWait
	linkWait = 50 * time.Millisecond
	defer func() { linkWait = old }()
	var out bytes.Buffer
	con := newConsole(&out, "h", "ilo", 24, 80)
	con.Start()
	defer con.Close()
	tl := &stuckToggle{closed: make(chan struct{})}
	k := newKeys(&Setup{Link: tl}, con, func() {}, make(chan struct{}))
	k.start(nil)
	done := make(chan struct{})
	go func() { k.Input([]byte(string(rune(escapeByte)) + "c")); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the input loop waited on the stuck toggle")
	}
	select {
	case <-tl.closed:
	default:
		t.Error("the link was not hung up")
	}
}

type stuckToggle struct {
	hangLink
	closed chan struct{}
}

func (s *stuckToggle) Toggle() (string, bool, error) {
	<-s.closed
	return "", false, errors.New("closed")
}
func (s *stuckToggle) Hangup() error { close(s.closed); return nil }

func TestLogMarkDoesNotSplitALine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.log")
	l, err := openLog(path, "hdr", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Write([]byte("root@host:~# ls /e"))
	_ = l.Mark("BREAK sent")
	_ = l.Write([]byte("tc\r\nnext\r\npart"))
	_ = l.Mark("at close")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 6 || lines[1] != "root@host:~# ls /etc" || !strings.HasSuffix(lines[2], "BREAK sent ---") ||
		lines[3] != "next" || lines[4] != "part" || !strings.HasSuffix(lines[5], "at close ---") {
		t.Errorf("marker split or lost a line:\n%s", b)
	}
}

func TestCloseResetsRemoteModes(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out, "h", "sol", 24, 80)
	c.Start()
	c.Output([]byte("\x1b[?1049h\x1b[?25l\x1b[?1003h"))
	out.Reset()
	c.Close()
	s := out.String()
	for _, want := range []string{"\x1b[?1049l", "\x1b[?25h", "\x1b[?1000l", "\x1b[?1002l", "\x1b[?1003l",
		"\x1b[?1006l", "\x1b[?1004l", "\x1b[?1l", "\x1b>", "\x1b(B", "\x0f", "\x1b[?2004l", "\x1b[r"} {
		if !strings.Contains(s, want) {
			t.Errorf("close missing %q in %q", want, s)
		}
	}
	if strings.Index(s, "\x1b[?1049l") > strings.Index(s, "\x1b[r") {
		t.Errorf("alternate screen must be left before the region is cleared: %q", s)
	}
}

func TestPasteReenabledAfterRemoteTurnsItOff(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out, "h", "sol", 24, 80)
	c.Start()
	defer c.Close()
	out.Reset()
	c.Output([]byte("prompt\x1b[?2004l"))
	if !strings.Contains(out.String(), "\x1b[?2004h") {
		t.Errorf("bracketed paste not turned back on: %q", out.String())
	}
	out.Reset()
	c.Output([]byte("more"))
	if strings.Contains(out.String(), "\x1b[?2004h") {
		t.Errorf("paste re-sent with nothing to repair: %q", out.String())
	}
}

func TestNoRepaintWhenNothingChanged(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out, "h", "sol", 24, 80)
	c.Start()
	defer c.Close()
	c.Output([]byte("hello\r\n"))
	out.Reset()
	c.tickOnce(time.Now().Add(10 * time.Second))
	if out.Len() != 0 {
		t.Errorf("idle tick repainted a clean bar: %q", out.String())
	}
	c.mu.Lock()
	c.dirty = true
	c.mu.Unlock()
	c.tickOnce(time.Now().Add(10 * time.Second))
	if !strings.Contains(out.String(), "\x1b[24;1H") {
		t.Errorf("dirty bar not repainted: %q", out.String())
	}
}

func TestClearScreenSplitAcrossReadsIsDetected(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out, "h", "sol", 24, 80)
	c.Start()
	defer c.Close()
	c.Output([]byte("x\x1b[2"))
	out.Reset()
	c.Output([]byte("Jy"))
	if !strings.Contains(out.String(), "\x1b[1;23r") {
		t.Errorf("split clear screen not detected, no repaint: %q", out.String())
	}
	var s clobberScan
	s.Feed([]byte("\x1b"))
	if clob, _ := s.Feed([]byte("c")); !clob {
		t.Error("ESC split from c not detected")
	}
}

func TestBarSelectsNormalCharset(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out, "h", "sol", 24, 80)
	c.Start()
	defer c.Close()
	if !strings.Contains(out.String(), "\x1b[24;1H\x1b(B\x0f") {
		t.Errorf("start paint does not select the normal charset: %q", out.String())
	}
	out.Reset()
	c.SetSize(30, 80)
	s := out.String()
	i, j := strings.Index(s, "\x1b(B\x0f"), strings.Index(s, "\x1b[?7l")
	if i < 0 || j < i || !strings.HasSuffix(s, "\x1b8") {
		t.Errorf("repaint order wrong: %q", s)
	}
}

func TestEraseToEndOfScreenRepaintsBar(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out, "h", "sol", 24, 80)
	c.Start()
	defer c.Close()
	out.Reset()
	c.Output([]byte("\x1b[H\x1b[J"))
	if !strings.Contains(out.String(), "\x1b[24;1H") {
		t.Errorf("bar not repainted after CSI H CSI J: %q", out.String())
	}
}

func TestLogMarkersKeepOrderAfterErasedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.log")
	l, err := openLog(path, "hdr", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Write([]byte("prompt"))
	_ = l.Mark("first")
	_ = l.Write([]byte("\r\x1b[2K"))
	_ = l.Mark("second")
	_ = l.Write([]byte("done\r\n"))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	i, j := strings.Index(string(b), "first ---"), strings.Index(string(b), "second ---")
	if i < 0 || j < 0 || i > j {
		t.Errorf("markers out of order (first=%d second=%d):\n%s", i, j, b)
	}
}

func TestConsoleReleasesRegionOnAlternateScreen(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	c := newConsole(&out, "node01", "vsp", 24, 80)
	c.Start()
	out.Reset()

	// vim starts, its start split across reads: the region is released and the
	// bar row blanked, and nothing paints the bar while it runs.
	c.Output([]byte("\x1b[?10"))
	c.Output([]byte("49h\x1b[2J~\r\n"))
	s := out.String()
	if !strings.Contains(s, "\x1b[r") {
		t.Fatalf("alternate screen: region not released: %q", s)
	}
	if strings.Contains(s, "\x1b[24;1H") || strings.Contains(s, "\x1b[2K") {
		t.Errorf("alternate screen: the bar row was touched: %q", s)
	}
	if strings.Contains(s, "\x1b[1;23r") {
		t.Errorf("region set while on the alternate screen: %q", s)
	}
	out.Reset()
	c.SetMode(modeCommand)
	c.Note("hello", false)
	c.Output([]byte("\x1b[r\x1b[2Jmore"))
	if s := out.String(); strings.Contains(s, "\x1b[1;23r") || strings.Contains(s, "hello") {
		t.Errorf("bar painted on the alternate screen: %q", s)
	}
	out.Reset()

	// vim exits: the region and bar are back at once.
	c.Output([]byte("\x1b[r\x1b[?1049l$ "))
	if s := out.String(); !strings.Contains(s, "\x1b[1;23r") || !strings.Contains(s, "\x1b[24;1H") {
		t.Errorf("leaving the alternate screen: region and bar not restored: %q", s)
	}
}

func TestConsoleBareRegionResetAtShellRestores(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	c := newConsole(&out, "node01", "vsp", 24, 80)
	c.Start()
	out.Reset()
	c.Output([]byte("x\x1b[r"))
	if s := out.String(); !strings.Contains(s, "\x1b[1;23r") || !strings.Contains(s, "\x1b[24;1H") {
		t.Errorf("CSI r at a shell: region and bar not restored: %q", s)
	}
}

func TestAlternateScreenFirstFrameInTheSameChunk(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	c := newConsole(&out, "node01", "vsp", 24, 80)
	c.Start()
	out.Reset()
	frame := "\x1b[1;1Htop\x1b[24;1Hlast row\x1b[1;24r\x1b[24;80H"
	c.Output([]byte("$ vim\r\n\x1b[?1049h" + frame))
	s := out.String()
	sw, rel := strings.Index(s, "\x1b[?1049h"), strings.Index(s, "\x1b7\x1b[r\x1b8")
	if sw < 0 || rel < sw+len("\x1b[?1049h") || rel > strings.Index(s, frame) {
		t.Fatalf("region not released between the switch and the frame: %q", s)
	}
	if !strings.HasSuffix(s, frame) {
		t.Errorf("something was written after the first frame: %q", s[strings.Index(s, frame)+len(frame):])
	}
	// Leaving restores the region and bar after the leave sequence.
	out.Reset()
	c.Output([]byte("\x1b[?1049l$ "))
	s = out.String()
	if i, j := strings.Index(s, "\x1b[?1049l"), strings.Index(s, "\x1b[1;23r"); i < 0 || j < i || !strings.Contains(s, "\x1b[24;1H") {
		t.Errorf("leaving: region and bar not restored after the leave: %q", s)
	}
}
