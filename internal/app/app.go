// Package app holds the few names and hooks every other package shares: the
// application name, the exit-code error, and the hooks through which library
// packages report notes and progress to whichever front end is running them.
package app

import (
	"fmt"
	"io"
	"os"
	"strconv"
)

// Name is the binary name and config directory name. Change it here and in
// go.mod (module line) to rename the tool.
const Name = "oob"

// ExitCode carries a child process exit code up to main.
type ExitCode int

// Error implements error.
func (e ExitCode) Error() string { return "exit status " + strconv.Itoa(int(e)) }

// Code turns a (status, error) pair into an error: err if set, an ExitCode
// for a non-zero status, nil otherwise.
func Code(n int, err error) error {
	if err != nil {
		return err
	}
	if n != 0 {
		return ExitCode(n)
	}
	return nil
}

// Notef prints a "note:" line to w.
func Notef(w io.Writer, format string, a ...any) {
	fmt.Fprintf(w, "note: "+format+"\n", a...)
}

// The hooks below are set once by the front end (cmd/oob) before any command
// runs. The defaults are safe for tests and other callers: notices print
// straight away, progress is dropped, and notes are not quiet.
var (
	// AddNotice queues a note to print once the terminal is back to normal.
	AddNotice = func(m string) { Notef(os.Stderr, "%s", m) }
	// Progressf shows a transient status line.
	Progressf = func(format string, a ...any) {}
	// Quiet reports whether transient notes are silenced (many hosts at once).
	Quiet = func() bool { return false }
)
