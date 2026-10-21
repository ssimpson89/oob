package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"unicode"
	"unicode/utf8"

	"github.com/ssimpson89/oob/internal/app"
)

// Remote-derived text (BMC error bodies, CLI answers, ipmitool and racadm
// output, power state strings) is attacker-influenced on a compromised BMC.
// Printing it raw lets it move the cursor, retitle the window, or hide lines.
// CleanRemote is the one place that makes such text safe to print. The serial
// console is the exception: it stays raw on purpose.

// remoteMax bounds the text of one remote message (characters).
const (
	remoteMax     = 4096
	remoteLineMax = 300
)

// CleanRemoteN returns s with every escape sequence (CSI, OSC, DCS, APC, PM,
// SOS and two-byte escapes), C0 controls except tab and newline, DEL, C1
// controls, bidi/format characters and invalid UTF-8 removed. A carriage
// return counts as a newline when it does not already precede one. The result
// is cut to limit characters (no limit when limit <= 0), with a marker.
func CleanRemoteN(s string, limit int) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == 0x1b:
			i = skipEscape(rs, i)
			continue
		case r == '\r':
			if i+1 < len(rs) && rs[i+1] == '\n' {
				continue // the \n counts
			}
			r = '\n'
		case r == '\n' || r == '\t':
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			continue
		case unicode.Is(unicode.Cf, r):
			continue
		}
		if limit > 0 && n >= limit {
			b.WriteString("...(truncated)")
			return b.String()
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// skipEscape returns the index of the last rune of the escape sequence that
// starts at rs[i] (an ESC).
func skipEscape(rs []rune, i int) int {
	j := i + 1
	if j >= len(rs) {
		return i
	}
	switch c := rs[j]; {
	case c == '[': // CSI: parameters, intermediates, one final byte 0x40-0x7e
		for j++; j < len(rs); j++ {
			if rs[j] >= 0x40 && rs[j] <= 0x7e {
				return j
			}
		}
		return len(rs) - 1
	case c == ']' || c == 'P' || c == '_' || c == '^' || c == 'X':
		// A string runs until BEL, ST (ESC \ or U+009C) or the end of the text.
		for j++; j < len(rs); j++ {
			if rs[j] == 0x07 || rs[j] == 0x9c {
				return j
			}
			if rs[j] == 0x1b {
				if j+1 < len(rs) && rs[j+1] == '\\' {
					return j + 1
				}
				return j - 1 // a new escape starts; let the caller read it
			}
		}
		return len(rs) - 1
	case c >= 0x20 && c <= 0x2f: // intermediates, then a final byte
		for j++; j < len(rs); j++ {
			if rs[j] < 0x20 || rs[j] > 0x2f {
				return j
			}
		}
		return len(rs) - 1
	}
	return j // ESC plus one character
}

// CleanRemote makes multi-line remote text safe to print.
func CleanRemote(s string) string { return CleanRemoteN(s, remoteMax) }

// CleanRemoteLine is CleanRemote for one line of output, such as a status
// table cell: whitespace (including newlines) collapses to single spaces.
func CleanRemoteLine(s string) string {
	return capRunes(strings.Join(strings.Fields(CleanRemoteN(s, remoteLineMax*4)), " "), remoteLineMax)
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "...(truncated)"
}

// SanitizeWriter cleans what is written to it line by line before passing it
// on, for ipmitool output that goes straight to the terminal.
type SanitizeWriter struct {
	w   io.Writer
	buf []byte
}

// NewSanitizeWriter returns a SanitizeWriter that writes to w.
func NewSanitizeWriter(w io.Writer) *SanitizeWriter { return &SanitizeWriter{w: w} }

func (s *SanitizeWriter) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := s.buf[:i+1]
		if _, err := io.WriteString(s.w, CleanRemoteN(string(line), 0)); err != nil {
			return 0, err
		}
		s.buf = s.buf[i+1:]
	}
	if len(s.buf) > 64<<10 { // a runaway line with no newline
		if _, err := io.WriteString(s.w, CleanRemoteN(string(s.buf), 0)); err != nil {
			return 0, err
		}
		s.buf = nil
	}
	return len(p), nil
}

// Flush writes any unfinished last line.
func (s *SanitizeWriter) Flush() {
	if len(s.buf) > 0 {
		_, _ = io.WriteString(s.w, CleanRemoteN(string(s.buf), 0))
		s.buf = nil
	}
}

// Errors that say what happened in terms a person can act on.

// TimeoutError is a timeout talking to a BMC.
type TimeoutError struct {
	Host, Via string
	After     time.Duration
	Unknown   bool // a state-changing request may or may not have been applied
	Cause     error
}

// Error implements error.
func (e *TimeoutError) Error() string {
	s := fmt.Sprintf("timed out after %s contacting %s (%s)", e.After.Round(time.Second), e.Host, e.Via)
	if e.Unknown {
		s += "; " + ResultUnknown(e.Host)
	}
	return s
}

func (e *TimeoutError) Timeout() bool { return true }
func (e *TimeoutError) Unwrap() error { return e.Cause }

// Is lets errors.Is(err, context.DeadlineExceeded) keep working.
func (e *TimeoutError) Is(target error) bool { return target == context.DeadlineExceeded }

// ResultUnknown is the sentence for a state-changing action that did not
// answer.
func ResultUnknown(name string) string {
	if name == "" {
		name = "<name>"
	}
	return "result unknown; check `" + app.Name + " power " + name + "` before retrying"
}

// IsTimeout reports whether err is a deadline or network timeout.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// StripErrorPrefix removes any leading "Error:" labels so a caller can add its
// own without printing "ERROR: Error: ...".
func StripErrorPrefix(s string) string {
	s = strings.TrimSpace(s)
	for {
		l := strings.ToLower(s)
		switch {
		case strings.HasPrefix(l, "error:"):
			s = strings.TrimSpace(s[len("error:"):])
		default:
			return s
		}
	}
}

// CleanedError is an error whose text has been sanitized. The original stays
// reachable through Unwrap.
type CleanedError struct {
	Msg string
	Err error
}

func (e *CleanedError) Error() string { return e.Msg }
func (e *CleanedError) Unwrap() error { return e.Err }

// CleanErr sanitizes the text of err (an SSH disconnect reason, for one, is
// chosen by the remote) without hiding its type from errors.Is and errors.As.
func CleanErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if c := CleanRemote(msg); c != msg {
		return &CleanedError{Msg: c, Err: err}
	}
	return err
}
