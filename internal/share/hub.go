// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// Adapted from bauddy (internal/share/hub.go): one process owns the console and
// shares it with windows and at most one AI tool. The shared "port" is oob's
// console transport; typing from every client goes through one paced typer.

package share

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/termtext"
	"github.com/ssimpson89/oob/internal/typer"
)

const (
	replayMax      = 64 << 10        // recent output a joining window gets
	agentReplayMax = 2 << 10         // of the current line, all an AI tool gets on joining
	typingWindow   = 3 * time.Second // an AI tool can't take the keyboard this soon after the user's last key
	queueMax       = 16 << 20        // bytes waiting for one client before it's dropped as too slow
	dataChunk      = 64 << 10        // queued output for one client is merged into messages this big
	sendTimeout    = 5 * time.Second
	helloTimeout   = 5 * time.Second
	dropNoteEvery  = 2 * time.Second // how often a window is told its keys are being dropped
	endWait        = 2 * time.Second // after the output ends, how long to wait for the exit status
)

var (
	// pager matches a pager waiting at the end of the output, where a
	// backspace could be taken as a key.
	pager = regexp.MustCompile(`(?i)-\s*\(?more\b[^\n]*$`)
	// loginAt matches a login or password question on the cursor line.
	loginAt = regexp.MustCompile(`(?i)(login|username|password)( for \S+)?: ?$`)
	// lineStart finds where the cursor's line begins.
	lineStart = regexp.MustCompile("[\r\n]|\x1b\\[[0-9;]*[Hf]")
)

type serialLink interface{ Serial() bool }
type sizeLiner interface {
	SizeLine(rows, cols int) (string, bool)
}

// hub shares one console transport between the clients connected to it: the
// windows and at most one AI tool. It runs in whichever process opened it.
//
// Lock order: enqMu, then mu. mu guards the fields below it; enqMu orders what
// goes into the typer, so a stop can drop exactly what the AI tool had queued.
// Nothing holds mu while waiting on the typer or the link.
type hub struct {
	tr   *console.Transport
	ty   *typer.Typer
	now  func() time.Time
	stop context.CancelFunc

	wrote atomic.Int64 // bytes the typer has written to the link

	enqMu    sync.Mutex
	enqCount int64   // bytes handed to the typer, as far as is known
	flight   *flight // the AI tool's write being typed

	prog chan [2]int // latest typing progress, for broadcast

	mu        sync.Mutex
	peers     map[*peer]struct{}
	local     *peer // the host process's own window or AI client
	ring      []byte
	kind      string
	atPrompt  bool
	agent     *peer
	lease     bool
	paused    bool
	gen       int // changes whenever the AI tool's keyboard is taken back
	lastKey   time.Time
	userTyped int
	agentLine typing
	busy      bool
	closed    bool
	orphan    bool // the host's own client left; the hub ends with its last window
	lastDrop  time.Time
	result    *console.Result
	txErr     error // why the typer stopped, if it did

	finishOnce  sync.Once
	closeOnce   sync.Once
	releaseOnce sync.Once
	onClose     func() // the host's: stop listening, let go of the lock
}

// flight is the AI tool's write that is being typed.
type flight struct {
	start int64 // its first byte's place in the typer's stream
	b     []byte
}

// typing is what the AI tool has typed since it last pressed Enter.
type typing struct {
	chars   int
	ctrl    bool
	text    []byte
	entered bool
}

// peer is one connected client.
type peer struct {
	w    *wire
	conn net.Conn
	// out holds what is waiting to be sent; output waiting together is merged,
	// so a flood of it costs memory in bytes, not in messages.
	qmu   sync.Mutex
	out   []msg
	qsize int
	wake  chan struct{}
	done  chan struct{}
	agent bool
	name  string
	once  sync.Once
}

