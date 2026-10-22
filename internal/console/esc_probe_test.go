package console

import (
	"testing"
	"time"
)

// A lone ESC, as vim's users press it, must reach the remote.
func TestLoneEscReachesTheRemote(t *testing.T) {
	for _, serial := range []bool{false} {
		pl := &plainLink{note: make(chan struct{}, 1)}
		r := newRig(t, pl, Pacing{}, nil)
		r.k.Input([]byte{0x1b})
		time.Sleep(escWait + 20*time.Millisecond)
		r.k.Flush()
		r.k.Input([]byte(":"))
		pl.waitSent(t, 2)
		pl.mu.Lock()
		got := string(pl.sent)
		pl.mu.Unlock()
		if got != "\x1b:" {
			t.Errorf("serial=%v: sent %q, want ESC then :", serial, got)
		}
	}
}

func TestLoneEscReachesASerialRemote(t *testing.T) {
	d := newSerialDev()
	r := newRig(t, d, Pacing{}, nil)
	r.k.Input([]byte{0x1b})
	time.Sleep(escWait + 20*time.Millisecond)
	r.k.Flush()
	r.k.Input([]byte(":"))
	d.waitSent(t, 2)
	d.mu.Lock()
	got := string(d.sent)
	d.mu.Unlock()
	if got != "\x1b:" {
		t.Errorf("sent %q, want ESC then :", got)
	}
}

func TestEscThenKeyBeforeTheHoldEnds(t *testing.T) {
	pl := &plainLink{note: make(chan struct{}, 1)}
	r := newRig(t, pl, Pacing{}, nil)
	for _, b := range []string{"i", "a", "\x1b", ":", "q", "!", "\r"} {
		r.k.Input([]byte(b))
	}
	pl.waitSent(t, 7)
	pl.mu.Lock()
	got := string(pl.sent)
	pl.mu.Unlock()
	if got != "ia\x1b:q!\r" {
		t.Errorf("sent %q, want %q", got, "ia\x1b:q!\r")
	}
	if m := r.k.parser.Mode(); m != 0 {
		t.Errorf("parser left in mode %v", m)
	}
}
