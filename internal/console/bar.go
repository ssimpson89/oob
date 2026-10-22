// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// seg, fitLine and the hint-dropping layouts follow bauddy's
// internal/tui/view.go, reworked to plain ANSI (no lipgloss) so the bar can be
// painted on the bottom row of the normal screen.

package console

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ssimpson89/oob/internal/app"
)

// seg is one run of text on the bar.
type seg struct {
	text string
	bold bool
}

func segWidth(ss []seg) int {
	n := 0
	for _, s := range ss {
		n += utf8.RuneCountInString(s.text)
	}
	return n
}

// layout is one way to arrange the bar.
type layout struct{ left, right []seg }

// Bar styles: reverse video follows the user's theme; the blue one marks
// command mode so it is obvious that typing isn't going to the remote.
const (
	styleNormal = "\x1b[0;7m"
	styleAsk    = "\x1b[0;44;97m"
	styleReset  = "\x1b[0m"
)

// cutRunes cuts s to at most w cells (runes; the bar text is ASCII plus a few
// symbols, so a rune is a cell).
func cutRunes(s string, w int) string {
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	r := []rune(s)
	return string(r[:max(0, w)])
}

// line lays out left and right on one row of exactly w cells, in style.
func line(style string, w int, left, right []seg) string {
	gap := max(1, w-segWidth(left)-segWidth(right))
	var sb strings.Builder
	used := 0
	emit := func(ss []seg) {
		for _, s := range ss {
			t := cutRunes(s.text, w-used)
			if t == "" {
				continue
			}
			if s.bold {
				sb.WriteString("\x1b[1m" + t + "\x1b[22m")
			} else {
				sb.WriteString(t)
			}
			used += utf8.RuneCountInString(t)
		}
	}
	emit(left)
	emit([]seg{{text: strings.Repeat(" ", gap)}})
	emit(right)
	if used < w {
		sb.WriteString(strings.Repeat(" ", w-used))
	}
	return style + sb.String() + styleReset
}

// fitLine renders the first layout that fits in w, or the last one, cut to w.
func fitLine(style string, w int, layouts []layout) string {
	for _, l := range layouts {
		if segWidth(l.left)+segWidth(l.right)+1 <= w {
			return line(style, w, l.left, l.right)
		}
	}
	last := layouts[len(layouts)-1]
	return line(style, w, last.left, last.right)
}

// hint is a key and what it does: "s size  ".
func hint(key, label string) []seg {
	return []seg{{key, true}, {" " + label + "  ", false}}
}

// barState is everything the bar shows.
type barState struct {
	host    string
	kind    string // "sol" or "ssh"
	mode    keyMode
	logging bool
	auto    bool
	// atPrompt: the session is at the BMC's CLI prompt, so c goes to the console.
	atPrompt bool
	note     string
	noteErr  bool
	// sending is "1.2k/4k" while a big queue is being typed, else "".
	sending string
	// shared is the console's other users, as "shared · AI connected"; "" when
	// the console is shared with nobody, or not shared.
	shared string
	// sharing: the console is shared, so the i key does something.
	sharing bool
	// aiPaused: the i key resumes the AI tool.
	aiPaused bool
}

