package console

import (
	"os"
	"strings"
	"testing"
)

func TestTranscriptSinceTailAndLog(t *testing.T) {
	tr := NewTranscript("node01", "sol")
	tr.Feed([]byte("one\r\ntwo\r\n\x1b[31mred\x1b[0m\r\nlogin: "))
	if got := tr.Count(); got != 3 {
		t.Fatalf("Count = %d, want 3", got)
	}
	if got := tr.Partial(); got != "login: " {
		t.Errorf("Partial = %q", got)
	}
	lines, lost, count, partial := tr.Snapshot(1)
	if strings.Join(lines, "|") != "two|red" || lost != 0 || count != 3 || partial != "login: " {
		t.Errorf("Snapshot(1) = %q, %d, %d, %q", lines, lost, count, partial)
	}
	if got := strings.Join(tr.Tail(2), "|"); got != "two|red" {
		t.Errorf("Tail(2) = %q", got)
	}
	dir := t.TempDir()
	path, err := tr.StartLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	tr.Feed([]byte("root\r\n"))
	tr.Mark("typed")
	if p, err := tr.Close(); err != nil || p != path {
		t.Fatalf("Close = %q, %v", p, err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{"one", "red", "root", "typed", "node01"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("log lacks %q:\n%s", want, b)
		}
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v", st.Mode().Perm())
	}
}

func TestTranscriptDropsOldLinesAndSaysSo(t *testing.T) {
	tr := NewTranscript("node01", "sol")
	for i := 0; i < HistoryLines+50; i++ {
		tr.Feed([]byte("x\n"))
	}
	lines, lost, _, _ := tr.Snapshot(0)
	if len(lines) != HistoryLines || lost != 50 {
		t.Errorf("Snapshot(0): %d lines, %d lost", len(lines), lost)
	}
}