func newHub(tr *console.Transport) *hub {
	ctx, cancel := context.WithCancel(context.Background())
	h := &hub{tr: tr, now: time.Now, stop: cancel, peers: map[*peer]struct{}{},
		kind: tr.Kind, prog: make(chan [2]int, 1)}
	h.ty = console.NewTyper(tr.Link, tr.Pacing)
	h.ty.Progress = h.progress
	go func() {
		err := h.ty.Run(ctx, writerFunc(func(b []byte) (int, error) {
			err := tr.Link.Send(b)
			h.wrote.Add(int64(len(b)))
			return len(b), err
		}))
		if ctx.Err() == nil && err != nil {
			h.mu.Lock()
			h.txErr = err
			h.note("typing stopped: " + err.Error())
			h.mu.Unlock()
		}
	}()
	go h.progressLoop()
	go h.pump()
	return h
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// progress is the typer's progress hook: it must not block or take locks, so
// it leaves the latest value for progressLoop.
func (h *hub) progress(pending, total int) {
	v := [2]int{pending, total}
	for {
		select {
		case h.prog <- v:
			return
		default:
			select {
			case <-h.prog:
			default:
			}
		}
	}
}

func (h *hub) progressLoop() {
	for v := range h.prog {
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return
		}
		for p := range h.peers {
			if !p.agent {
				h.queue(p, msg{T: tProg, Pending: v[0], Total: v[1]})
			}
		}
		h.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
}

// pump shares the console's output until it ends, then tells everyone.
func (h *hub) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := h.tr.Out.Read(buf)
		if n > 0 {
			h.mu.Lock()
			h.output(append([]byte(nil), buf[:n]...))
			h.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	res := console.Result{}
	select {
	case res = <-h.tr.Done:
	case <-time.After(endWait):
	}
	h.finish(res)
}

// output follows the console's state, records output and sends it to
// everyone. The state goes first, so a client that reads it with the data
// sees where the console was when that data arrived. Call with mu held.
func (h *hub) output(b []byte) {
	if h.tr.Observe != nil {
		if kind, at, _ := h.tr.Observe(b); kind != h.kind || at != h.atPrompt {
			h.kind, h.atPrompt = kind, at
			h.broadcastState()
		}
	}
	h.ring = append(h.ring, b...)
	if over := len(h.ring) - replayMax; over > 0 {
		h.ring = h.ring[over:]
	}
	for p := range h.peers {
		h.queue(p, msg{T: tData, Data: b})
	}
}

// finish tells every client the console ended, once, and closes the hub.
func (h *hub) finish(res console.Result) {
	h.finishOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		h.result = &res
		text := ""
		if res.Err != nil {
			text = res.Err.Error()
		}
		for p := range h.peers {
			h.queue(p, msg{T: tEnd, Code: res.Code, Text: text})
		}
		h.mu.Unlock()
		h.stop()
	})
}

// shutdown ends the console from the host's side: everyone is told, the
// transport is closed, and the host stops listening. Safe to call twice.
func (h *hub) shutdown() {
	h.closeOnce.Do(func() {
		h.finish(console.Result{Err: errors.New("the oob that opened this console closed it")})
		if h.tr.Close != nil {
			h.tr.Close()
		}
		if h.onClose != nil {
			h.onClose()
		}
	})
}

// release ends the session at the BMC's end, once, after the transport is closed.
func (h *hub) release() {
	h.releaseOnce.Do(func() {
		if h.tr.Release != nil {
			h.tr.Release()
		}
	})
}

// hostLeft is the host process's own client going away. If other windows are
// still on the console and the host may keep it (an MCP server can; a
// terminal's oob sol can't, it is about to exit), the hub stays until the last
// of them leaves. It reports whether it stayed.
func (h *hub) hostLeft(noLinger bool) bool {
	h.mu.Lock()
	windows := 0
	for p := range h.peers {
		if !p.agent && p != h.local {
			windows++
		}
	}
	stay := windows > 0 && !noLinger && !h.closed
	if stay {
		h.orphan = true
	}
	h.mu.Unlock()
	return stay
}

// cursorLine is the output since the cursor's line began. Call with mu held.
func (h *hub) cursorLine() []byte {
	start := 0
	for _, loc := range lineStart.FindAllIndex(h.ring, -1) {
		start = loc[1]
	}
	return h.ring[start:]
}

// cursorText is the cursor's line as the user sees it. Call with mu held.
func (h *hub) cursorText() string {
	var c termtext.Cleaner
	c.Feed(h.cursorLine())
	return c.Partial()
}

func (h *hub) atLogin() bool { return loginAt.MatchString(h.cursorText()) }

