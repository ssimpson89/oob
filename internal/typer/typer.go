// Package typer is a paced input queue for slow serial links. Everything a
// user types or pastes goes through one queue and comes out one byte at a time
// with a short gap, a longer one after Enter, so a BMC's small input buffer
// never overruns and what was typed is what arrives.
package typer

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Typer paces bytes into a writer. Set the fields, then call Run in a
// goroutine; Enqueue, Urgent, Clear and Queued are safe to call from any
// other, and none of them ever waits on the writer: the queue is a buffer
// under a mutex that Run drains, so a stuck transport write cannot stall the
// caller (the stdin handler).
type Typer struct {
	// CharDelay is the gap after each byte, LineDelay the gap after CR or LF
	// (the remote may be busy running the line). The first byte of an idle
	// queue goes out at once; delays only separate bytes. Both zero makes
	// Typer a plain writer.
	CharDelay, LineDelay time.Duration
	// Paced, if set, is asked before each byte; while it says false the
	// delays are zero. A console that can move between a serial line and an
	// ssh prompt uses this.
	Paced func() bool
	// Wait returns a channel that fires after d. It defaults to time.After;
	// tests inject one that fires at once.
	Wait func(d time.Duration) <-chan time.Time
	// Progress, if set, is told how many bytes are still queued and how many
	// the current burst held in all. It runs in Run's goroutine, so it must
	// not block, and not call back into the Typer. Changes made by Urgent and
	// Clear are reported shortly after they return, not before.
	Progress func(pending, total int)

	once sync.Once
	wake chan struct{} // capacity 1: something changed, Run should look

	mu       sync.Mutex
	q        []byte // bytes accepted and not yet written or dropped; the one being written is still at the front
	waiters  []waiter
	accepted int      // bytes ever accepted
	total    int      // bytes in the current burst
	urgent   [][]byte // to write next, in order, ahead of q
	gen      uint64   // bumped when q is dropped, so Run knows a write in flight was dropped
	report   bool     // Progress is owed
	stopped  bool
}

type waiter struct {
	upto int // closed once this many bytes have been written or dropped
	done chan struct{}
}

func (t *Typer) init() {
	t.once.Do(func() { t.wake = make(chan struct{}, 1) })
}

func (t *Typer) poke() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// release closes the waiters whose bytes are all written or dropped. Caller
// holds mu.
func (t *Typer) release() {
	resolved := t.accepted - len(t.q)
	n := 0
	for _, wt := range t.waiters {
		if wt.upto > resolved {
			break
		}
		close(wt.done)
		n++
	}
	t.waiters = t.waiters[n:]
}

// dropLocked forgets everything queued. Caller holds mu.
func (t *Typer) dropLocked() {
	t.q, t.total = nil, 0
	t.gen++
	t.report = true
	t.release()
}

// Enqueue queues b behind everything already queued and returns at once. The
// returned channel closes once b has been written, or dropped by Clear,
// Urgent or the end of Run, so a caller can wait for its text to be out.
func (t *Typer) Enqueue(b []byte) <-chan struct{} {
	t.init()
	done := make(chan struct{})
	if len(b) == 0 {
		close(done)
		return done
	}
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		close(done)
		return done
	}
	if len(t.q) == 0 {
		t.total = 0
	}
	t.q = append(t.q, b...)
	t.accepted += len(b)
	t.total += len(b)
	t.waiters = append(t.waiters, waiter{t.accepted, done})
	t.report = true
	t.mu.Unlock()
	t.poke()
	return done
}

// Queued is how many bytes Enqueue has accepted that are neither written nor
// dropped yet. Unlike Progress it is exact the moment Enqueue is called, from
// any goroutine.
func (t *Typer) Queued() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.q)
}

// Urgent drops everything queued and has Run write b next, for ctrl+c and the
// like: whatever was waiting is stale once the user has interrupted. It does
// not wait for the write, which may be stuck.
func (t *Typer) Urgent(b []byte) {
	t.init()
	if len(b) == 0 {
		return
	}
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.dropLocked()
	t.urgent = append(t.urgent, append([]byte(nil), b...))
	t.mu.Unlock()
	t.poke()
}

// Clear drops everything queued and returns at once. Nothing dropped is
// written after it returns, except a byte already inside a blocked Write.
func (t *Typer) Clear() {
	t.init()
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.dropLocked()
	t.mu.Unlock()
	t.poke()
}

// Run writes queued bytes to w until ctx ends or a write fails, and returns
// that error. Whatever is queued then is dropped, and later calls do nothing.
func (t *Typer) Run(ctx context.Context, w io.Writer) error {
	t.init()
	wait := t.Wait
	if wait == nil {
		wait = time.After
	}
	var hold <-chan time.Time // the gap after the last byte, while it runs
	paced := func() bool {
		return (t.CharDelay > 0 || t.LineDelay > 0) && (t.Paced == nil || t.Paced())
	}
	// progress reports to the Progress callback; an unpaced write is over
	// before anyone could see it. Called without mu held.
	progress := func(pending, total int) {
		if t.Progress != nil && (pending == 0 || paced()) {
			t.Progress(pending, total)
		}
	}
	defer func() {
		t.mu.Lock()
		t.stopped = true
		t.urgent = nil
		t.dropLocked()
		t.report = false
		t.mu.Unlock()
		progress(0, 0)
	}()
	gap := func(last byte, next []byte) time.Duration {
		if !paced() {
			return 0
		}
		if last == '\r' && len(next) > 0 && next[0] == '\n' {
			return t.CharDelay // CRLF is one Enter
		}
		if last == '\r' || last == '\n' {
			return t.LineDelay
		}
		return t.CharDelay
	}
	write := func(b []byte) error {
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("typer: %w", err)
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		t.mu.Lock()
		// Urgent first, and before every write, so a ctrl+c lands between
		// bytes even when no delay would have yielded to it.
		if len(t.urgent) > 0 {
			b := t.urgent[0]
			t.urgent = t.urgent[1:]
			t.mu.Unlock()
			if err := write(b); err != nil {
				return err
			}
			if d := gap(b[len(b)-1], nil); d > 0 {
				hold = wait(d)
			}
			continue
		}
		if t.report {
			t.report = false
			pending, total := len(t.q), t.total
			t.mu.Unlock()
			progress(pending, total)
			continue
		}
		if len(t.q) > 0 && hold == nil {
			n := 1
			if !paced() {
				n = len(t.q) // one write, as a plain writer would
			}
			chunk := append([]byte(nil), t.q[:n]...)
			gen := t.gen
			t.mu.Unlock()
			if err := write(chunk); err != nil {
				return err
			}
			t.mu.Lock()
			var pending, total int
			if t.gen == gen { // else the write was dropped while it ran
				t.q = t.q[n:]
				if len(t.q) == 0 {
					t.q, t.total = nil, 0
				}
				t.release()
				t.report = false
				pending, total = len(t.q), t.total
				next := t.q[:min(1, len(t.q))] // for the CRLF check
				t.mu.Unlock()
				progress(pending, total)
				if d := gap(chunk[n-1], next); d > 0 {
					hold = wait(d)
				}
			} else {
				t.mu.Unlock()
				if d := gap(chunk[n-1], nil); d > 0 {
					hold = wait(d)
				}
			}
			continue
		}
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.wake:
		case <-hold:
			hold = nil
		}
	}
}