// commandLayouts builds the command-mode bar: all hints first, then one fewer
// at a time, lowest keep first (ties drop the later one), so a narrow window
// keeps the keys that matter. The ? hint reads "more" once any is hidden.
func commandLayouts(st barState) []layout {
	keys := slices.Clone(commandKeys)
	for i := range keys {
		switch {
		case keys[i].act == actLog && st.logging:
			keys[i].short = "stop log"
		case keys[i].act == actBMC && st.atPrompt:
			keys[i].short = "console"
		case keys[i].act == actAuto && st.auto:
			keys[i].short = "auto-size off"
		case keys[i].act == actStop && st.sending == "":
			keys[i].short = ""
		case keys[i].act == actAI && !st.sharing:
			keys[i].short = ""
		case keys[i].act == actAI && st.aiPaused:
			keys[i].short = "resume AI"
		}
	}
	dropped := 0
	var layouts []layout
	right := []seg{{"esc", true}, {" back", false}}
	for {
		var hints []seg
		for _, b := range keys {
			if b.short == "" {
				continue
			}
			label := b.short
			if b.act == actHelp && dropped > 0 {
				label = "more"
			}
			hints = append(hints, hint(b.key, label)...)
		}
		lead := []seg{{" " + shortKey + " ", true}}
		layouts = append(layouts, layout{slices.Concat(lead, hints), right})
		// drop the lowest-keep hint still present
		idx := -1
		for i, b := range keys {
			if b.short == "" || b.keep >= keepAlways {
				continue
			}
			if idx < 0 || b.keep <= keys[idx].keep {
				idx = i
			}
		}
		if idx < 0 {
			break
		}
		keys[idx].short = ""
		dropped++
	}
	layouts = append(layouts, layout{[]seg{{" ^] ", true}, {"? more", false}}, nil})
	return layouts
}

// barLine renders the bar for st, w cells wide, escapes included.
func barLine(st barState, w int) string {
	switch st.mode {
	case modeCommand:
		return fitLine(styleAsk, w, commandLayouts(st))
	case modeSysrq:
		lead := []seg{{" BREAK, then which key?  ", true}}
		right := []seg{{"esc", true}, {" cancel", false}}
		keys := [][]seg{hint("b", "reboot"), hint("s", "sync"), hint("u", "remount ro"), hint("o", "poweroff"), hint("c", "crash")}
		var layouts []layout
		for n := len(keys); n >= 0; n-- {
			layouts = append(layouts, layout{slices.Concat(append([][]seg{lead}, keys[:n]...)...), right})
		}
		return fitLine(styleAsk, w, layouts)
	case modeConfirm:
		lead := []seg{{" Power cycle " + st.host + "? ", true}}
		right := []seg{{"esc", true}, {" cancel", false}}
		return fitLine(styleAsk, w, []layout{
			{slices.Concat(lead, hint("y", "yes"), hint("n", "no")), right},
			{slices.Concat([]seg{{" Power cycle? ", true}}, hint("y", "yes")), right},
			{[]seg{{" Power cycle? y/n", true}}, nil},
		})
	}
	name := []seg{{" " + app.Name + " " + st.host + " " + st.kind, true}}
	var log []seg
	if st.logging {
		log = []seg{{"  [log]", false}}
	}
	if st.shared != "" {
		log = append(log, seg{"  [" + st.shared + "]", false})
	}
	var note, noteShort []seg
	switch {
	case st.sending != "":
		progress := "  sending " + st.sending
		note = []seg{{progress, true}, {" (" + shortKey + " x clears)", false}}
		noteShort = []seg{{progress, true}}
	case st.note != "":
		note = []seg{{"  " + st.note, st.noteErr}}
		noteShort = note
	}
	keys := []seg{{shortKey, true}, {" keys ", false}}
	quit := slices.Concat([]seg{{"~.", true}, {" quit  ", false}}, keys)
	layouts := []layout{
		{slices.Concat(name, log, note), quit},
		{slices.Concat(name, log, noteShort), quit},
		{slices.Concat(name, log, note), keys},
		{slices.Concat(name, log, noteShort), keys},
	}
	if st.sending != "" { // the progress outranks the rest of the bar
		layouts = append(layouts,
			layout{slices.Concat([]seg{{" " + st.host, true}}, noteShort), keys},
			layout{noteShort, keys},
		)
	}
	layouts = append(layouts,
		layout{slices.Concat(name, log), quit},
		layout{slices.Concat(name, log), keys},
		layout{slices.Concat([]seg{{" " + st.host, true}}, log, noteShort), keys},
		layout{slices.Concat([]seg{{" " + st.host, true}}, log), keys},
		layout{slices.Concat([]seg{{" " + st.host, true}}), keys},
		layout{keys, nil},
	)
	return fitLine(styleNormal, w, layouts)
}

