package console

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

type fakeQueue struct {
	mu      sync.Mutex
	queued  []string
	urgent  []string
	cleared int
	prog    func(pending, total int)
}

func (q *fakeQueue) Enqueue(b []byte) <-chan struct{} {
	q.mu.Lock()
	q.queued = append(q.queued, string(b))
	q.mu.Unlock()
	c := make(chan struct{})
	close(c)
	return c
}
func (q *fakeQueue) Urgent(b []byte) {
	q.mu.Lock()
	q.urgent = append(q.urgent, string(b))
	q.mu.Unlock()
}
func (q *fakeQueue) Clear()                                { q.mu.Lock(); q.cleared++; q.mu.Unlock() }
func (q *fakeQueue) OnProgress(f func(pending, total int)) { q.prog = f }

type fakeShared struct {
	notes  chan Note
	status SharedStatus
	toggle int
}

func (s *fakeShared) Notes() <-chan Note   { return s.notes }
func (s *fakeShared) Status() SharedStatus { return s.status }
func (s *fakeShared) TogglePause() error   { s.toggle++; return nil }

func TestBarShowsWhoIsOnASharedConsole(t *testing.T) {
	for _, tc := range []struct {
		s    SharedStatus
		want string
	}{
		{SharedStatus{Agent: "Claude"}, "shared · Claude connected"},
		{SharedStatus{Agent: "Claude", Working: true}, "shared · Claude working"},
		{SharedStatus{Agent: "Claude", Paused: true}, "shared · Claude paused"},
		{SharedStatus{Windows: 2}, "shared · 2 other windows"},
		{SharedStatus{}, ""},
	} {
		var out bytes.Buffer
		con := newConsole(&out, "node01", "sol", 24, 120)
		con.SetShared(tc.s)
		bar := barLine(con.st, 120)
		if tc.want == "" && strings.Contains(bar, "shared") {
			t.Errorf("%+v: a console shared with nobody shows %q", tc.s, bar)
		}
		if !strings.Contains(bar, tc.want) {
			t.Errorf("%+v: bar %q lacks %q", tc.s, bar, tc.want)
		}
	}
}

func TestPauseHintOnlyWhenShared(t *testing.T) {
	st := barState{host: "h", kind: "sol", mode: modeCommand}
	if bar := barLine(st, 200); strings.Contains(bar, "pause AI") {
		t.Errorf("an unshared console offers pausing: %q", bar)
	}
	st.sharing = true
	if bar := barLine(st, 200); !strings.Contains(bar, "pause AI") {
		t.Errorf("a shared console doesn't: %q", bar)
	}
	st.aiPaused = true
	if bar := barLine(st, 200); !strings.Contains(bar, "resume AI") {
		t.Errorf("a paused AI can't be resumed from the bar: %q", bar)
	}
	if !strings.Contains(helpText(), "Pause the AI tool") {
		t.Error("help doesn't list the pause key")
	}
}

func TestSharedKeysGoToTheHubsQueue(t *testing.T) {
	var out bytes.Buffer
	con := newConsole(&out, "h", "sol", 24, 80)
	con.Start()
	defer con.Close()
	q := &fakeQueue{}
	sh := &fakeShared{notes: make(chan Note, 1)}
	cs := &Setup{Link: &fakeLink{}, Queue: q, Shared: sh}
	stop := make(chan struct{})
	defer close(stop)
	k := newKeys(cs, con, func() {}, stop)
	k.start(nil)
	if k.ty != nil || q.prog == nil {
		t.Fatal("a shared console still runs its own typer")
	}
	k.Input([]byte("ls\r"))
	k.Input([]byte("\x03"))
	esc := string(rune(escapeByte))
	k.Input([]byte(esc + "i"))
	k.Input([]byte(esc + "x"))
	q.mu.Lock()
	defer q.mu.Unlock()
	if strings.Join(q.queued, "") != "ls\r" || len(q.urgent) != 1 || q.urgent[0] != "\x03" {
		t.Errorf("queued %q urgent %q", q.queued, q.urgent)
	}
	if sh.toggle != 1 {
		t.Errorf("the i key toggled the pause %d times", sh.toggle)
	}
	if q.cleared != 1 {
		t.Errorf("cleared %d times", q.cleared)
	}
}

func TestSharedNoteIsShownAndMarked(t *testing.T) {
	var out bytes.Buffer
	con := newConsole(&out, "h", "sol", 24, 120)
	con.Start()
	defer con.Close()
	con.SharedNote(Note{Text: "Claude is working · {keys} i pauses it"}, SharedStatus{Agent: "Claude", Working: true})
	if !strings.Contains(con.st.note, "^] i pauses it") {
		t.Errorf("note = %q", con.st.note)
	}
	if con.st.shared != "shared · Claude working" {
		t.Errorf("shared = %q", con.st.shared)
	}
}
