package typer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	cd = 10 * time.Millisecond
	ld = 200 * time.Millisecond
)

// rig runs a Typer against a recording writer. Its Wait never sleeps: it logs
// the delay it was asked for, then either fires at once (auto) or when the
// test calls step.
type rig struct {
	t      *testing.T
	ty     *Typer
	auto   bool
	cancel context.CancelFunc
	errc   chan error

	mu     sync.Mutex
	log    []string // "w:<bytes>" and "d:<delay>", in the order they happened
	writes chan string
	waits  chan chan time.Time
	fail   error
}

func newRig(t *testing.T, auto bool, char, line time.Duration) *rig {
	t.Helper()
	r := &rig{t: t, auto: auto, errc: make(chan error, 1), writes: make(chan string, 1024), waits: make(chan chan time.Time, 1024)}
	r.ty = &Typer{CharDelay: char, LineDelay: line, Wait: r.wait}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	go func() { r.errc <- r.ty.Run(ctx, r) }()
	return r
}

func (r *rig) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return 0, r.fail
	}
	r.log = append(r.log, "w:"+fmt.Sprintf("%q", b))
	r.writes <- string(b)
	return len(b), nil
}

func (r *rig) wait(d time.Duration) <-chan time.Time {
	r.mu.Lock()
	r.log = append(r.log, "d:"+d.String())
	r.mu.Unlock()
	ch := make(chan time.Time, 1)
	if r.auto {
		ch <- time.Time{}
	} else {
		r.waits <- ch
	}
	return ch
}

// step lets the pending delay end.
func (r *rig) step() {
	r.t.Helper()
	select {
	case ch := <-r.waits:
		ch <- time.Time{}
	case <-time.After(5 * time.Second):
		r.t.Fatal("Run is not waiting on a delay")
	}
}

// next returns the next write.
func (r *rig) next() string {
	r.t.Helper()
	select {
	case s := <-r.writes:
		return s
	case <-time.After(5 * time.Second):
		r.t.Fatal("nothing was written")
		return ""
	}
}

// stop ends Run and returns what it logged, complete.
func (r *rig) stop() []string {
	r.t.Helper()
	r.cancel()
	select {
	case <-r.errc:
	case <-time.After(5 * time.Second):
		r.t.Fatal("Run did not return")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

func isClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("done channel never closed")
	}
}

func isOpen(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("done channel closed early")
	default:
	}
}

func TestBytesGoOutOneAtATimeWithLineDelayAfterEnter(t *testing.T) {
	r := newRig(t, true, cd, ld)
	isClosed(t, r.ty.Enqueue([]byte("ab\rc")))
	got := strings.Join(r.stop(), " ")
	want := `w:"a" d:10ms w:"b" d:10ms w:"\r" d:200ms w:"c" d:10ms`
	if got != want {
		t.Errorf("log\n got  %s\n want %s", got, want)
	}
}

func TestCRLFIsOneEnter(t *testing.T) {
	r := newRig(t, true, cd, ld)
	isClosed(t, r.ty.Enqueue([]byte("a\r\nb\n")))
	got := strings.Join(r.stop(), " ")
	want := `w:"a" d:10ms w:"\r" d:10ms w:"\n" d:200ms w:"b" d:10ms w:"\n" d:200ms`
	if got != want {
		t.Errorf("log\n got  %s\n want %s", got, want)
	}
}

func TestLoneKeyGoesOutAtOnce(t *testing.T) {
	r := newRig(t, false, cd, ld)
	done := r.ty.Enqueue([]byte("a"))
	if got := r.next(); got != "a" {
		t.Fatalf("wrote %q", got)
	}
	isClosed(t, done)
	if log := r.stop(); log[0] != `w:"a"` {
		t.Errorf("log = %v, the first byte must not wait", log)
	}
}

func TestKeystrokesDuringAPasteLandAfterItInOrder(t *testing.T) {
	r := newRig(t, false, cd, ld)
	paste := r.ty.Enqueue([]byte("abc"))
	if r.next() != "a" {
		t.Fatal("paste did not start")
	}
	key := r.ty.Enqueue([]byte("x")) // typed while the paste is still going
	more := r.ty.Enqueue([]byte("y"))
	var got string
	for i := 0; i < 5; i++ {
		r.step()
		if i < 4 {
			got += r.next()
		}
	}
	if got != "bcxy" {
		t.Errorf("after the first byte came %q, want bcxy", got)
	}
	isClosed(t, paste)
	isClosed(t, key)
	isClosed(t, more)
}

func TestUrgentDropsTheQueueAndWritesAtOnce(t *testing.T) {
	r := newRig(t, false, cd, ld)
	done := r.ty.Enqueue([]byte("abcdef"))
	if r.next() != "a" {
		t.Fatal("paste did not start")
	}
	r.ty.Urgent([]byte{0x03}) // no step: the delay doesn't hold it back
	if got := r.next(); got != "\x03" {
		t.Fatalf("wrote %q, want ctrl+c", got)
	}
	isClosed(t, done)
	r.step()
	r.step()
	r.ty.Enqueue([]byte("z"))
	if got := r.next(); got != "z" {
		t.Errorf("wrote %q after the interrupt, want z", got)
	}
	if log := strings.Join(r.stop(), " "); strings.Contains(log, `"b"`) {
		t.Errorf("a dropped byte was written: %s", log)
	}
}

