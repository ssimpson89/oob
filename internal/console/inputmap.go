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
		case len(rest) == 0 || rest[0] == 0x1b:
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
			i += min(2, len(rest))
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
		n++
		if c >= 0x40 && c <= 0x7e {
			break
		}
	}
	return n
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
