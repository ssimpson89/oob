// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// Adapted from bauddy (internal/mcpserver/console.go): the console as plain
// text with a read position (what is new since the model last looked), the
// cursor-line and prompt tracking the guard depends on, and the shaping of
// output into bounded results.

package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ssimpson89/oob/internal/bmc"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/share"
)

const (
	// What one result shows at most, keeping the newest. The model reads every
	// byte, and a boot log can run to thousands of lines.
	shownLinesMax = 300
	shownBytesMax = 22 << 10
	lineMax       = 2 << 10 // a longer line keeps its end
	// ptyRows and ptyCols are the window size the remote is told. Wide, so
	// long lines don't wrap and fool the prompt tracking.
	ptyRows, ptyCols = 50, 220
	// afterBreak is how long the next key counts as a possible SysRq command.
	afterBreak = 10 * time.Second
)

// quietFor is how long the console must be silent before its answer counts as
// finished. A variable so tests can shorten it.
var quietFor = 400 * time.Millisecond

// session is one open server console.
type session struct {
	id   string
	host string
	addr string // the BMC address and type the console was opened on
	typ  string
	tr   *bmc.ConsoleTransport
	// sh is the shared console the session types through: the hub owns the one
	// typer and decides who may type.
	sh *share.Client
	tx *console.Transcript

	// sendMu lets one tool at a time type, including while it waits for the
	// answer. Reads don't take it.
	sendMu sync.Mutex

	mu        sync.Mutex
	kind      string // "sol", "vsp", "com2", or the BMC prompt's label
	atBMC     bool   // at the BMC's own command line, not the host console
	seen      int    // lines returned so far
	seenAt    string // the cursor line then
	cursorWas string
	prompt    string // the last cursor line that looked like a prompt, with nothing typed
	prompts   int    // how many times a prompt has appeared
	answersAt int    // the prompt count when a command was last approved (-1: none)
	typing    typing // what send_keys has typed since the last Enter
	lastEsc   bool   // the last byte sent was ESC
	lastUsed  time.Time
	ended     bool // nothing more can be typed
	marked    bool // the end is in the transcript, with its exit status
	endMsg    string
	changed   chan struct{} // closed and replaced whenever something arrives
	logPath   string
	closed    bool

	doneC   chan struct{} // closed once the transport has ended
	eofC    chan struct{} // closed once the output stream hit EOF: the console will say no more
	result  console.Result
	drained chan struct{} // closed once the output reader has finished
	once    sync.Once
	trOnce  sync.Once // the transport is torn down once
}

// newSession starts recording tr's output.
func newSession(id, host string, tr *bmc.ConsoleTransport, tx *console.Transcript, sh *share.Client) *session {
	c := &session{
		id: id, host: host, tr: tr, tx: tx, sh: sh, kind: tr.Kind, answersAt: -1,
		lastUsed: time.Now(), changed: make(chan struct{}),
		doneC: make(chan struct{}), eofC: make(chan struct{}), drained: make(chan struct{}),
	}
	go func() {
		r := <-tr.Done
		c.mu.Lock()
		c.result = r
		c.mu.Unlock()
		close(c.doneC)
	}()
	go c.drain()
	return c
}

// drain records the console's output until it ends.
func (c *session) drain() {
	defer close(c.drained)
	buf := make([]byte, 32<<10)
	for {
		n, err := c.tr.Out.Read(buf)
		if n > 0 {
			c.feed(buf[:n])
		}
		if err != nil {
			// Nothing more will arrive, so nothing typed now can be checked
			// against what the console shows: a stale prompt would wave the
			// rest of a batch through. Refuse new sends before anyone waiting
			// on eofC can act on the hang-up.
			c.endInput()
			close(c.eofC)
			select { // the exit status is worth a moment, collected apart from that
			case <-c.doneC:
			case <-time.After(2 * time.Second):
			}
			c.markEnded()
			return
		}
	}
}

