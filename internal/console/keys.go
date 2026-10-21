// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// The command-mode idea (an escape key, then one key; one table that drives
// both the hints and the help) is adapted from bauddy's internal/keymap.

package console

import (
	"fmt"
	"strings"
	"time"
)

// escapeByte is ctrl+], the telnet tradition, which devices almost never need.
const escapeByte = 0x1d

// act is something the user asked the console to do.
type act int

const (
	actNone act = iota
	actSize
	actAuto
	actLog
	actBreak
	actBMC
	actSysrq // uses key.arg
	actLiteral
	actPower
	actDisconnect
	actHelp
	actCancel      // left command mode with nothing done (unknown key, n)
	actRaw         // toggle raw (unpaced) paste
	actStop        // stop a paste in progress (x, or esc in command mode)
	actPaste       // bracketed paste finished: action.text is the pasted text
	actPasteTooBig // a paste over pasteMax: none of it is sent
)

// action is a parsed key: what to do, plus the SysRq letter.
type action struct {
	act  act
	arg  byte
	text string // actPaste: what was pasted
	at   int    // actPaste: how many of the forwarded bytes came before it
}

// keyMode is where the key parser is.
type keyMode int

const (
	modeConsole keyMode = iota // typing goes to the remote
	modeCommand                // ctrl+] pressed, waiting for a command key
	modeSysrq                  // r pressed, waiting for the SysRq letter
	modeConfirm                // p pressed, waiting for y or n
)

// binding is one command-mode key, with text for the hints and for help.
type binding struct {
	key   string
	act   act
	short string // hint label on the bar; empty for keys only listed in help
	help  string
	keep  int // on a narrow bar, higher stays longer
	sends bool
}

const keepAlways = 100

// commandKeys is the command-mode keymap, in bar order.
var commandKeys = []binding{
	{"s", actSize, "size", "Send the window size to the remote (stty rows/cols over SOL, window change over ssh)", 9, true},
	{"a", actAuto, "auto-size", "Toggle sending the size whenever the window is resized (off by default for SOL)", 4, false},
	{"l", actLog, "log", "Start or stop logging to a file (includes earlier output)", 8, false},
	{"b", actBreak, "break", "Send BREAK", 7, true},
	{"c", actBMC, "bmc prompt", "Toggle between the host console and the BMC's own CLI prompt (iLO, iDRAC, Supermicro SSH SOL; not IPMI SOL)", 3, true},
	{"r", actSysrq, "r+key", "SysRq: BREAK, then the next key (r then b reboots a Linux console)", 6, true},
	{"w", actRaw, "raw paste", "Toggle raw paste: send a paste all at once instead of paced and echo-checked (serial consoles; paced is the default)", 1, false},
	{"x", actStop, "stop paste", "Stop a paste in progress (esc does the same)", 1, false},
	{"p", actPower, "power cycle", "Power cycle the machine (asks y/n; ipmitool, or off then on over a second SSH connection on an iLO)", 5, true},
	{"]", actLiteral, "send ^]", "Send ctrl+] itself", 2, true},
	{".", actDisconnect, "disconnect", "Disconnect", keepAlways, false},
	{"?", actHelp, "help", "Show every key", keepAlways, false},
}

// shortKey abbreviates a key for narrow windows: "ctrl+]" -> "^]".
func shortKey(k string) string {
	if rest, ok := strings.CutPrefix(k, "ctrl+"); ok {
		return "^" + rest
	}
	return k
}

// helpText is the full key list, printed by the ? key.
func helpText() string {
	var sb strings.Builder
	sb.WriteString("Everything you type goes to the remote. Press ctrl+], then one key:\n")
	for _, b := range commandKeys {
		fmt.Fprintf(&sb, "  %-4s %s\n", b.key, b.help)
	}
	sb.WriteString("  esc  Back to the console\n")
	sb.WriteString("Also, at the start of a line: ~. disconnects, ~~ sends one ~. (Inside an outer ssh session, type ~~. so the outer ssh passes one ~ through.)\n")
	return sb.String()
}

// Bracketed paste markers, as a terminal sends them around pasted text.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// pasteMax caps the size of one paste. A larger one is rejected whole, never
// sent in part: running a prefix of a script is worse than running none of it.
const pasteMax = 4 << 20

// escWait is how long a held ESC waits for the rest of its sequence before it
// is taken as a bare key: short enough to feel instant, long enough for a
// terminal's write to arrive in a second read.
const escWait = 50 * time.Millisecond

// keyParser turns raw stdin bytes into bytes for the remote and actions. It is
// pure: no I/O, so it is easy to test. A trailing ESC that might begin a paste
// marker or an arrow key is held until the next Feed or Flush.
type keyParser struct {
	mode    keyMode
	started bool // false until the first byte: the session start counts as a line start
	midLine bool // the last byte forwarded was not CR or LF
	tilde   bool // a ~ at line start is held back until the next byte
	paste   bool // between the paste markers: bytes are collected, not typed
	pasted  []byte
	over    bool // the paste passed pasteMax: the rest is discarded and none of it is sent
	held    []byte
	skip    int // command mode is skipping an escape sequence: 1 CSI, 2 SS3
}

// Mode reports where the parser is, for the bar.
func (p *keyParser) Mode() keyMode { return p.mode }

// Pending reports that bytes are held waiting for Flush or more input.
func (p *keyParser) Pending() bool { return len(p.held) > 0 }