// serve talks to one client until it goes away. local marks the host
// process's own client.
func (h *hub) serve(conn net.Conn, local bool) {
	w := newWire(conn)
	_ = conn.SetReadDeadline(h.now().Add(helloTimeout))
	hello, err := w.recv()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil || hello.T != tHello {
		conn.Close()
		return
	}
	if hello.Version != version {
		_ = w.send(msg{T: tBye, Text: "another version of oob has this console open; close that one first"})
		conn.Close()
		return
	}
	p := &peer{w: w, conn: conn, wake: make(chan struct{}, 1), done: make(chan struct{}), agent: hello.Agent, name: hello.Name}

	h.mu.Lock()
	switch {
	case h.closed:
		h.mu.Unlock()
		_ = w.send(msg{T: tBye, Text: "this console has just closed"})
		conn.Close()
		return
	case p.agent && h.agent != nil:
		h.mu.Unlock()
		_ = w.send(msg{T: tBye, Text: "another AI tool is using this console"})
		conn.Close()
		return
	}
	h.peers[p] = struct{}{}
	if local {
		h.local = p
	}
	if p.agent {
		h.agent = p
		h.note(p.name + " joined")
	}
	// The state, then the output so far, then live output: all queued under
	// mu, so nothing is lost or repeated in between.
	h.broadcastState()
	if replay := h.replay(p.agent, hello.Replay); len(replay) > 0 {
		h.queue(p, msg{T: tData, Data: replay, Replay: true})
	}
	h.mu.Unlock()

	go p.writeLoop()
	h.readLoop(p)
	h.leave(p)
}

// replay is the output a new client starts with: for a window, the recent
// output from the start of a line; for an AI tool, only the line the cursor is
// on, so it never sees what came before it joined.
func (h *hub) replay(agent, wanted bool) []byte {
	if agent {
		line := h.cursorLine()
		return bytes.Clone(line[max(0, len(line)-agentReplayMax):])
	}
	if !wanted {
		return nil
	}
	if i := bytes.IndexByte(h.ring, '\n'); i >= 0 && len(h.ring) == replayMax {
		return bytes.Clone(h.ring[i+1:])
	}
	return bytes.Clone(h.ring)
}

// leave forgets a client that went away. An AI tool leaving mid-command has
// its unsent text erased, so it can't leave half a command on the line.
func (h *hub) leave(p *peer) {
	p.drop()
	h.mu.Lock()
	if _, ok := h.peers[p]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.peers, p)
	wasAgent := p == h.agent
	needRevoke := wasAgent && (h.lease || h.agentLine.chars > 0)
	h.mu.Unlock()
	if needRevoke {
		h.revoke(false)
	}
	h.mu.Lock()
	if wasAgent {
		h.agent = nil
		h.note(p.name + " left")
	}
	h.broadcastState()
	lastWindow := h.orphan && !h.closed
	if lastWindow {
		for q := range h.peers {
			if !q.agent {
				lastWindow = false
			}
		}
	}
	h.mu.Unlock()
	if lastWindow {
		go func() {
			h.shutdown()
			h.release()
		}()
	}
}

func (h *hub) readLoop(p *peer) {
	for {
		m, err := p.w.recv()
		if err != nil {
			return
		}
		switch m.T {
		case tWrite:
			h.userWrite(p, m.Data, false)
		case tUrgent:
			h.userWrite(p, m.Data, true)
		case tClear:
			h.userClear(p)
		case tReq:
			go h.request(p, m)
		}
	}
}

// enq queues b behind everything already queued. Call without mu.
func (h *hub) enq(b []byte) <-chan struct{} {
	h.enqMu.Lock()
	defer h.enqMu.Unlock()
	h.enqCount += int64(len(b))
	return h.ty.Enqueue(b)
}

// dropQueued drops what is queued and returns the part of the AI tool's
// write in flight that was already typed. Call without mu.
func (h *hub) dropQueued() (typed []byte) {
	h.enqMu.Lock()
	defer h.enqMu.Unlock()
	h.ty.Clear()
	w := h.wrote.Load()
	if f := h.flight; f != nil {
		typed = f.b[:max(0, min(int64(len(f.b)), w-f.start))]
		h.flight = nil
	}
	h.enqCount = w
	return typed
}