// feed records console output and follows the cursor.
func (c *session) feed(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := c.tx.Count()
	c.tx.Feed(b)
	completed := c.tx.Count() - before
	if c.tr.Observe != nil {
		kind, at, _ := c.tr.Observe(b)
		c.kind, c.atBMC = kind, at
	}
	// A chunk can carry the echo, the output and the next prompt together, so a
	// prompt identical to the last one looks like no change. A line completed
	// within the chunk and a prompt after it is a prompt appearing, however the
	// output was cut into chunks.
	if cursor := c.tx.Partial(); cursor != c.cursorWas || (completed > 0 && looksLikePrompt(cursor)) {
		c.cursorWas = cursor
		// Typed text ending in # (reboot #) extends the prompt; it isn't a new one.
		extends := completed == 0 && c.prompt != "" && len(cursor) > len(c.prompt) && strings.HasPrefix(cursor, c.prompt)
		if looksLikePrompt(cursor) && !extends {
			c.prompt = cursor
			c.prompts++
		}
	}
	c.notifyLocked()
}

// endInput marks the session ended, so canSend refuses. The message is
// provisional until markEnded has the exit status.
func (c *session) endInput() {
	c.mu.Lock()
	if !c.ended {
		c.ended = true
		c.endMsg = "the console connection ended"
	}
	c.mu.Unlock()
}

// markEnded records that the transport has gone, with its exit status if it
// gave one in time.
func (c *session) markEnded() {
	c.endInput()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.marked {
		return
	}
	c.marked = true
	switch {
	case c.result.Err != nil:
		c.endMsg = "the console connection ended: " + c.result.Err.Error()
	case c.result.Code != 0:
		c.endMsg = fmt.Sprintf("the console connection ended with exit status %d", c.result.Code)
	default:
		c.endMsg = "the console connection ended"
	}
	c.tx.Mark(c.endMsg)
	c.notifyLocked()
}

// notifyLocked wakes anything waiting for output. Call with mu held.
func (c *session) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// changes returns a channel that closes when something next arrives. Take it
// before sending, so a fast answer isn't missed.
func (c *session) changes() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

// touch marks the session as in use.
func (c *session) touch() {
	c.mu.Lock()
	c.lastUsed = time.Now()
	c.mu.Unlock()
}

func (c *session) idleFor() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.lastUsed)
}

// line is where the cursor is.
func (c *session) line() line {
	c.mu.Lock()
	defer c.mu.Unlock()
	return line{c.tx.Partial(), c.prompt, c.prompts}
}

// canSend is nil when something can be typed at the session.
func (c *session) canSend() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("session %s is closed, so nothing was sent; call connect again", c.id)
	}
	if c.ended {
		return fmt.Errorf("%s, so nothing was sent. Disconnect and connect again", c.endMsg)
	}
	if c.sh.Status().Paused {
		return errors.New(pausedText) // before anything else, so a paused tool is told so
	}
	return nil
}

// errStuck means a write to the console didn't complete before the call's time
// ran out, so the session was torn down.
var errStuck = errors.New("the console stopped accepting input before the call's time ran out, so the session was closed " +
	"(part of what was typed may have reached the console). Connect again")

// toolSlack is the time a send tool has beyond its waits for the console to
// take what it types. A variable so tests can shorten it.
var toolSlack = 10 * time.Second

// guarded runs fn, a write to the console, so that it can't outlast ctx. A transport
// that stops taking input (an SSH channel with a closed window) would block
// the write for good, and with it the session's send lock. When ctx ends first
// the transport is closed, which fails the write, and guarded waits a moment for it
// to be gone so nothing lands after the tool has returned. The session is
// dead then; the caller forgets it.
func (c *session) guarded(ctx context.Context, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// A stop or a dropped link cancels the call without the write being stuck,
		// and the write returns at once with its own error: let it.
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			select {
			case err := <-done:
				return err
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	c.closeTransport()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return errStuck
}

// closeTransport tears the transport down, once. It types nothing at the
// console.
func (c *session) closeTransport() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.trOnce.Do(c.tr.Close)
}

