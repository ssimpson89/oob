package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCleanRemote(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"plain", "hello world\tx\nnext", "hello world\tx\nnext"},
		{"CSI", "a\x1b[31mred\x1b[0m b\x1b[2J\x1b[1;1Hc", "ared bc"},
		{"OSC BEL", "x\x1b]0;evil title\x07y", "xy"},
		{"OSC ST", "x\x1b]52;c;ZXZpbA==\x1b\\y", "xy"},
		{"DCS", "x\x1bP1$r0m\x1b\\y", "xy"},
		{"APC and PM", "a\x1b_secret\x1b\\b\x1b^pm\x1b\\c", "abc"},
		{"unterminated OSC eats the rest", "ok\x1b]0;never ends", "ok"},
		{"charset", "a\x1b(Bb", "ab"},
		{"two byte", "a\x1b7b\x1b=c", "abc"},
		{"C0 and DEL", "a\x00b\x07c\x08d\x7fe\x0bf", "abcdef"},
		{"CR", "one\r\ntwo\rthree", "one\ntwo\nthree"},
		{"C1 as runes", "a\u0080b\u009bc\u009d0;x\u009cd", "abc0;xd"},
		{"bidi", "a‮b‏c", "abc"},
		{"bad utf8", "a\xffb", "a�b"},
		{"lone ESC at end", "a\x1b", "a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanRemote(c.in); got != c.want {
				t.Errorf("cleanRemote(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	if got := cleanRemote(strings.Repeat("a", remoteMax+50)); len(got) > remoteMax+20 || !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("not capped: %d", len(got))
	}
	if got := cleanRemoteLine("a\n\n  b\t\x1b[1mc\x1b[0m"); got != "a b c" {
		t.Errorf("line: %q", got)
	}
}

func TestSanitizeWriter(t *testing.T) {
	var buf bytes.Buffer
	w := &sanitizeWriter{w: &buf}
	w.Write([]byte("fine line\nbad \x1b]0;t"))
	w.Write([]byte("itle\x07line\n\x1b[2Jtail"))
	w.Flush()
	if got := buf.String(); got != "fine line\nbad line\ntail" {
		t.Errorf("got %q", got)
	}
}

func TestStatusFailAndTimeout(t *testing.T) {
	if r := statusFail("Error: Unable to establish\x1b[1m"); !r.failed || r.text != "ERROR: Unable to establish" {
		t.Errorf("%+v", r)
	}
	if r := statusFail("ERROR: Error: x"); r.text != "ERROR: x" {
		t.Errorf("%+v", r)
	}
	te := &timeoutErr{host: "node01", via: "Redfish", after: 15 * time.Second}
	if te.Error() != "timed out after 15s contacting node01 (Redfish)" || !errors.Is(te, context.DeadlineExceeded) {
		t.Errorf("%v", te)
	}
	te.unknown = true
	if !strings.Contains(te.Error(), "result unknown; check `oob power node01` before retrying") {
		t.Errorf("%v", te)
	}
	if ce := cleanErr(errors.New("disconnect: \x1b]0;x\x07hi")); ce.Error() != "disconnect: hi" {
		t.Errorf("%q", ce)
	}
	base := &timeoutErr{host: "h"}
	var got *timeoutErr
	if !errors.As(cleanErr(base), &got) {
		t.Error("cleanErr hid the type")
	}
}
