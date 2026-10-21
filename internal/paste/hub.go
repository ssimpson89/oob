package paste

import "sync"

// Hub fans a device's output out to taps, so a Pacer can watch the echo
// while the console or session keeps reading the same stream. The zero Hub
// is ready to use.
type Hub struct {
	mu     sync.Mutex
	taps   map[*tap]struct{}
	screen screenTracker
}

// FullScreen reports whether the output so far left the alternate screen on:
// vim, less and the like are showing.
func (h *Hub) FullScreen() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.screen.on
}

// tap queues chunks without bound: Feed must never wait for a Pacer that is
// blocked writing, because the writer may itself be waiting for the output
// side to drain.
type tap struct {
	mu   sync.Mutex
	q    [][]byte
	wake chan struct{}
	ch   chan []byte
	done chan struct{}
}

// Tap starts a copy of the output from now on. Call the returned func to
// stop it; after that the channel yields nothing more.
func (h *Hub) Tap() (rx <-chan []byte, untap func()) {
	t := &tap{wake: make(chan struct{}, 1), ch: make(chan []byte), done: make(chan struct{})}
	h.mu.Lock()
	if h.taps == nil {
		h.taps = map[*tap]struct{}{}
	}
	h.taps[t] = struct{}{}
	h.mu.Unlock()
	go t.pump()
	var once sync.Once
	return t.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.taps, t)
			h.mu.Unlock()
			close(t.done)
		})
	}
}

// Feed gives b to every tap. It copies b, and never blocks.
func (h *Hub) Feed(b []byte) {
	if len(b) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.screen.feed(b)
	if len(h.taps) == 0 {
		return
	}
	c := append([]byte(nil), b...)
	for t := range h.taps {
		t.mu.Lock()
		t.q = append(t.q, c)
		t.mu.Unlock()
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
}

func (t *tap) pump() {
	for {
		t.mu.Lock()
		var b []byte
		if len(t.q) > 0 {
			b, t.q = t.q[0], t.q[1:]
		}
		t.mu.Unlock()
		if b == nil {
			select {
			case <-t.wake:
				continue
			case <-t.done:
				return
			}
		}
		select {
		case t.ch <- b:
		case <-t.done:
			return
		}
	}
}
