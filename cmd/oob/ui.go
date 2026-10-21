package main

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/config"
	"golang.org/x/term"
)

// Messages for the person at the terminal: progress lines while connecting,
// "note:" lines, and notes queued until the terminal is back to normal.

// quietNotes silences transient notes (progress lines, fallback and retry
// notes) while a table of many hosts is being built.
var quietNotes atomic.Bool

// setQuietNotes turns the quiet mode on or off. The status command turns it on.
func setQuietNotes(b bool) { quietNotes.Store(b) }

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
		app.Notef(os.Stderr, "%s", m)
	}
	notices.msgs = nil
}

func init() {
	app.AddNotice = addNotice
	app.Progressf = progressf
	app.Quiet = quietNotes.Load
	config.Override = func() string { return cfgFlag }
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