func TestClearDropsPendingAndClosesDone(t *testing.T) {
	r := newRig(t, false, cd, ld)
	first := r.ty.Enqueue([]byte("abc"))
	second := r.ty.Enqueue([]byte("def"))
	if r.next() != "a" {
		t.Fatal("did not start")
	}
	isOpen(t, first)
	r.ty.Clear()
	isClosed(t, first)
	isClosed(t, second)
	r.step()
	if log := strings.Join(r.stop(), " "); log != `w:"a" d:10ms` {
		t.Errorf("log = %s, want only the first byte", log)
	}
}

func TestCancelStopsMidQueue(t *testing.T) {
	r := newRig(t, false, cd, ld)
	done := r.ty.Enqueue([]byte("abc"))
	if r.next() != "a" {
		t.Fatal("did not start")
	}
	r.cancel()
	if err := <-r.errc; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
	isClosed(t, done)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.log) != 2 { // the first byte and its delay; nothing after
		t.Errorf("log = %v", r.log)
	}
	// After Run is gone, calls return at once rather than hang.
	isClosed(t, r.ty.Enqueue([]byte("x")))
	r.ty.Urgent([]byte("x"))
	r.ty.Clear()
}

func TestNoDelaysIsAPlainWriter(t *testing.T) {
	r := newRig(t, true, 0, 0)
	isClosed(t, r.ty.Enqueue([]byte("hello\r\nworld\r")))
	if got := r.next(); got != "hello\r\nworld\r" {
		t.Errorf("wrote %q in one go, want the whole text", got)
	}
	if log := r.stop(); len(log) != 1 {
		t.Errorf("log = %v, want a single write and no delays", log)
	}
}

func TestPacedCanSwitchOff(t *testing.T) {
	r := newRig(t, true, cd, ld)
	r.ty.Paced = func() bool { return false }
	isClosed(t, r.ty.Enqueue([]byte("abc")))
	if got := r.next(); got != "abc" {
		t.Errorf("wrote %q, want all at once", got)
	}
}

