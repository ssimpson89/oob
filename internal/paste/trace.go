package paste

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Trace is a debug log of a paste: bytes sent and received, with times, and
// the pacer's decisions. A nil *Trace logs nothing. Sent bytes are
// replaced by a count while a password is being typed. Everything else
// is written as it is, so the file holds the pasted text and the device's
// output: treat it as sensitive.
type Trace struct {
	mu    sync.Mutex
	f     *os.File
	start time.Time
}

// OpenTrace appends to the file at path, creating it private to the user.
func OpenTrace(path string) (*Trace, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil { // an older file may be world-readable
		f.Close()
		return nil, err
	}
	t := &Trace{f: f, start: time.Now()}
	t.line("--", "paste trace opened")
	return t, nil
}

// Close ends the trace.
func (t *Trace) Close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.f.Close()
	t.f = nil
}

func (t *Trace) line(kind, text string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return
	}
	fmt.Fprintf(t.f, "%s +%07.3fs %-4s %s\n", time.Now().Format("2006-01-02T15:04:05.000"), time.Since(t.start).Seconds(), kind, text)
}

func (t *Trace) tx(b []byte, secret bool) {
	if t == nil {
		return
	}
	if secret {
		t.line("TX", fmt.Sprintf("<%d bytes redacted: password>", len(b)))
		return
	}
	t.line("TX", fmt.Sprintf("%q", b))
}

func (t *Trace) rx(b []byte) { t.line("RX", fmt.Sprintf("%q", b)) }

func (t *Trace) note(s string) { t.line("PACE", s) }
