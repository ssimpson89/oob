package paste

import (
	"testing"
	"time"
)

func TestHubDeliversInOrderWithoutBlocking(t *testing.T) {
	var h Hub
	rx, untap := h.Tap()
	defer untap()
	for _, s := range []string{"a", "b", "c"} { // nobody is reading yet: Feed must not block
		h.Feed([]byte(s))
	}
	for _, want := range []string{"a", "b", "c"} {
		select {
		case got := <-rx:
			if string(got) != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("no %q", want)
		}
	}
}

func TestHubUntapStops(t *testing.T) {
	var h Hub
	rx, untap := h.Tap()
	untap()
	untap() // twice is fine
	h.Feed([]byte("x"))
	select {
	case b := <-rx:
		t.Fatalf("got %q after untap", b)
	case <-time.After(50 * time.Millisecond):
	}
}
