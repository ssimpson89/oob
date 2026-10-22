// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// Adapted from bauddy (internal/logfile/logfile.go): the writer, marker lines
// and file naming. Like bauddy, escape sequences are removed and backspaces
// and carriage-return overwrites applied, so the file reads like the screen.

package console

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/termtext"
)

// logWriter is an open console log.
type logWriter struct {
	f    *os.File
	path string
	c    termtext.Cleaner
	// held is marker text waiting for the open line to end.
	held []byte
}

// openLog appends to path (creating it, mode 0600: logs can hold configs and
// secrets), writes header, then the earlier lines and the unfinished line, so
// a log started late includes what was already on screen.
func openLog(path, header string, earlier []string, partial string) (*logWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l := &logWriter{f: f, path: path}
	var pre []byte
	pre = append(pre, "# "+header+"\n"...)
	for _, ln := range earlier {
		pre = append(pre, ln+"\n"...)
	}
	_, err = f.Write(pre)
	if err == nil && (len(earlier) > 0 || partial != "") {
		err = l.Mark("logging turned on; output above was captured before that")
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	if partial != "" {
		l.c.Feed([]byte(partial)) // the line in progress continues in the log
	}
	return l, nil
}

// Path returns the file's path.
func (l *logWriter) Path() string { return l.path }

// Write logs raw output. Complete lines are written at once; a partial line
// waits for its newline or Close. Markers held back while a line was open go
// out right after that line ends.
func (l *logWriter) Write(raw []byte) error {
	out := l.c.Feed(raw)
	if len(l.held) > 0 && len(out) > 0 {
		// The first line out is the one that was open: it ends at the first newline.
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			joined := make([]byte, 0, len(out)+len(l.held))
			joined = append(joined, out[:i+1]...)
			joined = append(joined, l.held...)
			out = append(joined, out[i+1:]...)
			l.held = nil
		}
	}
	_, err := l.f.Write(out)
	return err
}

// Mark writes a marker line such as "--- 09:13:01 BREAK sent ---". A marker
// never splits a line: while one is open the marker waits for its end.
func (l *logWriter) Mark(msg string) error {
	m := fmt.Sprintf("--- %s %s ---\n", time.Now().Format("15:04:05"), msg)
	// Queue behind anything already held too: an erased line leaves Partial()
	// empty while older markers still wait, and order must be kept.
	if l.c.Partial() != "" || len(l.held) > 0 {
		l.held = append(l.held, m...)
		return nil
	}
	_, err := l.f.WriteString(m)
	return err
}

// Close writes any partial line, then any held markers, and closes the file.
func (l *logWriter) Close() error {
	out := append(l.c.Flush(), l.held...)
	l.held = nil
	_, werr := l.f.Write(out)
	if err := l.f.Close(); err != nil {
		return err
	}
	return werr
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// logName is the generated file name, e.g. node01-20261010-091203.log.
func logName(host string, now time.Time) string {
	return fmt.Sprintf("%s-%s.log", unsafeChars.ReplaceAllString(host, "-"), now.Format("20060102-150405"))
}

// stateDir is $XDG_STATE_HOME/oob, else ~/.local/state/oob.
func stateDir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, app.Name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", app.Name), nil
}

// logPath is where a log for host started at now goes.
func logPath(host string, now time.Time) (string, error) {
	d, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "logs", logName(host, now)), nil
}

// joinLogDir is where a log for host started at now goes under dir.
func joinLogDir(dir, host string, now time.Time) string {
	return filepath.Join(dir, logName(host, now))
}

// uniqueLogPath returns path, or path with -2, -3... before the extension if a
// file is already there (a log restarted within the same second).
func uniqueLogPath(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	p := path
	for i := 2; i < 1000; i++ {
		if _, err := os.Lstat(p); err != nil {
			return p
		}
		p = fmt.Sprintf("%s-%d%s", base, i, ext)
	}
	return p
}