// userWrite is a window's typing. While the AI tool has the keyboard it is
// dropped, so the console's line never mixes the two; the window is told why.
func (h *hub) userWrite(p *peer, b []byte, urgent bool) {
	if len(b) == 0 {
		return
	}
	h.mu.Lock()
	if p.agent || h.closed {
		h.mu.Unlock()
		return
	}
	if h.lease {
		if h.now().Sub(h.lastDrop) > dropNoteEvery {
			h.lastDrop = h.now()
			h.queue(p, msg{T: tNote, Text: h.agent.name + " is working · {keys} i pauses it"})
		}
		h.mu.Unlock()
		return
	}
	h.lastKey = h.now()
	h.trackUser(b)
	h.mu.Unlock()
	if !urgent {
		h.enq(b)
		return
	}
	h.enqMu.Lock()
	h.ty.Urgent(b)
	h.enqCount = h.wrote.Load() + int64(len(b))
	h.enqMu.Unlock()
}

// userClear is the window's x key: drop what is queued to be typed.
func (h *hub) userClear(p *peer) {
	h.mu.Lock()
	skip := p.agent || h.lease || h.closed
	h.mu.Unlock()
	if !skip {
		h.dropQueued()
	}
}

// trackUser counts what the user has typed and not entered, so an AI tool
// doesn't type onto it. The user taking the line also ends the AI tool's claim
// on it. Call with mu held.
func (h *hub) trackUser(b []byte) {
	h.agentLine = typing{}
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case c == '\r' || c == '\n' || c == 0x03 || c == 0x15:
			h.userTyped = 0
		case c == 0x7f || c == '\b':
			h.userTyped = max(0, h.userTyped-1)
		case c == 0x1b:
			i += escapeLen(b[i+1:])
		case c >= 0x20 && utf8.RuneStart(c):
			h.userTyped++
		}
	}
}

// escapeLen is how many bytes after an ESC belong to its sequence.
func escapeLen(rest []byte) int {
	if len(rest) == 0 {
		return 0
	}
	if rest[0] != '[' && rest[0] != 'O' {
		return 1
	}
	for i := 1; i < len(rest); i++ {
		if rest[i] >= 0x40 {
			return i + 1
		}
	}
	return len(rest)
}

// trackAgent follows what the AI tool typed, so a stop can erase it and the
// windows' history can mark its commands. Call with mu held.
func (h *hub) trackAgent(b []byte) {
	t := &h.agentLine
	for _, c := range b {
		switch {
		case c == '\r' || c == '\n':
			if len(t.text) > 0 {
				h.markCommand(string(t.text))
			}
			*t = typing{entered: true}
		case c == 0x03 || c == 0x15:
			*t = typing{}
		case c >= 0x20 && c != 0x7f:
			if utf8.RuneStart(c) {
				t.chars++
			}
			t.text = append(t.text, c)
		default:
			t.ctrl = true
		}
	}
}

// markCommand puts the AI tool's command in the windows' history and log,
// except an answer to a password prompt. Call with mu held.
func (h *hub) markCommand(text string) {
	if h.atLogin() {
		text = "answered a login prompt"
	} else {
		text = "typed: " + text
	}
	for p := range h.peers {
		if !p.agent && h.agent != nil {
			h.queue(p, msg{T: tMark, Text: h.agent.name + " " + text})
		}
	}
}

// Why the user stopped the AI tool, as it's told.
const (
	StopCleared = "cleared"          // its unsent command was erased
	StopLeft    = "left on the line" // its unsent text couldn't be erased
	StopRunning = "running"          // its command was entered and may still be running
	StopPaused  = "paused"           // nothing of it was on the line
)

// revoke takes the keyboard back from the AI tool: what it had queued is
// dropped, what it typed and didn't enter is erased, and the result says what
// happened. Call without mu.
func (h *hub) revoke(bool) (note, reason string) {
	h.mu.Lock()
	h.gen++
	h.lease = false
	h.mu.Unlock()
	partial := h.dropQueued()
	h.mu.Lock()
	if len(partial) > 0 {
		h.trackAgent(partial)
	}
	t := h.agentLine
	h.agentLine = typing{}
	erase := 0
	switch {
	case t.chars > 0 && !t.ctrl && !pager.Match(h.ring) && !h.atLogin():
		erase = t.chars
		note, reason = "paused · unsent command erased", StopCleared
	case t.chars > 0:
		note, reason = "paused · its text is left on the line", StopLeft
	case t.entered:
		note, reason = "paused · its command is still running", StopRunning
	default:
		note, reason = "paused", StopPaused
	}
	h.mu.Unlock()
	if erase > 0 {
		h.enq(bytes.Repeat([]byte{'\b'}, erase)) // backspace is understood almost everywhere; ctrl+u isn't
	}
	return note, reason
}

