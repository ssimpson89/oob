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

// VSPKeysNote is the bar note for keys the iLO VSP can't pass.
const VSPKeysNote = "iLO VSP can't pass arrow/function keys: shell ctrl+p/n/b/f, vim h/j/k/l"

// ErrKeysDropped is what an AI tool is told when what it sent can't go through
// an input-mapping link: it would have been dropped, so none of it is typed.
var ErrKeysDropped = errors.New("this console is an iLO VSP, which can't pass arrow keys, function keys or Alt-keys, so nothing was sent. " +
	"Use shell ctrl+p/n/b/f or vim h/j/k/l instead, or ssh to the server")

// MapInput maps b for l: unchanged unless l is an InputMapper.
func MapInput(l Link, b []byte) (out []byte, note string) {
	if m, ok := l.(InputMapper); ok {
		return m.MapInput(b)
	}
	return b, ""
}

// MapVSPInput is the iLO VSP's mapping, for a link that is in the console:
// a lone ESC is typed as ESC ESC (the only way one gets through); a CSI
// sequence (ESC [ ... final), an SS3 sequence (ESC O x) and an Alt-key
// (ESC x) are dropped with VSPKeysNote, since the VSP would drop the ESC and
// the next byte anyway, or leave for the iLO CLI on ESC (. Focus reports
// (ESC [ I, ESC [ O) are dropped silently. An ESC at the end of b is lone: the
// key parser only hands one over by itself after the key had time to be a
// whole key. Two ESCs in a row are two lone ESCs.
func MapVSPInput(b []byte) (out []byte, note string) {
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
			note = VSPKeysNote
			i += n
		case rest[0] == 'O':
			note = VSPKeysNote
			if len(rest) > 1 && rest[1] != 0x03 {
				i += 2
			} else {
				i++ // a cut-off ESC O: ctrl+c after it is still typed
			}
		default: // Alt-key
			note = VSPKeysNote
			i++
		}
	}
	return out, note
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
