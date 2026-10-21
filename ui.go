package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"golang.org/x/term"
)

// Messages for the person at the terminal: progress lines while connecting,
// "note:" lines, and notes queued until the terminal is back to normal.

// quietNotes silences transient notes (progress lines, fallback and retry
// notes) while a table of many hosts is being built.
var quietNotes atomic.Bool

// setQuietNotes turns the quiet mode on or off. The status command turns it on.
func setQuietNotes(b bool) { quietNotes.Store(b) }

// notef prints a "note:" line to w.
func notef(w io.Writer, format string, a ...any) {
	fmt.Fprintf(w, "note: "+format+"\n", a...)
}

var notices struct {
	sync.Mutex
	msgs []string
}

func addNotice(m string) {
	notices.Lock()
	notices.msgs = append(notices.msgs, m)
	notices.Unlock()
}

// flushNotices prints queued notes to stderr, once each, after the terminal is
// back to normal.
func flushNotices() {
	notices.Lock()
	defer notices.Unlock()
	for _, m := range notices.msgs {
		notef(os.Stderr, "%s", m)
	}
	notices.msgs = nil
}

// progressf shows a transient status line on stderr, only when stderr is a
// terminal so scripts and pipes stay clean.
func progressf(format string, a ...any) {
	if quietNotes.Load() {
		return
	}
	if term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
	}
}