var (
	errStopped = errors.New("stopped by the user")
	errNoLease = errors.New("the AI tool doesn't have the keyboard")
)

func (h *hub) request(p *peer, m msg) {
	reply := msg{T: tReply, ID: m.ID}
	var err error
	switch m.Op {
	case opPause:
		err = h.pause(p)
	case opLease, opRelease:
		err = h.agentOp(p, m)
	case opWrite:
		err = h.agentWrite(p, m.Data)
	default:
		err = h.linkOp(p, m, &reply)
	}
	if err != nil {
		reply.Err = err.Error()
	}
	h.mu.Lock()
	h.queue(p, reply)
	h.mu.Unlock()
}

// pause is the user pausing the AI tool, or letting it type again.
func (h *hub) pause(p *peer) error {
	h.mu.Lock()
	switch {
	case p.agent:
		h.mu.Unlock()
		return errors.New("only a window can do that")
	case h.paused: // even with no AI tool on: the pause must be undoable
		h.paused = false
		name := "AI tool"
		if h.agent != nil {
			name = h.agent.name
		}
		h.note(name + " resumed")
		h.broadcastState()
		h.mu.Unlock()
		return nil
	case h.agent == nil:
		h.mu.Unlock()
		return errors.New("no AI tool is using this console")
	}
	a := h.agent
	needRevoke := h.lease || h.agentLine.chars > 0 || h.agentLine.ctrl
	leased := h.lease
	h.paused = true
	h.mu.Unlock()
	note, reason := a.name+" paused", StopPaused
	if needRevoke {
		note, reason = h.revoke(false)
		if leased {
			h.mu.Lock()
			h.queue(a, msg{T: tStopped, Text: reason})
			h.mu.Unlock()
		}
	}
	h.mu.Lock()
	h.note(note)
	h.broadcastState()
	h.mu.Unlock()
	return nil
}

// refuse says why p can't act on the console right now, or nil. Call with mu held.
func (h *hub) refuse(p *peer) error {
	switch {
	case p.agent && h.paused:
		return ErrPaused
	case h.busy:
		return ErrBusy
	case !p.agent && h.lease:
		return errors.New(h.agent.name + " is working · {keys} i pauses it")
	case p.agent && (h.userTyped > 0 || h.now().Sub(h.lastKey) < typingWindow):
		return ErrTyping
	}
	return nil
}

// agentOp is the AI tool taking the keyboard for a command, or giving it back.
func (h *hub) agentOp(p *peer, m msg) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case !p.agent:
		return errors.New("only an AI tool takes the keyboard")
	case m.Op == opRelease:
		if h.lease {
			h.lease = false // what it typed without Enter stays tracked, for its next command or a stop
			h.broadcastState()
		}
		return nil
	case h.lease:
		return ErrBusy
	}
	if err := h.refuse(p); err != nil {
		return err
	}
	h.lease, h.agentLine.entered = true, false
	h.broadcastState()
	return nil
}

// agentWrite types the AI tool's bytes and returns once they are out. Only
// while it has the keyboard, and not once the user has taken it back.
func (h *hub) agentWrite(p *peer, b []byte) error {
	h.enqMu.Lock()
	h.mu.Lock()
	var err error
	switch {
	case !p.agent:
		err = errors.New("only an AI tool uses write")
	case h.closed:
		err = errors.New("the console has ended")
	case h.paused:
		err = ErrPaused
	case !h.lease:
		err = errStopped
	}
	gen := h.gen
	h.mu.Unlock()
	if err != nil {
		h.enqMu.Unlock()
		return err
	}
	f := &flight{start: h.enqCount, b: b}
	h.flight = f
	h.enqCount += int64(len(b))
	done := h.ty.Enqueue(b)
	h.enqMu.Unlock()

	select {
	case <-done:
	case <-p.done:
		return errors.New("the AI tool went away")
	}
	h.enqMu.Lock()
	defer h.enqMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gen != gen {
		return errStopped // revoke counted what was typed
	}
	if h.txErr != nil { // the typer stopped, and dropped the bytes
		return h.txErr
	}
	if h.flight == f {
		h.flight = nil
	}
	h.trackAgent(b)
	return nil
}