// Flush resolves held bytes as ordinary input, once no more have come in
// escWait: a lone ESC is a bare esc key.
func (p *keyParser) Flush() (fwd []byte, acts []action) {
	if len(p.held) == 0 {
		return nil, nil
	}
	b := p.held
	p.held = nil
	return p.feed(b, true)
}

func isSysrqKey(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// markerAt reports the paste marker at the start of b: its length, or 0, and
// whether b is only the start of one (more bytes needed).
func markerAt(b []byte) (n int, partial bool) {
	for _, m := range [...]string{pasteStart, pasteEnd} {
		if len(b) >= len(m) && string(b[:len(m)]) == m {
			return len(m), false
		}
		if len(b) < len(m) && string(b) == m[:len(b)] {
			partial = true
		}
	}
	return 0, partial
}

// Feed consumes b. Bytes in console mode are returned for the remote; command
// keys become actions. An escape sequence (arrow key and the like) pressed in
// command mode is swallowed, so it doesn't count as a bare esc. Paste markers
// are always removed; inside a paste no key handling happens.
func (p *keyParser) Feed(b []byte) (fwd []byte, acts []action) {
	if len(p.held) > 0 {
		b = append(p.held, b...)
		p.held = nil
	}
	return p.feed(b, false)
}

func (p *keyParser) feed(b []byte, final bool) (fwd []byte, acts []action) {
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == 0x1b {
			n, partial := markerAt(b[i:])
			if partial && !final {
				p.held = append([]byte(nil), b[i:]...)
				break
			}
			if n > 0 {
				if string(b[i:i+n]) == pasteStart {
					p.paste, p.pasted, p.over = true, nil, false
					p.skip = 0
					if p.tilde { // the held ~ was typed before the paste
						p.tilde = false
						fwd = append(fwd, '~')
						p.midLine = true
					}
					if p.mode != modeConsole {
						p.mode = modeConsole
						acts = append(acts, action{act: actCancel})
					}
				} else {
					switch {
					case p.paste && p.over:
						acts = append(acts, action{act: actPasteTooBig, at: len(fwd)})
					case p.paste && len(p.pasted) > 0:
						acts = append(acts, action{act: actPaste, text: string(p.pasted), at: len(fwd)})
					}
					p.paste, p.pasted, p.over = false, nil, false
				}
				i += n - 1
				continue
			}
		}
		if p.paste {
			p.midLine = c != '\r' && c != '\n'
			p.started = true
			switch {
			case p.over:
			case len(p.pasted) >= pasteMax: // too big: drop it all, and the rest up to the end marker
				p.over, p.pasted = true, nil
			default:
				p.pasted = append(p.pasted, c)
			}
			continue
		}
		if p.mode == modeConsole {
			if !p.started {
				p.started = true
			}
			if p.tilde {
				// ssh-style escapes: ~. disconnects, ~~ sends one ~, ~x sends both.
				p.tilde = false
				switch c {
				case '.':
					acts = append(acts, action{act: actDisconnect})
					continue
				case '~':
					fwd = append(fwd, '~')
					p.midLine = true
					continue
				}
				fwd = append(fwd, '~')
				p.midLine = true
			}
			switch {
			case c == escapeByte:
				p.mode = modeCommand
			case c == '~' && !p.midLine:
				p.tilde = true
			default:
				fwd = append(fwd, c)
				p.midLine = c != '\r' && c != '\n'
			}
			continue
		}
		if p.skip != 0 && c != 0x1b {
			// inside an arrow key or the like that began in an earlier read
			if p.skip == 2 || (c >= 0x40 && c <= 0x7e) {
				p.skip = 0
			}
			continue
		}
		p.skip = 0
		if c == 0x1b {
			if i+1 < len(b) && (b[i+1] == '[' || b[i+1] == 'O') {
				p.skip = 1
				if b[i+1] == 'O' {
					p.skip = 2
				}
				i++ // CSI or SS3: skip to its final byte
				continue
			}
			prev := p.mode
			p.mode = modeConsole
			if prev == modeCommand {
				acts = append(acts, action{act: actStop}) // esc: back to the console, and stops a paste
			} else {
				acts = append(acts, action{act: actCancel})
			}
			continue
		}
		switch p.mode {
		case modeCommand:
			acts = append(acts, p.command(c)...)
		case modeSysrq:
			p.mode = modeConsole
			if isSysrqKey(c) {
				acts = append(acts, action{act: actSysrq, arg: c})
			} else {
				acts = append(acts, action{act: actCancel})
			}
		case modeConfirm:
			p.mode = modeConsole
			if c == 'y' || c == 'Y' {
				acts = append(acts, action{act: actPower})
			} else {
				acts = append(acts, action{act: actCancel})
			}
		}
	}
	return fwd, acts
}

// command handles the key after ctrl+].
func (p *keyParser) command(c byte) []action {
	p.mode = modeConsole
	switch c {
	case 's':
		return []action{{act: actSize}}
	case 'a':
		return []action{{act: actAuto}}
	case 'l':
		return []action{{act: actLog}}
	case 'b':
		return []action{{act: actBreak}}
	case 'c':
		return []action{{act: actBMC}}
	case 'r':
		p.mode = modeSysrq
		return nil
	case ']', escapeByte: // ctrl+] twice also sends it, as in telnet
		return []action{{act: actLiteral}}
	case 'w':
		return []action{{act: actRaw}}
	case 'x':
		return []action{{act: actStop}}
	case 'p':
		p.mode = modeConfirm
		return nil
	case '.':
		return []action{{act: actDisconnect}}
	case '?':
		return []action{{act: actHelp}}
	}
	return []action{{act: actCancel}}
}
