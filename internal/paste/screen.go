package paste

import (
	"bytes"
	"regexp"
)

// altScreen matches the private modes that switch to the alternate screen:
// 1049, 1047 and 47, set (h) or reset (l).
var altScreen = regexp.MustCompile(`\x1b\[\?([0-9;]*)([hl])`)

// addressing matches a cursor move to a row and column, or to home, or a
// screen clear: what a full-screen program draws with, and a shell prompt
// never does.
var addressing = regexp.MustCompile(`\x1b\[(?:[0-9]*;[0-9]*[Hf]|[Hf]|2J)`)

const tailKeep = 16 // bytes kept in case an escape sequence is split between reads

func altModeIn(params string) bool {
	for _, p := range bytes.Split([]byte(params), []byte(";")) {
		switch string(p) {
		case "1049", "1047", "47":
			return true
		}
	}
	return false
}

// sawScreenProgram reports whether b, with the end of the earlier output,
// shows a full-screen program: the alternate screen switched on, or cursor
// addressing.
func (r *run) sawScreenProgram(b []byte) bool {
	buf := append(r.tail, b...)
	r.tail = append(r.tail[:0], buf[max(0, len(buf)-tailKeep):]...)
	if !bytes.Contains(buf, []byte("\x1b[")) {
		return false
	}
	for _, m := range altScreen.FindAllSubmatch(buf, -1) {
		if string(m[2]) == "h" && altModeIn(string(m[1])) {
			return true
		}
	}
	return addressing.Match(buf)
}

// screenTracker follows whether the alternate screen is on, across reads.
type screenTracker struct {
	on   bool
	tail []byte
}

func (s *screenTracker) feed(b []byte) {
	buf := append(s.tail, b...)
	s.tail = append(s.tail[:0], buf[max(0, len(buf)-tailKeep):]...)
	if !bytes.Contains(buf, []byte("\x1b[?")) {
		return
	}
	for _, m := range altScreen.FindAllSubmatch(buf, -1) {
		if altModeIn(string(m[1])) {
			s.on = string(m[2]) == "h"
		}
	}
}