// write types b at the console, within ctx, and returns when the last byte is
// out. On a serial console that takes a while, since the hub paces them.
func (c *session) write(ctx context.Context, b []byte) error {
	return c.writeUnits(ctx, [][]byte{b})
}

// writeUnits is write for a call made of separate keys and text: the hub fits
// each to the console on its own, so "esc" then "enter" is ESC ESC CR and not
// the Alt-Enter the joined bytes would read as, and it refuses the whole call if
// the link would drop any of it.
func (c *session) writeUnits(ctx context.Context, units [][]byte) error {
	b := bytes.Join(units, nil)
	c.mu.Lock()
	c.lastEsc = len(b) > 0 && b[len(b)-1] == 0x1b
	c.mu.Unlock()
	err := c.guarded(ctx, func() error {
		return c.sh.TypeKeys(context.Background(), units) // guarded ends it if ctx does
	})
	if errors.Is(err, errStuck) {
		return err
	}
	return shareError(err)
}

// typingBudget is the typing time for a wire payload: a LineDelay for each
// CR or LF in it, a CharDelay for each byte, and one more LineDelay for an
// Enter still pending from an earlier call.
func (c *session) typingBudget(wire []byte) time.Duration {
	lines := bytes.Count(wire, []byte{'\r'}) + bytes.Count(wire, []byte{'\n'})
	return c.typingTime(len(wire), lines+1)
}

// typingTime is how long the hub takes to type n bytes in lines lines, so a
// tool's time limit covers it. It is zero for a console that isn't paced.
func (c *session) typingTime(n, lines int) time.Duration {
	l, ok := c.tr.Link.(interface{ Serial() bool })
	if !ok || !l.Serial() || c.tr.Pacing.Char == nil || c.tr.Pacing.Line == nil {
		return 0
	}
	return time.Duration(n)*(*c.tr.Pacing.Char) + time.Duration(lines)*(*c.tr.Pacing.Line)
}

// unread returns what the console printed since output was last returned,
// ending with the line the cursor is on, and marks it returned.
func (c *session) unread() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	lines, lost, count, cursor := c.tx.Snapshot(c.seen)
	c.seen, c.seenAt = count, cursor
	return shown(lines, cursor, lost)
}

// output is the unread output, followed by any warnings.
func (c *session) output() string {
	return strings.Join(append([]string{c.unread()}, c.notes()...), "\n\n")
}

// notes are warnings for the model about the session, if any.
func (c *session) notes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n []string
	if c.ended {
		n = append(n, "Session ended: "+c.endMsg+". Disconnect, then connect again.")
	}
	if c.atBMC {
		n = append(n, "This session is at the BMC's own command line, not the host console. Everything typed here needs the "+
			"user's approval. bmc_prompt with to=console goes back.")
	}
	if console.SecretPrompt.MatchString(c.tx.Partial()) {
		n = append(n, "The console is asking for a login. Ask the user to log in or to give you credentials. Don't guess: "+
			"failed logins can lock the account.")
	}
	return n
}

// unreadWaiting reports whether there's output not yet returned.
func (c *session) unreadWaiting() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _, count, cursor := c.tx.Snapshot(c.seen)
	return count != c.seen || cursor != c.seenAt
}

// settle waits for the console to answer: until it has been quiet for quietFor
// after it last printed, or limit passes. changed is from changes, taken
// before sending. With started, output has already begun, so quiet counts from
// now.
func (c *session) settle(ctx context.Context, changed <-chan struct{}, limit time.Duration, started bool) {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	var quiet <-chan time.Time // runs once output starts
	if started {
		quiet = time.After(quietFor)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-quiet:
			return
		case <-c.doneC:
			return
		case <-c.eofC:
			return
		case <-changed:
			changed = c.changes()
			quiet = time.After(quietFor)
		}
	}
}