// escTracker follows the escape sequences in a stream of remote output across
// chunk boundaries, so the bar is never painted into the middle of one (which
// would garble the screen). It also catches a UTF-8 character cut short.
//
// A sequence that never ends (noise, a lost terminator) must not freeze the
// bar: CAN and SUB abort it, it is treated as ended after seqMax bytes, and the
// console resets the tracker after the remote has been quiet for a while.
type escTracker struct {
	state int
	n     int // bytes consumed by the sequence in progress
	utf   int // continuation bytes still owed to a multibyte character
}

// Tracker states. The trk prefix keeps them apart from termCleaner's s* states
// in termtext.go, which are a different machine.
const (
	trkText = iota
	trkEsc
	trkCSI
	trkStr
	trkStrEsc
)

// seqMax caps a CSI or string sequence: real ones are far shorter, and a
// sequence with no terminator would otherwise hold the bar off forever.
const seqMax = 4 << 10

// Reset forgets any sequence or character in progress.
func (t *escTracker) Reset() { *t = escTracker{} }

// Feed consumes a chunk of output.
func (t *escTracker) Feed(b []byte) {
	for _, c := range b {
		if t.state != trkText {
			if c == 0x18 || c == 0x1a { // CAN, SUB abort a sequence
				t.state, t.n = trkText, 0
				continue
			}
			if t.n++; t.n > seqMax {
				t.state, t.n = trkText, 0
				continue
			}
		}
		switch t.state {
		case trkEsc:
			switch {
			case c == 0x1b: // ESC ESC: the second one starts the sequence
				t.n = 0
			case c == '[':
				t.state = trkCSI
			case c == ']' || c == 'P' || c == '_' || c == '^' || c == 'X':
				t.state = trkStr
			case c >= 0x20 && c <= 0x2f: // intermediate: stay until the final byte
			default:
				t.state = trkText
			}
		case trkCSI:
			if c >= 0x40 && c <= 0x7e {
				t.state = trkText
			}
		case trkStr:
			switch c {
			case 0x07:
				t.state = trkText
			case 0x1b:
				t.state = trkStrEsc
			}
		case trkStrEsc:
			t.state = trkText
		default:
			switch {
			case t.utf > 0 && c&0xC0 == 0x80:
				t.utf--
			case c == 0x1b:
				t.state, t.n, t.utf = trkEsc, 0, 0
			case c >= 0xF0:
				t.utf = 3
			case c >= 0xE0:
				t.utf = 2
			case c >= 0xC0:
				t.utf = 1
			default:
				t.utf = 0
			}
		}
	}
}

// Idle reports whether the stream is between sequences and characters.
func (t *escTracker) Idle() bool { return t.state == trkText && t.utf == 0 }

// clobbers reports whether b holds something that resets the scroll region or
// wipes the bar row: DECSTBM (CSI r), full reset (ESC c), soft reset
// (CSI ! p), clear screen (CSI 2J or 3J), or switching the alternate screen
// (CSI ? 47, 1047 or 1049 h or l). Full-screen programs do these.
func clobbers(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != 0x1b || i+1 >= len(b) {
			continue
		}
		if b[i+1] == 'c' {
			return true
		}
		if b[i+1] != '[' {
			continue
		}
		j := i + 2
		for j < len(b) && !(b[j] >= 0x40 && b[j] <= 0x7e) {
			j++
		}
		if j >= len(b) {
			break
		}
		params := string(b[i+2 : j])
		switch b[j] {
		case 'r':
			if !strings.HasPrefix(params, "?") {
				return true
			}
		case 'p':
			if params == "!" {
				return true
			}
		case 'J':
			if params == "2" || params == "3" {
				return true
			}
		case 'h', 'l':
			if rest, ok := strings.CutPrefix(params, "?"); ok {
				for _, p := range strings.Split(rest, ";") {
					if n, err := strconv.Atoi(p); err == nil && (n == 47 || n == 1047 || n == 1049) {
						return true
					}
				}
			}
		}
		i = j
	}
	return false
}
