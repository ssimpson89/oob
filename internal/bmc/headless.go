package bmc

import (
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// Headless use. The command line lets drivers print straight to the terminal
// and lets ipmitool inherit stdin. A front end with no terminal (the MCP
// server, whose stdout and stdin carry the protocol) runs a driver inside
// Capture instead: everything the driver prints, and everything ipmitool says
// on stdout or stderr, lands in a bounded buffer, ipmitool gets no stdin, and
// ipmitool is killed when the context ends.
//
// Capture runs one call at a time, because the driver code prints through
// package-level seams rather than a per-call writer.

// captureMax bounds the text one Capture keeps; the rest is dropped.
const captureMax = 256 << 10

type ioState struct {
	out io.Writer
	ctx context.Context
}

var (
	capturing  atomic.Pointer[ioState]
	captureSem = make(chan struct{}, 1)
)

// Stdout is where a driver's own output goes: the terminal, or the buffer of
// the Capture that is running. Fake drivers in tests print here too.
func Stdout() io.Writer {
	if s := capturing.Load(); s != nil {
		return s.out
	}
	return os.Stdout
}

// stderrW is where ipmitool's stderr goes: the terminal, or the capture.
func stderrW() io.Writer {
	if s := capturing.Load(); s != nil {
		return s.out
	}
	return os.Stderr
}

// stdinR is ipmitool's stdin: the terminal, or nothing while capturing.
func stdinR() io.Reader {
	if capturing.Load() != nil {
		return nil
	}
	return os.Stdin
}

// runCtx is the context ipmitool runs under.
func runCtx() context.Context {
	if s := capturing.Load(); s != nil {
		return s.ctx
	}
	return context.Background()
}

// Capture runs fn with its output collected instead of printed, and returns
// that output (at most about captureMax bytes) and fn's error. It waits its turn
// behind other captures, or gives up when ctx ends. ipmitool runs started by
// fn are killed when ctx ends.
func Capture(ctx context.Context, fn func() error) (string, error) {
	select {
	case captureSem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-captureSem }()
	buf := &boundedBuffer{max: captureMax}
	capturing.Store(&ioState{out: buf, ctx: ctx})
	defer capturing.Store(nil)
	err := fn()
	return buf.String(), err
}

// boundedBuffer keeps the first headMax bytes written to it and the last
// max-headMax, with a marker where the middle was dropped: the start of an
// output says what it is and the end of a log is its newest part.
type boundedBuffer struct {
	mu      sync.Mutex
	head    []byte
	tail    []byte
	max     int
	dropped bool
}

func (w *boundedBuffer) headMax() int { return w.max / 4 }

func (w *boundedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if room := w.headMax() - len(w.head); room > 0 && len(w.tail) == 0 {
		take := min(room, len(p))
		w.head = append(w.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	w.tail = append(w.tail, p...)
	if keep := w.max - w.headMax(); len(w.tail) > keep {
		w.tail = append([]byte(nil), w.tail[len(w.tail)-keep:]...)
		w.dropped = true
	}
	return n, nil
}

func (w *boundedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dropped {
		return string(w.head) + "\n...(output truncated)...\n" + string(w.tail)
	}
	return string(w.head) + string(w.tail)
}
