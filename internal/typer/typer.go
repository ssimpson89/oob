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
	"sync/atomic"
	"time"
)

// Typer paces bytes into a writer. Set the fields, then call Run in a
// goroutine; Enqueue, Urgent and Clear are safe to call from any other. The
// queue lives inside Run and is touched by nothing else.
type Typer struct {
	// CharDelay is the gap after each byte, LineDelay the gap after CR or LF
	// (the remote may be busy running the line). The first byte of an idle
	// queue goes out at once; delays only separate bytes. Both zero makes
	// Typer a plain writer.
	CharDelay, LineDelay time.Duration
	// EscDelay is the gap after a lone ESC (one not starting an arrow-key or
	// similar sequence). Programs like vim wait briefly after ESC to see if a
	// sequence follows; if the next key reaches them in that window, ESC and
	// the key merge into one "Alt-key" and both are lost. Over a serial
	// console keys typed apart can arrive bunched, so give ESC room.
	EscDelay time.Duration
	// Paced, if set, is asked before each byte; while it says false the
	// delays are zero. A console that can move between a serial line and an
	// ssh prompt uses this.
	Paced func() bool
	// Wait returns a channel that fires after d. It defaults to time.After;
	// tests inject one that fires at once.
	Wait func(d time.Duration) <-chan time.Time
	// Progress, if set, is told how many bytes are still queued and how many
	// the current burst held in all. It runs in Run's goroutine, so it must
	// not block, and not call back into the Typer.
	Progress func(pending, total int)

	queued atomic.Int64 // bytes accepted by Enqueue and not yet written or dropped

	// gen counts cancellations. Urgent and Clear bump it before they ask Run
	// to drop the queue; an Enqueue that captured an older value is blocked
	// handing its text over and is discarded by Run, since the cancellation
	// covers everything submitted before it.
	gen atomic.Uint64

	once    sync.Once
	in      chan item
	urgent  chan []byte
	clear   chan chan struct{}
	stopped chan struct{}
}

type item struct {
	b    []byte
	gen  uint64
	done chan struct{}
}

type waiter struct {
	upto int // closed once this many bytes have been written or dropped
	done chan struct{}
}

func (t *Typer) init() {
	t.once.Do(func() {
		t.in = make(chan item)
		t.urgent = make(chan []byte)
		t.clear = make(chan chan struct{})
		t.stopped = make(chan struct{})
	})
}

// Enqueue queues b behind everything already queued. The returned channel
// closes once b has been written, or dropped by Clear, Urgent or the end of
// Run, so a caller can wait for its text to be out.
func (t *Typer) Enqueue(b []byte) <-chan struct{} {
	t.init()
	done := make(chan struct{})
	if len(b) == 0 {
		close(done)
		return done
	}
	gen := t.gen.Load()
	t.queued.Add(int64(len(b))) // counted before Run can see it, so Queued never under-reports
	select {
	case t.in <- item{b, gen, done}:
	case <-t.stopped:
		t.queued.Add(-int64(len(b)))
		close(done)
	}
	return done
}

// Queued is how many bytes Enqueue has accepted that are neither written nor
// dropped yet. Unlike Progress it is exact the moment Enqueue is called, from
// any goroutine.
func (t *Typer) Queued() int { return int(t.queued.Load()) }

// Urgent drops everything queued and writes b now, for ctrl+c and the like:
// whatever was waiting is stale once the user has interrupted. That includes
// text whose Enqueue has not returned yet; it is dropped, never written.
func (t *Typer) Urgent(b []byte) {
	t.init()
	if len(b) == 0 {
		return
	}
	t.gen.Add(1)
	select {
	case t.urgent <- append([]byte(nil), b...):
	case <-t.stopped:
	}
}

// Clear drops everything queued, and returns once that is done: nothing
// dropped, including text from an Enqueue still in flight, is written after
// it, and Progress has been told.
func (t *Typer) Clear() {
	t.init()
	ack := make(chan struct{})
	t.gen.Add(1)
	select {
	case t.clear <- ack:
		<-ack
	case <-t.stopped:
	}
}

// Run writes queued bytes to w until ctx ends or a write fails, and returns
// that error. Whatever is queued then is dropped, and later calls do nothing.
func (t *Typer) Run(ctx context.Context, w io.Writer) error {
	t.init()
	wait := t.Wait
	if wait == nil {
		wait = time.After
	}
	var (
		q       []byte
		waiters []waiter
		sent    int              // bytes written or dropped, ever
		total   int              // bytes in the current burst
		hold    <-chan time.Time // the gap after the last byte, while it runs
	)
	paced := func() bool {
		return (t.CharDelay > 0 || t.LineDelay > 0 || t.EscDelay > 0) && (t.Paced == nil || t.Paced())
	}
	progress := func() { // an unpaced write is over before anyone could see it
		if t.Progress != nil && (len(q) == 0 || paced()) {
			t.Progress(len(q), total)
		}
	}
	release := func(upto int) { // close the waiters whose bytes are all accounted for
		n := 0
		for _, wt := range waiters {
			if wt.upto > upto {
				break
			}
			close(wt.done)
			n++
		}
		waiters = waiters[n:]
	}
	drop := func() {
		sent += len(q)
		t.queued.Add(-int64(len(q)))
		q, total = q[:0], 0
		release(sent)
		progress()
	}
	defer func() {
		close(t.stopped)
		drop()
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
		if last == 0x1b && (len(next) == 0 || (next[0] != '[' && next[0] != 'O')) {
			return max(t.EscDelay, t.CharDelay)
		}
		return t.CharDelay
	}
	write := func(b []byte) error {
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("typer: %w", err)
		}
		return nil
	}
	serveUrgent := func(b []byte) error {
		drop()
		if err := write(b); err != nil {
			return err
		}
		if d := gap(b[len(b)-1], nil); d > 0 {
			hold = wait(d)
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Service Urgent and Clear before every write, so a ctrl+c lands
		// between bytes even when no delay would have yielded to it.
		if len(q) > 0 && hold == nil {
			select {
			case b := <-t.urgent:
				if err := serveUrgent(b); err != nil {
					return err
				}
				continue
			case ack := <-t.clear:
				drop()
				close(ack)
				continue
			default:
			}
			n := 1
			if !paced() {
				n = len(q) // one write, as a plain writer would
			}
			chunk := q[:n]
			if err := write(chunk); err != nil {
				return err
			}
			last := chunk[n-1]
			q = q[n:]
			sent += n
			t.queued.Add(-int64(n))
			release(sent)
			if len(q) == 0 {
				total = 0
			}
			progress()
			if d := gap(last, q); d > 0 {
				hold = wait(d)
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case it := <-t.in:
			if it.gen != t.gen.Load() { // submitted before an Urgent or Clear: stale
				t.queued.Add(-int64(len(it.b)))
				close(it.done)
				continue
			}
			if len(q) == 0 {
				total = 0
			}
			q = append(q, it.b...)
			total += len(it.b)
			waiters = append(waiters, waiter{sent + len(q), it.done})
			progress()
		case b := <-t.urgent:
			if err := serveUrgent(b); err != nil {
				return err
			}
		case ack := <-t.clear:
			drop()
			close(ack)
		case <-hold:
			hold = nil
		}
	}
}
