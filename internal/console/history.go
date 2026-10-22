// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// The 10000-line history follows bauddy's internal/tui/log.go: output is kept
// as plain lines so a log started late still includes what came before.

package console

import "github.com/ssimpson89/oob/internal/termtext"

// historyLinesMax is how many completed lines of output are kept.
const historyLinesMax = 10000

// history keeps the newest completed lines of console output as plain text
// (escape sequences removed), in a ring so memory stays bounded.
type history struct {
	cl    termtext.Cleaner
	buf   []string // ring storage, grows to max then wraps
	max   int
	start int // index of the oldest line once the ring is full
	n     int // lines completed since the start, including ones the ring dropped
}

func newHistory(max int) *history {
	if max < 1 {
		max = 1
	}
	return &history{max: max}
}

// Feed adds raw console output.
func (h *history) Feed(raw []byte) {
	out := h.cl.Feed(raw)
	for len(out) > 0 {
		i := 0
		for i < len(out) && out[i] != '\n' {
			i++
		}
		h.push(string(out[:i]))
		if i < len(out) {
			i++
		}
		out = out[i:]
	}
}

func (h *history) push(line string) {
	h.n++
	if len(h.buf) < h.max {
		h.buf = append(h.buf, line)
		return
	}
	h.buf[h.start] = line
	h.start = (h.start + 1) % h.max
}

// Len is the number of completed lines held.
func (h *history) Len() int { return len(h.buf) }

// Lines returns the held lines, oldest first.
func (h *history) Lines() []string {
	out := make([]string, 0, len(h.buf))
	out = append(out, h.buf[h.start:]...)
	return append(out, h.buf[:h.start]...)
}

// Partial is the unfinished current line, usually a prompt.
func (h *history) Partial() string { return h.cl.Partial() }

// Count is how many lines have been completed since the start, including ones
// the ring has dropped.
func (h *history) Count() int { return h.n }

// Last returns the newest k held lines, oldest first.
func (h *history) Last(k int) []string {
	k = max(0, min(k, len(h.buf)))
	out := make([]string, 0, k)
	for i := len(h.buf) - k; i < len(h.buf); i++ {
		out = append(out, h.buf[(h.start+i)%len(h.buf)])
	}
	return out
}