// waitFor waits until unread output matches p, or limit passes, and reports
// whether it matched.
func (c *session) waitFor(ctx context.Context, p *regexp.Regexp, limit time.Duration) bool {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		changed := c.changes()
		if c.unreadMatches(p) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-c.doneC:
			return c.unreadMatches(p)
		case <-changed:
		}
	}
}

func (c *session) unreadMatches(p *regexp.Regexp) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	lines, _, _, cursor := c.tx.Snapshot(c.seen)
	if slices.ContainsFunc(lines, p.MatchString) {
		return true
	}
	return (len(lines) > 0 || cursor != c.seenAt) && p.MatchString(cursor) // not the prompt already returned
}

// history returns the last n lines, including the cursor line, that match p
// (all of them if p is nil), within shownBytesMax.
func (c *session) history(p *regexp.Regexp, n int) string {
	all := c.tx.Tail(console.HistoryLines)
	if cursor := c.tx.Partial(); cursor != "" {
		all = append(all, cursor)
	}
	var out []string
	size := 0
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		if l := clip(all[i]); p == nil || p.MatchString(all[i]) {
			if size += len(l) + 1; size > shownBytesMax {
				break
			}
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return "(nothing found)"
	}
	slices.Reverse(out)
	return strings.Join(out, "\n")
}

// clip cuts a line longer than lineMax, keeping its end.
func clip(l string) string {
	if len(l) <= lineMax {
		return l
	}
	l = l[len(l)-lineMax:]
	for len(l) > 0 && !utf8.RuneStart(l[0]) {
		l = l[1:]
	}
	return "…" + l
}

// shown renders lines and the cursor line for a result, keeping the newest
// within shownLinesMax and shownBytesMax.
func shown(lines []string, cursor string, lost int) string {
	if len(lines) == 0 && cursor == "" && lost == 0 {
		return "(no new output)"
	}
	cursor = clip(cursor)
	size, keep := len(cursor), 0
	for keep < len(lines) && keep < shownLinesMax {
		l := clip(lines[len(lines)-1-keep])
		if size+len(l)+1 > shownBytesMax {
			break
		}
		size += len(l) + 1
		keep++
	}
	var b strings.Builder
	if cut := lost + len(lines) - keep; cut > 0 {
		fmt.Fprintf(&b, "[%d earlier lines not shown; read_history has the last %d]\n", cut, console.HistoryLines)
	} else if len(lines) == 0 {
		b.WriteString("(no new lines; the cursor is on this one)\n")
	}
	for _, l := range lines[len(lines)-keep:] {
		b.WriteString(clip(l) + "\n")
	}
	b.WriteString(cursor)
	return b.String()
}

// target is the host an approval is for, noting the answer in the log.
func (c *session) target() target {
	return target{name: c.host, addr: c.addr, typ: c.typ, mark: c.tx.Mark}
}

// close ends the session and closes the log. Safe to call twice. It never
// types at the console: the console's own disconnect sequence starts with a
// carriage return, which would run whatever is on the line, so the transport
// is closed instead (an SSH channel hangs up; for SOL the ipmitool client is
// stopped and then the session is deactivated at the BMC).
func (c *session) close() {
	c.once.Do(func() {
		c.tx.Mark("session closed")
		c.closeTransport()
		if c.tr.Release != nil {
			rel := make(chan struct{})
			go func() {
				defer close(rel)
				c.tr.Release()
			}()
			select {
			case <-rel:
			case <-time.After(5 * time.Second):
			}
		}
		select {
		case <-c.drained:
		case <-time.After(3 * time.Second):
		}
		if path, _ := c.tx.Close(); path != "" {
			c.mu.Lock()
			c.logPath = path
			c.mu.Unlock()
		}
	})
}