func TestProgress(t *testing.T) {
	var mu sync.Mutex
	var got []string
	r := &rig{t: t, auto: true, errc: make(chan error, 1), writes: make(chan string, 64), waits: make(chan chan time.Time, 64)}
	r.ty = &Typer{CharDelay: cd, LineDelay: ld, Wait: r.wait, Progress: func(p, total int) {
		mu.Lock()
		got = append(got, fmt.Sprintf("%d/%d", p, total))
		mu.Unlock()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { r.errc <- r.ty.Run(ctx, r) }()
	isClosed(t, r.ty.Enqueue([]byte("ab")))
	isClosed(t, r.ty.Enqueue([]byte("c")))
	cancel()
	<-r.errc
	mu.Lock()
	defer mu.Unlock()
	// Pending counts down, and the total resets once the queue is empty. The
	// two Enqueues can land in the same burst or in two, so check the shape.
	if len(got) == 0 || got[len(got)-1] != "0/0" {
		t.Fatalf("progress = %v, want it to end at 0/0", got)
	}
	for _, g := range got {
		var p, total int
		fmt.Sscanf(g, "%d/%d", &p, &total)
		if p > total || total > 3 {
			t.Errorf("progress %s out of range", g)
		}
	}
}

func TestWriteErrorEndsRunAndReleasesEveryone(t *testing.T) {
	r := newRig(t, true, cd, ld)
	r.fail = errors.New("link down")
	done := r.ty.Enqueue([]byte("abc"))
	if err := <-r.errc; err == nil || !strings.Contains(err.Error(), "link down") {
		t.Errorf("Run = %v, want the write error", err)
	}
	isClosed(t, done)
	isClosed(t, r.ty.Enqueue([]byte("x")))
}

func TestLoneEscGetsRoomButSequencesDont(t *testing.T) {
	r := newRig(t, true, cd, ld)
	r.ty.EscDelay = 150 * time.Millisecond
	// ESC then ":" (vim) gets the ESC gap; an arrow key (ESC [ A) does not.
	isClosed(t, r.ty.Enqueue([]byte("\x1b:\x1b[A")))
	got := strings.Join(r.stop(), " ")
	want := `w:"\x1b" d:150ms w:":" d:10ms w:"\x1b" d:10ms w:"[" d:10ms w:"A" d:10ms`
	if got != want {
		t.Errorf("log\n got  %s\n want %s", got, want)
	}
}

func TestQueuedIsExactAfterEnqueueReturns(t *testing.T) {
	r := newRig(t, false, cd, ld)
	if r.ty.Queued() != 0 {
		t.Fatalf("Queued = %d on a fresh typer", r.ty.Queued())
	}
	r.ty.Enqueue([]byte("abc"))
	if got := r.ty.Queued(); got != 3 && got != 2 { // "a" may already be out
		t.Errorf("Queued = %d right after Enqueue(abc)", got)
	}
	r.next()
	r.ty.Clear()
	if got := r.ty.Queued(); got != 0 {
		t.Errorf("Queued = %d after Clear, want 0", got)
	}
}

// With no per-char delay but a line delay, nothing yields between bytes, so
// Urgent has to be serviced before each write or ctrl+c waits out the line.
func TestUrgentLandsBeforeEnterWithZeroCharDelay(t *testing.T) {
	gate := make(chan struct{})
	var mu sync.Mutex
	var out []byte
	ty := &Typer{LineDelay: ld, Wait: func(time.Duration) <-chan time.Time { return make(chan time.Time) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := writeFn(func(b []byte) (int, error) {
		mu.Lock()
		first := len(out) == 0
		out = append(out, b...)
		mu.Unlock()
		if first {
			<-gate // hold the first byte's write while ctrl+c is requested
		}
		return len(b), nil
	})
	go func() { _ = ty.Run(ctx, w) }()
	ty.Enqueue([]byte("reboot\r"))
	for {
		mu.Lock()
		n := len(out)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	go ty.Urgent([]byte{0x03})
	time.Sleep(50 * time.Millisecond) // Urgent is now blocked sending
	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := string(out)
		mu.Unlock()
		if strings.Contains(got, "\x03") {
			if strings.Contains(got, "\r") {
				t.Fatalf("wrote %q, ctrl+c must land before Enter", got)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("ctrl+c never written")
}

type writeFn func([]byte) (int, error)

func (f writeFn) Write(b []byte) (int, error) { return f(b) }

// While Run is stuck inside a write, Enqueue, Urgent, Clear and Queued must
// return at once, and text queued before an Urgent or Clear must never be
// written after it.
func TestCallersNeverWaitOnABlockedWriter(t *testing.T) {
	for _, name := range []string{"urgent", "clear"} {
		t.Run(name, func(t *testing.T) {
			gate := make(chan struct{})
			var mu sync.Mutex
			var out []string
			entered := make(chan struct{})
			ty := &Typer{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := writeFn(func(b []byte) (int, error) {
				mu.Lock()
				out = append(out, string(b))
				first := len(out) == 1
				mu.Unlock()
				if first {
					close(entered)
					<-gate
				}
				return len(b), nil
			})
			go func() { _ = ty.Run(ctx, w) }()
			ty.Enqueue([]byte("first"))
			<-entered // Run is inside the write
			calls := make(chan struct{})
			var old <-chan struct{}
			go func() {
				old = ty.Enqueue([]byte("OLD"))
				_ = ty.Queued()
				if name == "urgent" {
					ty.Urgent([]byte{0x03})
				} else {
					ty.Clear()
				}
				close(calls)
			}()
			select {
			case <-calls:
			case <-time.After(5 * time.Second):
				t.Fatal("a caller blocked on the stuck writer")
			}
			isClosed(t, old)
			if got := ty.Queued(); got != 0 {
				t.Errorf("Queued = %d, want 0", got)
			}
			close(gate)
			deadline := time.Now().Add(5 * time.Second)
			for name == "urgent" && time.Now().Before(deadline) {
				mu.Lock()
				n := len(out)
				mu.Unlock()
				if n >= 2 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, s := range out {
				if strings.Contains(s, "OLD") {
					t.Fatalf("stale submission was written: %q", out)
				}
			}
			if name == "urgent" && (len(out) != 2 || out[1] != "\x03") {
				t.Errorf("out = %q, want first then ctrl+c", out)
			}
		})
	}
}

// Regression: no char delay but a line delay, a paste in progress with the
// writer stuck, an ordinary key read meanwhile, then ctrl+c. None of the
// calls may block, and ctrl+c must be written before the paste's Enter.
func TestCtrlCServicedBeforeEnterWhileWriterBlocked(t *testing.T) {
	gate := make(chan struct{})
	var mu sync.Mutex
	var out []byte
	entered := make(chan struct{})
	ty := &Typer{LineDelay: ld, Wait: func(time.Duration) <-chan time.Time { return make(chan time.Time) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := writeFn(func(b []byte) (int, error) {
		mu.Lock()
		first := len(out) == 0
		out = append(out, b...)
		mu.Unlock()
		if first {
			close(entered)
			<-gate
		}
		return len(b), nil
	})
	go func() { _ = ty.Run(ctx, w) }()
	ty.Enqueue([]byte("reboot\r"))
	<-entered
	done := make(chan struct{})
	go func() {
		ty.Enqueue([]byte("k")) // an ordinary key during the paste
		ty.Urgent([]byte{0x03})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stdin handler blocked behind the stuck write")
	}
	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := string(out)
		mu.Unlock()
		if strings.Contains(got, "\x03") {
			if strings.Contains(got, "\r") {
				t.Fatalf("wrote %q, ctrl+c must land before Enter", got)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("ctrl+c never written")
}
