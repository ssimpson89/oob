package console

import (
	"bytes"
	"errors"
)

// InputMapper is a link that has to change what is typed at it. The iLO VSP
// reads ESC as the first byte of a two-byte escape (ESC ESC passes both,
// ESC ( returns to the iLO CLI, ESC and anything else are both dropped), so a
// bare ESC and every escape sequence need rewriting before they are typed.
// MapInput returns the bytes to type and, when it dropped something the user
// should hear about, a note for the bar. The same note text means the same
// thing everywhere (console keys, share hub, MCP), so callers decide how
// often to show it. A link that needs no mapping doesn't implement it.
type InputMapper interface {
	MapInput(b []byte) (out []byte, note string)
}

// PasteStripper is a link whose input is mapped somewhere else, as a shared
// window's is (the hub's link maps it): it can't be an InputMapper without
// mapping twice, but a paste to it still has its escape sequences stripped.
type PasteStripper interface {
	StripsPaste() bool
}

// stripsPaste reports whether a paste to l has its escape sequences stripped.
func stripsPaste(l Link) bool {
	if _, ok := l.(InputMapper); ok {
		return true
	}
	p, ok := l.(PasteStripper)
	return ok && p.StripsPaste()
}

// VSPKeysNote is the bar note for keys the iLO VSP can't pass.
const VSPKeysNote = "iLO VSP can't pass arrow keys in full-screen programs: use h/j/k/l in vim"

// ErrKeysDropped is what an AI tool is told when what it sent can't go through
// an input-mapping link: it would have been dropped, so none of it is typed.
var ErrKeysDropped = errors.New("this console is an iLO VSP, which can't pass function keys or Alt-keys, or arrow keys in a full-screen program, so nothing was sent. " +
	"In vim use h/j/k/l, or ssh to the server")

// MapInput maps b for l: unchanged unless l is an InputMapper.
func MapInput(l Link, b []byte) (out []byte, note string) {
	if m, ok := l.(InputMapper); ok {
		return m.MapInput(b)
	}
	return b, ""
}

// MapVSPInput is the iLO VSP's mapping, for a link that is in the console:
// a lone ESC is typed as ESC ESC (the only way one gets through). Arrow, Home
// and End keys (CSI and SS3 forms) are typed as the readline controls ctrl+p,
// n, f, b, a and e unless fullScreen (the remote is on the alternate screen,
// where those mean nothing); every other CSI sequence (ESC [ ... final) and SS3
// sequence (ESC O x), and those keys in full screen, are dropped with
// VSPKeysNote, since the VSP would drop the ESC and the next byte anyway. ESC
// followed by a printable byte is a lone ESC and that key: a fast "<Esc>:wq"
// arrives in one read, and the VSP can't pass an Alt-key whichever it was. ESC
// ( is the exception, dropped, since it leaves for the iLO CLI. Focus reports
// (ESC [ I, ESC [ O) are dropped silently. An ESC at the end of b is lone: the
// key parser only hands one over by itself after the key had time to be a
// whole key. Two ESCs in a row are two lone ESCs.
func MapVSPInput(b []byte, fullScreen bool) (out []byte, note string) {
	if bytes.IndexByte(b, 0x1b) < 0 {
		return b, ""
	}
	out = make([]byte, 0, len(b)+2)
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c != 0x1b {
			out = append(out, c)
			continue
		}
		rest := b[i+1:]
		switch {
		case len(rest) == 0 || rest[0] == 0x1b || rest[0] == 0x03: // ctrl+c is a key of its own, never part of another
			out = append(out, 0x1b, 0x1b)
		case rest[0] == '[':
			n := csiLen(rest)
			if n == 2 && (rest[1] == 'I' || rest[1] == 'O') { // focus report
				i += n
				continue
			}
			if k := readlineKey(rest[:n]); k != 0 && !fullScreen {
				out = append(out, k)
			} else {
				note = VSPKeysNote
			}
			i += n
		case rest[0] == 'O':
			if len(rest) > 1 && !fullScreen && readlineKey(rest[:2]) != 0 {
				out = append(out, readlineKey(rest[:2]))
			} else {
				note = VSPKeysNote
			}
			if len(rest) > 1 && rest[1] != 0x03 {
				i += 2
			} else {
				i++ // a cut-off ESC O: ctrl+c after it is still typed
			}
		case rest[0] >= 0x20 && rest[0] < 0x7f && rest[0] != '(': // a lone ESC, then that key
			out = append(out, 0x1b, 0x1b)
		default: // ESC ( and other Alt-keys
			note = VSPKeysNote
			i++
		}
	}
	return out, note
}

// readlineKey is the readline control for an unmodified arrow, Home or End key
// given as seq (the bytes after ESC: "[A", "OH", "[1~"), or 0 if it is not one.
// Delete is not mapped: ctrl+d on an empty line ends the shell.
func readlineKey(seq []byte) byte {
	switch string(seq) {
	case "[A", "OA":
		return 0x10
	case "[B", "OB":
		return 0x0e
	case "[C", "OC":
		return 0x06
	case "[D", "OD":
		return 0x02
	case "[H", "OH", "[1~", "[7~":
		return 0x01
	case "[F", "OF", "[4~", "[8~":
		return 0x05
	}
	return 0
}

// AltScreenSetter is a link whose input mapping depends on whether the remote
// is in a full-screen program.
type AltScreenSetter interface {
	SetAltScreen(on bool)
}

