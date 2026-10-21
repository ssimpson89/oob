package console

import (
	"fmt"
	"sync"
	"time"

	"github.com/ssimpson89/oob/internal/app"
)

// Transcript records a console's output with no terminal involved: the plain
// text history the bar keeps (the newest 10000 lines, escape sequences
// removed, cursor moves applied), a count of completed lines so a reader can
// ask for what is new since it last looked, and an optional session log in
// the same format the bar's log uses.
//
// The text is plain but it is still remote-derived: pass what you hand to
// anyone through CleanRemote.
type Transcript struct {
	mu   sync.Mutex
	hist *history
	host string
	kind string
	log  *logWriter
}

// NewTranscript starts an empty transcript for host; kind labels the console
// ("sol", "com2", ...) in the log header.
func NewTranscript(host, kind string) *Transcript {
	return &Transcript{hist: newHistory(historyLinesMax), host: host, kind: kind}
}

// HistoryLines is how many completed lines a transcript keeps.
const HistoryLines = historyLinesMax

// Feed adds raw console output.
func (t *Transcript) Feed(raw []byte) {
	if len(raw) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hist.Feed(raw)
	if t.log != nil {
		if err := t.log.Write(raw); err != nil {
			_ = t.log.Close()
			t.log = nil
		}
	}
}

// Count is the number of lines completed so far, including any the history
// has dropped.
func (t *Transcript) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.hist.Count()
}

// Partial is the unfinished current line, usually a prompt.
func (t *Transcript) Partial() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.hist.Partial()
}

// Snapshot returns, in one step, the lines completed after the first seen
// lines (newest kept), how many of those were dropped from the history before
// anyone asked (lost), the count to pass as seen next time, and the unfinished
// current line.
func (t *Transcript) Snapshot(seen int) (lines []string, lost, count int, partial string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	count = t.hist.Count()
	n := max(0, count-seen)
	lost = max(0, n-t.hist.Len())
	return t.hist.Last(n - lost), lost, count, t.hist.Partial()
}

// Tail returns the newest n completed lines, oldest first.
func (t *Transcript) Tail(n int) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.hist.Last(n)
}

// StartLog starts a session log under dir (the oob state directory's logs
// folder when dir is empty), beginning with the history so far. It returns the
// log's path. Logging an already logged transcript is a no-op.
func (t *Transcript) StartLog(dir string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.log != nil {
		return t.log.Path(), nil
	}
	now := time.Now()
	var path string
	if dir != "" {
		path = joinLogDir(dir, t.host, now)
	} else {
		p, err := logPath(t.host, now)
		if err != nil {
			return "", err
		}
		path = p
	}
	path = uniqueLogPath(path)
	header := fmt.Sprintf("%s mcp log: %s (%s), started %s; an AI agent typed what is sent here",
		app.Name, t.host, t.kind, now.Format("2006-01-02 15:04:05"))
	l, err := openLog(path, header, t.hist.Last(t.hist.Len()), t.hist.Partial())
	if err != nil {
		return "", err
	}
	t.log = l
	return path, nil
}

// Mark writes a marker line in the log, if one is open.
func (t *Transcript) Mark(msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.log != nil {
		_ = t.log.Mark(msg)
	}
}

// Close ends the log, if any, and returns its path ("" for none).
func (t *Transcript) Close() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.log == nil {
		return "", nil
	}
	path := t.log.Path()
	err := t.log.Close()
	t.log = nil
	return path, err
}
