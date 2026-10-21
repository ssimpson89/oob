// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// Adapted from bauddy (internal/logfile/logfile.go): the writer, marker lines
// and file naming. Like bauddy, escape sequences are removed and backspaces
// and carriage-return overwrites applied, so the file reads like the screen.

package console

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ssimpson89/oob/internal/app"
)

// logWriter is an open console log.
type logWriter struct {
	f    *os.File
	path string
	c    termCleaner
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
// waits for its newline or Close.
func (l *logWriter) Write(raw []byte) error {
	_, err := l.f.Write(l.c.Feed(raw))
	return err
}

// Mark writes a marker line such as "--- 09:13:01 BREAK sent ---".
func (l *logWriter) Mark(msg string) error {
	out := l.c.Flush()
	out = append(out, fmt.Sprintf("--- %s %s ---\n", time.Now().Format("15:04:05"), msg)...)
	_, err := l.f.Write(out)
	return err
}

// Close writes any partial line and closes the file.
func (l *logWriter) Close() error {
	_, werr := l.f.Write(l.c.Flush())
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