// AltTracker follows remote output for the alternate screen (CSI ?1049, 1047
// or 47, h or l), across reads, and tells the link when it changes. One per
// output stream; it has no lock of its own.
type AltTracker struct {
	tail []byte
	on   bool
}

// Follow scans b, the remote's next output, and updates l if it is an
// AltScreenSetter and the state changed.
func (t *AltTracker) Follow(l Link, b []byte) {
	buf := b
	if len(t.tail) > 0 {
		buf = append(t.tail, b...)
	}
	_, _, alt, cut := scanSeqs(buf)
	t.tail = nil
	if cut >= 0 && len(buf)-cut <= tailMax {
		t.tail = bytes.Clone(buf[cut:])
	}
	if alt == 0 || (alt > 0) == t.on {
		return
	}
	t.on = alt > 0
	if s, ok := l.(AltScreenSetter); ok {
		s.SetAltScreen(t.on)
	}
}

// csiLen is how many bytes of rest (which starts with '[') belong to a CSI
// sequence: the '[', parameter and intermediate bytes, and the final byte. A
// sequence cut off by the end of rest takes all of it.
func csiLen(rest []byte) int {
	n := 1
	for n < len(rest) {
		c := rest[n]
		if c < 0x20 || c == 0x7f { // a control key (ctrl+c) isn't part of it, and must not be eaten
			break
		}
		n++
		if c >= 0x40 && c <= 0x7e {
			break
		}
	}
	return n
}

// fragMax bounds how much of an unfinished escape sequence an InputFilter holds.
const fragMax = 32

// InputFilter maps one input stream for a link that needs it (see
// InputMapper), holding back an escape sequence that a read ended in the
// middle of. A terminal can write an arrow key as ESC O and A, or ESC [ 1 ; 5
// and C, and the reads can fall between them: mapped one at a time, the first
// half is dropped as a cut-off sequence and the second half is typed as plain
// characters. Held until the rest arrives, the sequence is dropped whole. What
// is held is bounded (fragMax), a control key such as ctrl+c ends it, and
// Flush, which callers run after a short wait, gives up on it. A lone ESC at
// the end is a whole key (see MapVSPInput) and is never held. One filter per
// input stream; it has no lock of its own. A link that needs no mapping is
// passed through untouched.
type InputFilter struct {
	held []byte
}

// Pending reports whether the start of an escape sequence is being held.
func (f *InputFilter) Pending() bool { return len(f.held) > 0 }

// Map is MapInput for the stream's next bytes.
func (f *InputFilter) Map(l Link, b []byte) (out []byte, note string) {
	m, ok := l.(InputMapper)
	if !ok || !mappingActive(m) {
		// No mapping now (not a VSP, or the BMC's own prompt): nothing is held,
		// and anything held from before the link changed goes out with b.
		if held := f.held; len(held) > 0 {
			f.held = nil
			b = append(held, b...)
		}
		if !ok {
			return b, ""
		}
		return m.MapInput(b)
	}
	if len(f.held) == 0 && bytes.IndexByte(b, 0x1b) < 0 {
		return m.MapInput(b)
	}
	data := append(f.held, b...)
	f.held = nil
	if i := unfinishedEsc(data); i >= 0 {
		f.held = bytes.Clone(data[i:])
		data = data[:i]
	}
	return m.MapInput(data)
}

// mappingProbe is a key that a mapping link drops (an Alt-key) and any other
// link passes as it is.
var mappingProbe = []byte{0x1b, 'x'}

// mappingActive reports whether m changes input right now. A link can
// implement InputMapper and still pass everything through (the iLO CLI's own
// prompt, a plain ssh session), and then no sequence is worth holding.
func mappingActive(m InputMapper) bool {
	out, _ := m.MapInput(bytes.Clone(mappingProbe))
	return !bytes.Equal(out, mappingProbe)
}

// Flush gives up on a held sequence: with the mapping in force it is dropped
// whole, with the link's note for dropped keys; if the link has stopped
// mapping since, out is what to type. The stream goes on from the next byte.
func (f *InputFilter) Flush(l Link) (out []byte, note string) {
	data := f.held
	f.held = nil
	if len(data) == 0 {
		return nil, ""
	}
	return MapInput(l, data)
}

// unfinishedEsc is the index in b of an escape sequence that b ends in the
// middle of (ESC O with nothing after it, ESC [ with no final byte yet), or -1.
func unfinishedEsc(b []byte) int {
	i := bytes.LastIndexByte(b, 0x1b)
	if i < 0 || i+1 >= len(b) {
		return -1 // none, or a lone ESC
	}
	rest := b[i+1:]
	switch rest[0] {
	case 'O':
		if len(rest) == 1 {
			return i
		}
	case '[':
		if len(rest)+1 > fragMax {
			return -1
		}
		for _, c := range rest[1:] {
			if c < 0x20 || c > 0x7e || c >= 0x40 { // a control key, or the final byte
				return -1
			}
		}
		return i
	}
	return -1
}

// StripEsc removes escape sequences from pasted text: pastes have none, so
// anything there is noise, and on an iLO VSP a stray ESC would be read with
// the byte after it.
func StripEsc(s string) string {
	if !bytes.Contains([]byte(s), []byte{0x1b}) {
		return s
	}
	b := []byte(s)
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != 0x1b {
			out = append(out, b[i])
			continue
		}
		rest := b[i+1:]
		switch {
		case len(rest) == 0:
		case rest[0] == '[':
			i += csiLen(rest)
		case rest[0] == 'O':
			i += min(2, len(rest))
		default:
			i++
		}
	}
	return string(out)
}