// linkOp runs an operation on the transport's link, one at a time, without
// holding mu while it runs.
func (h *hub) linkOp(p *peer, m msg, reply *msg) error {
	h.mu.Lock()
	local := p == h.local
	switch m.Op {
	case opSizeLine:
		h.mu.Unlock()
		if sl, ok := h.tr.Link.(sizeLiner); ok {
			reply.Text, reply.OK = sl.SizeLine(m.Rows, m.Cols)
		}
		return nil
	case opResize, opFit, opDisconnect:
		if !local || p.agent { // a joined window doesn't change the host's console
			h.mu.Unlock()
			if m.Op == opDisconnect {
				return errors.New("only the oob that opened the console can end it")
			}
			return nil
		}
	}
	if err := h.refuse(p); err != nil && m.Op != opDisconnect {
		h.mu.Unlock()
		return err
	}
	h.busy = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.busy = false
		h.mu.Unlock()
	}()

	switch m.Op {
	case opBreak:
		h.dropQueued()
		return h.tr.Link.Break()
	case opToggle:
		if h.tr.Toggle == nil {
			return errors.New("this console has no BMC prompt")
		}
		h.dropQueued()
		kind, at, err := h.tr.Toggle()
		h.mu.Lock()
		h.kind, h.atPrompt = kind, at
		h.broadcastState()
		h.mu.Unlock()
		reply.Kind, reply.AtPrompt = kind, at
		return err
	case opDisconnect:
		h.dropQueued()
		return h.tr.Link.Disconnect()
	case opResize:
		return h.tr.Link.Resize(m.Rows, m.Cols)
	case opFit:
		return h.tr.Link.FitSize(m.Rows, m.Cols)
	}
	return errors.New("unknown operation " + m.Op)
}

// Messages to clients. Call these with mu held.

func (h *hub) status() Status {
	s := Status{Kind: h.kind, AtPrompt: h.atPrompt, HasToggle: h.tr.Toggle != nil,
		Windows: len(h.peers), Working: h.lease, Paused: h.paused,
		SizeFree: h.tr.Link.SizeIsFree(), CharNS: int64(h.ty.CharDelay), LineNS: int64(h.ty.LineDelay)}
	if sl, ok := h.tr.Link.(serialLink); ok {
		s.Serial = sl.Serial()
	}
	if h.agent != nil {
		s.Agent, s.Windows = h.agent.name, s.Windows-1
	}
	return s
}

func (h *hub) broadcastState() {
	s := h.status()
	for p := range h.peers {
		h.queue(p, msg{T: tState, State: &s})
	}
}

// note shows text on every window's bar. "{keys}" stands for the command key.
func (h *hub) note(text string) {
	for p := range h.peers {
		if !p.agent {
			h.queue(p, msg{T: tNote, Text: text})
		}
	}
}

// queue sends m to p without waiting. A client that has fallen far behind is
// dropped rather than allowed to stall everyone.
func (h *hub) queue(p *peer, m msg) {
	p.qmu.Lock()
	if n := len(p.out); n > 0 && m.T == tData && !m.Replay && p.out[n-1].T == tData && !p.out[n-1].Replay &&
		len(p.out[n-1].Data)+len(m.Data) <= dataChunk {
		p.out[n-1].Data = append(p.out[n-1].Data, m.Data...)
	} else {
		p.out = append(p.out, m)
	}
	p.qsize += len(m.Data) + 64
	over := p.qsize > queueMax
	p.qmu.Unlock()
	if over {
		p.drop()
		return
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *peer) writeLoop() {
	defer p.drop()
	for {
		select {
		case <-p.wake:
		case <-p.done:
			return
		}
		p.qmu.Lock()
		batch := p.out
		p.out, p.qsize = nil, 0
		p.qmu.Unlock()
		for _, m := range batch {
			_ = p.conn.SetWriteDeadline(time.Now().Add(sendTimeout))
			if p.w.send(m) != nil || m.T == tEnd || m.T == tBye {
				return
			}
		}
	}
}

// drop closes the connection, which ends both of the peer's loops.
func (p *peer) drop() {
	p.once.Do(func() {
		close(p.done)
		p.conn.Close()
	})
}

var _ io.Reader = (*outBuf)(nil)
