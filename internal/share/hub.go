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
	replayMax      = 64 << 10 // recent output a joining window gets
	agentReplayMax = 2 << 10  // of the current line, all an AI tool gets on joining
	queueMax       = 16 << 20 // bytes waiting for one client before it's dropped as too slow
	dataChunk      = 64 << 10 // queued output for one client is merged into messages this big
	sendTimeout    = 5 * time.Second
	helloTimeout   = 5 * time.Second
	dropNoteEvery  = 2 * time.Second // how often a window is told its keys are being dropped
	endWait        = 2 * time.Second // after the output ends, how long to wait for the exit status
	progressEvery  = 50 * time.Millisecond
)

// breakMemory is how long a BREAK is reported to a client that joins after it.
const breakMemory = time.Minute

// typingWindow: an AI tool can't take the keyboard this soon after the user's
// last key. A variable so tests can shorten it.
var typingWindow = 3 * time.Second

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

// hangupper is a link that can end the session without typing anything at the
// remote (see console's hangupper).
type hangupper interface {
	Hangup() error
}

// hub shares one console transport between the clients connected to it: the
// windows and at most one AI tool. It runs in whichever process opened it.
//
// One goroutine, loop, owns every field in the second group below. Everything
// else (a client's reader, the output pump, the typer's goroutine, timers, the
// goroutines that run a blocking link operation) hands it a closure with post
// or query, and the closure runs there, one at a time, in the order posted. So
// a decision (may this client type? may the AI tool have the keyboard?) and the
// change it makes are never interleaved with anything else, and no lock is
// needed for them. Nothing in the loop waits on the network, the link or a
// timer: a blocking operation (a link operation) runs in a goroutine of its
// own, and posts its result back when it ends. The typer never blocks its
// callers (a stuck write holds only its own goroutine), so the loop calls it
// directly. While a link operation or an erase runs the hub is busy, and window
// input and new leases are refused.
type hub struct {
	tr      *console.Transport
	ty      *typer.Typer
	closeTr func() // closes the transport, once
	stop    context.CancelFunc

	addr, typ string // which console this is, as the host that opened it resolved it

	ev      chan func()   // closures for the loop
	quit    chan struct{} // closed by shutdown: the loop ends
	prog    chan [2]int   // latest typing progress, for broadcast
	tx      txLog         // what the typer wrote, from its goroutine to the loop
	sending atomic.Bool   // the typer is inside the link's Send

	closeOnce   sync.Once
	releaseOnce sync.Once
	unlockOnce  sync.Once
	onClose     func() // the host's: stop listening
	unlock      func() // the host's: let go of the ownership lock

	// Owned by the loop.

	peers     map[*peer]struct{}
	local     *peer // the host process's own window or AI client
	agent     *peer
	ring      []byte
	kind      string
	atPrompt  bool
	keysNoted bool // the link's input note was shown

	lease     bool
	leaseID   int               // which acquisition holds the keyboard, while lease is set
	cancelled map[cancelKey]int // lease ids released before their request arrived, per client, in order seen
	cancelSeq int
	paused    bool
	gen       int  // changes whenever the AI tool's keyboard is taken back
	writing   bool // the AI tool's write is being typed
	busy      int  // exclusive link operations (BREAK, toggle...) and an agent's erase running; window input and new leases wait for 0

	// What is on the console's line, from the bytes the typer has written.
	segs      []seg // who queued the bytes the typer has taken and not yet written, in order
	wrote     int64 // bytes written to the link
	lineDirty bool  // the last byte written was neither Enter nor ctrl+c: the line may hold typed, unsubmitted input
	userTyped int   // what the user has typed on the line and not entered
	userEsc   int   // where an escape sequence the user typed ended, between writes
	agentLine typing
	txErr     error // why the typer stopped, if it did

	lastKey  time.Time
	breakAt  time.Time // the last BREAK that went out, from any client
	lastDrop time.Time
	closed   bool
	orphan   bool // the host's own client left; the hub ends with its last window
}

// cancelKey names a lease request by the client that made it: ids count per client.
type cancelKey struct {
	p  *peer
	id int
}

// Whose bytes a seg holds.
const (
	byUser = iota
	byAgent
	byErase // the backspaces that undo what the AI tool left on the line
)

// seg is a run of bytes handed to the typer by one kind of client.
type seg struct{ who, n int }

// escape sequence states of the user's typing.
const (
	escNone  = iota
	escStart // after ESC
	escBody  // in a CSI or SS3 sequence, until its final byte
)

// typing is what the AI tool has typed since it last pressed Enter.
type typing struct {
	chars   int
	ctrl    bool
	text    []byte
	entered bool
	// sensitive: the line was started at a login or password prompt. Decided
	// before the first byte is sent, from where the cursor was then, since
	// what the console prints in answer can change the cursor line before the
	// typing is accounted for.
	sensitive bool
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

	// A window's typing is one input stream: filter holds the start of an
	// escape sequence a message ended in; frag gives up on it if the rest
	// doesn't come. Both belong to the loop.
	filter console.InputFilter
	frag   *time.Timer
}

// txLog is the one thing the typer's goroutine shares with the loop: what it
// wrote, in order, and why it stopped. The typer records a write before it
// counts the bytes as gone, so a loop that reads the typer's queue and then
// takes the log has seen every byte that left the queue.
type txLog struct {
	mu     sync.Mutex
	chunks [][]byte
	err    error
	wake   chan struct{}
}

func (l *txLog) add(b []byte, err error) {
	l.mu.Lock()
	if b != nil {
		l.chunks = append(l.chunks, bytes.Clone(b))
	}
	if err != nil && l.err == nil {
		l.err = err
	}
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *txLog) take() ([][]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.chunks
	l.chunks = nil
	return c, l.err
}

func newHub(tr *console.Transport) *hub {
	ctx, cancel := context.WithCancel(context.Background())
	h := &hub{tr: tr, stop: cancel, peers: map[*peer]struct{}{}, kind: tr.Kind,
		ev: make(chan func()), quit: make(chan struct{}), prog: make(chan [2]int, 1),
		tx: txLog{wake: make(chan struct{}, 1)}}
	h.closeTr = sync.OnceFunc(func() {
		if tr.Close != nil {
			tr.Close()
		}
	})
	h.ty = console.NewTyper(tr.Link, tr.Pacing)
	h.ty.Progress = h.progress
	go h.loop()
	go func() {
		err := h.ty.Run(ctx, writerFunc(func(b []byte) (int, error) {
			h.sending.Store(true)
			err := tr.Link.Send(b)
			h.sending.Store(false)
			if err != nil {
				// Recorded before the typer is told, so a write waiting on the
				// dropped bytes sees why; a failed write was not typed.
				h.tx.add(nil, err)
				return 0, err
			}
			h.tx.add(b, nil)
			return len(b), nil
		}))
		if ctx.Err() == nil && err != nil {
			h.tx.add(nil, err)
			h.post(func() { h.note("typing stopped: " + err.Error()) })
		}
	}()
	go h.pump()
	return h
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// loop runs the closures posted to it until shutdown. It also shares the
// typer's progress with the windows, at most once per progressEvery.
func (h *hub) loop() {
	in := h.prog
	var tick <-chan time.Time
	for {
		select {
		case f := <-h.ev:
			f()
		case <-h.tx.wake:
			h.drain()
		case v := <-in:
			if !h.closed {
				for p := range h.peers {
					if !p.agent {
						h.queue(p, msg{T: tProg, Pending: v[0], Total: v[1]})
					}
				}
			}
			in, tick = nil, time.After(progressEvery)
		case <-tick:
			in, tick = h.prog, nil
		case <-h.quit:
			return
		}
	}
}

// post runs f in the loop. It reports false, without running f, once the hub
// has shut down.
func (h *hub) post(f func()) bool {
	select {
	case h.ev <- f:
		return true
	case <-h.quit:
		return false
	}
}

// query is post, waiting until f has run.
func (h *hub) query(f func()) bool {
	done := make(chan struct{})
	if !h.post(func() { f(); close(done) }) {
		return false
	}
	<-done
	return true
}

// progress is the typer's progress hook: it must not block or take locks, so
// it leaves the latest value for the loop.
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

// Everything below runs in the loop, unless it says otherwise.

// drain takes in what the typer has written: the line's state follows the bytes
// that actually went out, whoever queued them.
func (h *hub) drain() {
	chunks, err := h.tx.take()
	if err != nil && h.txErr == nil {
		h.txErr = err
	}
	for _, b := range chunks {
		if len(b) == 0 {
			continue
		}
		h.wrote += int64(len(b))
		last := b[len(b)-1]
		h.lineDirty = last != '\r' && last != '\n' && last != 0x03
		for len(b) > 0 && len(h.segs) > 0 {
			s := &h.segs[0]
			n := min(s.n, len(b))
			switch s.who {
			case byUser:
				h.trackUser(b[:n])
			case byAgent:
				h.trackAgent(b[:n])
			}
			b, s.n = b[n:], s.n-n
			if s.n == 0 {
				h.segs = h.segs[1:]
			}
		}
	}
}

// pending is how much is waiting for the typer, or in it, with the line's
// state brought up to date with everything that is not.
func (h *hub) pending() int {
	n := h.ty.Queued()
	h.drain()
	return n
}

// enqueue queues b behind everything already queued. then, if set, gets the
// channel that closes once b is written or dropped.
func (h *hub) enqueue(b []byte, who int, then func(done <-chan struct{})) {
	h.segs = append(h.segs, seg{who, len(b)})
	done := h.ty.Enqueue(b)
	if then != nil {
		then(done)
	}
}

// clearQueue drops what is queued. What was written before is accounted for,
// and nothing dropped is.
func (h *hub) clearQueue() {
	h.ty.Clear()
	h.drain()
	h.segs = nil // all that is left of what the typer had taken is dropped
}

// pump shares the console's output until it ends, then tells everyone. It is
// not part of the loop: it only hands the loop what it read.
func (h *hub) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := h.tr.Out.Read(buf)
		if n > 0 {
			b := append([]byte(nil), buf[:n]...)
			if !h.post(func() { h.output(b) }) {
				return
			}
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
	h.post(func() { h.end(res) })
}

// output follows the console's state, records output and sends it to
// everyone. The state goes first, so a client that reads it with the data
// sees where the console was when that data arrived.
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

// end tells every client the console ended, once, and stops the typer.
func (h *hub) end(res console.Result) {
	if h.closed {
		return
	}
	h.closed = true
	text := ""
	if res.Err != nil {
		text = res.Err.Error()
	}
	for p := range h.peers {
		h.queue(p, msg{T: tEnd, Code: res.Code, Text: text})
	}
	h.stop()
	// A write still stuck in the link will never finish: the console is gone.
	// Close the transport so it fails and the typer's goroutine ends (shutdown
	// closes it too, and gets to it first if the host closed the console).
	if h.sending.Load() {
		go h.closeTr()
	}
}

// Not in the loop.

// shutdown ends the console from the host's side: everyone is told, the
// transport is closed, the host stops listening, and the loop ends. Safe to
// call twice.
func (h *hub) shutdown() {
	h.closeOnce.Do(func() {
		res := console.Result{Err: errors.New("the oob that opened this console closed it")}
		h.query(func() { h.end(res) })
		h.closeTr()
		if h.onClose != nil {
			h.onClose()
		}
		// The ownership lock stays until the BMC-side release has run: with it
		// let go first, another oob could open the console and have the
		// release deactivate its session. A transport with a Release is let go
		// by release; if nobody calls it, the lock goes after releaseWait.
		if h.tr.Release == nil {
			h.unlockHost()
		} else {
			time.AfterFunc(2*releaseWaitFor(), h.unlockHost)
		}
		close(h.quit)
	})
}

// releaseWait bounds the BMC-side release, in nanoseconds. Atomic so tests can
// shorten it while a hub from an earlier test is still finishing.
var releaseWait atomic.Int64

func init() { releaseWait.Store(int64(5 * time.Second)) }

func releaseWaitFor() time.Duration { return time.Duration(releaseWait.Load()) }

// unlockHost lets go of the host's ownership lock, once.
func (h *hub) unlockHost() {
	h.unlockOnce.Do(func() {
		if h.unlock != nil {
			h.unlock()
		}
	})
}

// release ends the session at the BMC's end, once, after the transport is
// closed, and only then lets go of the ownership lock. It waits at most
// releaseWait for the BMC.
func (h *hub) release() {
	h.releaseOnce.Do(func() {
		if h.tr.Release != nil {
			rel := make(chan struct{})
			go func() {
				defer close(rel)
				h.tr.Release()
			}()
			select {
			case <-rel:
			case <-time.After(releaseWaitFor()):
			}
		}
		h.unlockHost()
	})
}

// hostLeft is the host process's own client going away. If other windows are
// still on the console and the host may keep it (an MCP server can; a
// terminal's oob sol can't, it is about to exit), the hub stays until the last
// of them leaves. It reports whether it stayed.
func (h *hub) hostLeft(noLinger bool) (stay bool) {
	h.query(func() {
		windows := 0
		for p := range h.peers {
			if !p.agent && p != h.local {
				windows++
			}
		}
		stay = windows > 0 && !noLinger && !h.closed
		if stay {
			h.orphan = true
		}
	})
	return stay
}

// lingering reports whether the hub is kept by its windows after the host left.
func (h *hub) lingering() (linger bool) {
	h.query(func() { linger = h.orphan && !h.closed })
	return linger
}

// cursorLine is the output since the cursor's line began.
func (h *hub) cursorLine() []byte {
	start := 0
	for _, loc := range lineStart.FindAllIndex(h.ring, -1) {
		start = loc[1]
	}
	return h.ring[start:]
}

// cursorText is the cursor's line as the user sees it.
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
	_ = conn.SetReadDeadline(time.Now().Add(helloTimeout))
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
	if why := h.identityMismatch(hello); why != "" {
		_ = w.send(msg{T: tBye, Text: why})
		conn.Close()
		return
	}
	p := &peer{w: w, conn: conn, wake: make(chan struct{}, 1), done: make(chan struct{}), agent: hello.Agent, name: hello.Name}

	refused := "this console has just closed"
	h.query(func() { refused = h.join(p, local, hello.Replay) })
	if refused != "" {
		_ = w.send(msg{T: tBye, Text: refused})
		conn.Close()
		return
	}
	go p.writeLoop()
	h.readLoop(p)
	p.drop()
	h.post(func() { h.leave(p) })
}

// join admits p, or says why not. The state, then the output so far, then live
// output are all queued in this one step, so nothing is lost or repeated in
// between.
func (h *hub) join(p *peer, local, replay bool) string {
	switch {
	case h.closed:
		return "this console has just closed"
	case p.agent && h.agent != nil:
		return "another AI tool is using this console"
	}
	h.peers[p] = struct{}{}
	if local {
		h.local = p
	}
	if p.agent {
		h.agent = p
		h.note(p.name + " joined")
	}
	h.broadcastState()
	if !h.breakAt.IsZero() && time.Since(h.breakAt) < breakMemory {
		h.queue(p, msg{T: tBreak, AgoMS: time.Since(h.breakAt).Milliseconds()})
	}
	if replay := h.replay(p.agent, replay); len(replay) > 0 {
		h.queue(p, msg{T: tData, Data: replay, Replay: true})
	}
	return ""
}

// identityMismatch says why a client that resolved the host to something else
// can't join this console, or "". The socket's name already keeps different
// addresses apart; this checks what the client says in its hello, so a socket
// reached by another route still can't hand a window the wrong server's console.
func (h *hub) identityMismatch(hello msg) string {
	switch {
	case hello.Addr != h.addr:
		return "this console is open in another oob for a different BMC address than this host has now; close that one first"
	case hello.Type != "" && h.typ != "" && hello.Type != h.typ:
		return "this console is open in another oob as a " + h.typ + " host, but this one has it as " + hello.Type + "; close that one first"
	}
	return ""
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
	if p.frag != nil {
		p.frag.Stop()
		p.frag = nil
	}
	if _, ok := h.peers[p]; !ok {
		return
	}
	delete(h.peers, p)
	for k := range h.cancelled {
		if k.p == p {
			delete(h.cancelled, k)
		}
	}
	if p == h.agent && (h.lease || h.agentLine.chars > 0) {
		h.revoke(func(string, string) { h.left(p) })
		return
	}
	h.left(p)
}

// left finishes a client's leaving, once whatever it left on the line is dealt with.
func (h *hub) left(p *peer) {
	if p == h.agent {
		h.agent = nil
		h.note(p.name + " left")
	}
	h.broadcastState()
	// An orphaned hub (its host process left it for the windows) ends with its
	// last window, whether or not the console has ended meanwhile: end only
	// marks the hub closed, and it is shutdown that stops listening and lets
	// go of the socket and lock. shutdown and release run once.
	lastWindow := h.orphan
	for q := range h.peers {
		if !q.agent {
			lastWindow = false
		}
	}
	if lastWindow {
		go func() {
			h.shutdown()
			h.release()
		}()
	}
}

// readLoop hands the loop what a client sends, in order. Not in the loop.
func (h *hub) readLoop(p *peer) {
	for {
		m, err := p.w.recv()
		if err != nil {
			return
		}
		var f func()
		switch m.T {
		case tWrite:
			f = func() { h.userWrite(p, m.Data, false) }
		case tUrgent:
			f = func() { h.userWrite(p, m.Data, true) }
		case tClear:
			f = func() { h.userClear(p) }
		case tReq:
			f = func() { h.request(p, m) }
		default:
			continue
		}
		if !h.post(f) {
			return
		}
	}
}

// userWrite is a window's typing. While the AI tool has the keyboard, or an
// exclusive operation on the link (BREAK, toggle) is running, it is dropped, so
// the console's line never mixes the two; the window is told why.
func (h *hub) userWrite(p *peer, b []byte, urgent bool) {
	if len(b) == 0 || p.agent || h.closed || h.windowBlocked(p) {
		return
	}
	h.typeWindow(h.mapUser(p, b), urgent)
}

// windowBlocked drops a window's input if the keyboard isn't theirs now, and
// tells the window why (not too often).
func (h *hub) windowBlocked(p *peer) bool {
	if !h.lease && h.busy == 0 {
		return false
	}
	if now := time.Now(); now.Sub(h.lastDrop) > dropNoteEvery {
		h.lastDrop = now
		if h.lease {
			h.queue(p, msg{T: tNote, Text: h.agentName() + " is working · {keys} i pauses it"})
		} else {
			h.queue(p, msg{T: tNote, Text: "the console is busy · keys dropped"})
		}
	}
	return true
}

// typeWindow queues a window's bytes. An urgent write drops the queue first.
func (h *hub) typeWindow(b []byte, urgent bool) {
	if len(b) == 0 {
		return
	}
	h.lastKey = time.Now()
	if !urgent {
		h.enqueue(b, byUser, nil)
		return
	}
	h.clearQueue()
	h.segs = append(h.segs, seg{byUser, len(b)})
	h.ty.Urgent(b)
}

// agentName is how the AI tool is named to windows.
func (h *hub) agentName() string {
	if h.agent != nil {
		return h.agent.name
	}
	return "the AI tool"
}

// fragWait is how long a window's unfinished escape sequence waits for its
// rest before it is dropped. A variable so tests can shorten it.
var fragWait = 100 * time.Millisecond

// mapUser fits a window's typing to the link (an iLO VSP reads ESC with the
// byte after it) and shows the link's note once. The window's typing is one
// stream: an escape sequence split between two messages is held until the
// rest comes (or fragWait passes) and then dropped whole.
func (h *hub) mapUser(p *peer, b []byte) []byte {
	out, note := p.filter.Map(h.tr.Link, b)
	h.keysNote(note)
	if p.frag != nil {
		p.frag.Stop()
		p.frag = nil
	}
	if p.filter.Pending() {
		var t *time.Timer
		t = time.AfterFunc(fragWait, func() {
			h.post(func() {
				if p.frag == t {
					h.dropFragment(p)
				}
			})
		})
		p.frag = t
	}
	return out
}

func (h *hub) keysNote(note string) {
	if note != "" && !h.keysNoted {
		h.keysNoted = true
		h.note(note)
	}
}

// dropFragment gives up on a window's unfinished escape sequence.
// What it held is dropped while the link maps input; if the link has stopped
// mapping since, it was ordinary input and is typed.
func (h *hub) dropFragment(p *peer) {
	out, note := p.filter.Flush(h.tr.Link)
	h.keysNote(note)
	p.frag = nil
	if len(out) == 0 || h.closed || h.windowBlocked(p) {
		return
	}
	h.typeWindow(out, false)
}

// userClear is the window's x key: drop what is queued to be typed. The AI tool
// having the keyboard, or an exclusive link operation running (busy), refuses
// the clear as it refuses typing.
func (h *hub) userClear(p *peer) {
	if p.agent || h.lease || h.busy > 0 || h.closed {
		return
	}
	h.clearQueue()
}

// trackUser follows what the user has typed and not entered, so an AI tool
// doesn't type onto it. It goes by the bytes that were written: a paste that
// was cleared half way leaves what went out on the line, and the AI tool
// waits for the user to finish or clear it. The user taking the line also ends
// the AI tool's claim on it.
func (h *hub) trackUser(b []byte) {
	h.agentLine = typing{}
	for _, c := range b {
		switch {
		case h.userEsc == escStart:
			h.userEsc = escNone
			if c == '[' || c == 'O' {
				h.userEsc = escBody
			}
		case h.userEsc == escBody:
			if c >= 0x40 {
				h.userEsc = escNone
			}
		case c == '\r' || c == '\n' || c == 0x03 || c == 0x15:
			h.userTyped = 0
		case c == 0x7f || c == '\b':
			h.userTyped = max(0, h.userTyped-1)
		case c == 0x1b:
			h.userEsc = escStart
		case c >= 0x20 && utf8.RuneStart(c):
			h.userTyped++
		}
	}
}

// trackAgent follows what the AI tool typed, so a stop can erase it and the
// windows' history can mark its commands.
func (h *hub) trackAgent(b []byte) {
	t := &h.agentLine
	for _, c := range b {
		switch {
		case c == '\r' || c == '\n':
			if len(t.text) > 0 {
				h.markCommand(string(t.text), t.sensitive)
			}
			// What follows in the same write answers whatever this line
			// opened: after a login line, a password.
			*t = typing{entered: true, sensitive: t.sensitive}
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
// except an answer to a login or password prompt (sensitive: decided before the
// line was sent, see typing).
func (h *hub) markCommand(text string, sensitive bool) {
	if sensitive {
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
// dropped, what it typed and didn't enter is erased, and then, with the
// keyboard still held until the erase is typed, then is told what happened.
// The erase is typed outside the loop; the hub is busy until it is.
func (h *hub) revoke(then func(note, reason string)) {
	// The keyboard isn't given back until the cleanup below is typed: window
	// input and a new lease wait (busy), and what it had in flight is stopped
	// (gen). Otherwise a window's keys could land in the middle of the erase.
	h.gen++
	h.busy++
	h.clearQueue() // then the line's state is what was typed, no more
	h.erase(then)
}

// erase undoes what revoke found the AI tool had left on the line.
func (h *hub) erase(then func(note, reason string)) {
	t := h.agentLine
	h.agentLine = typing{}
	n := 0
	var note, reason string
	switch {
	case t.chars > 0 && !t.ctrl && !pager.Match(h.ring) && !h.atLogin():
		n = t.chars
		note, reason = "paused · unsent command erased", StopCleared
	case t.chars > 0:
		note, reason = "paused · its text is left on the line", StopLeft
	case t.entered:
		note, reason = "paused · its command is still running", StopRunning
	default:
		note, reason = "paused", StopPaused
	}
	finish := func() {
		h.busy--
		h.lease, h.leaseID = false, 0
		h.broadcastState()
		then(note, reason)
	}
	if n == 0 {
		finish()
		return
	}
	h.enqueue(bytes.Repeat([]byte{'\b'}, n), byErase, func(done <-chan struct{}) { // backspace is understood almost everywhere; ctrl+u isn't
		go func() {
			select {
			case <-done:
			case <-time.After(sendTimeout): // the typer stopped: don't hold the keyboard for it
			}
			h.post(finish)
		}()
	})
}

var (
	errStopped = errors.New("stopped by the user")
	errGone    = errors.New("this client has left the console")
)

// request does what a client asked and queues its reply, now or, for what takes
// time, when it is done.
func (h *hub) request(p *peer, m msg) {
	if _, here := h.peers[p]; !here { // it has left: nobody to answer, and nothing it asks is done
		return
	}
	reply := func(r msg, err error) {
		r.T, r.ID = tReply, m.ID
		if err != nil {
			r.Err = err.Error()
		}
		h.queue(p, r)
	}
	switch m.Op {
	case opPause:
		h.pause(p, func(err error) { reply(msg{}, err) })
	case opLease, opRelease:
		reply(msg{}, h.agentOp(p, m))
	case opWrite:
		h.agentWrite(p, m, func(err error) { reply(msg{}, err) })
	default:
		h.linkOp(p, m, reply)
	}
}

// pause is the user pausing the AI tool, or letting it type again.
func (h *hub) pause(p *peer, reply func(error)) {
	switch {
	case p.agent:
		reply(errors.New("only a window can do that"))
		return
	case h.paused: // even with no AI tool on: the pause must be undoable
		h.paused = false
		name := "AI tool"
		if h.agent != nil {
			name = h.agent.name
		}
		h.note(name + " resumed")
		h.broadcastState()
		reply(nil)
		return
	case h.agent == nil:
		reply(errors.New("no AI tool is using this console"))
		return
	}
	a := h.agent
	needRevoke := h.lease || h.agentLine.chars > 0 || h.agentLine.ctrl
	leased := h.lease
	h.paused = true
	finish := func(note, reason string) {
		if needRevoke && leased {
			h.queue(a, msg{T: tStopped, Text: reason})
		}
		h.note(note)
		h.broadcastState()
		reply(nil)
	}
	if !needRevoke {
		finish(a.name+" paused", StopPaused)
		return
	}
	h.revoke(finish)
}

// refuse says why p can't act on the console right now, or nil.
func (h *hub) refuse(p *peer) error {
	switch {
	case p.agent && h.paused:
		return ErrPaused
	case h.busy > 0:
		return ErrBusy
	case !p.agent && h.lease:
		return errors.New(h.agentName() + " is working · {keys} i pauses it")
	case p.agent && (h.userTyped > 0 || time.Since(h.lastKey) < typingWindow):
		return ErrTyping
	}
	return nil
}

// cancelledMax bounds how many early releases the hub remembers.
const cancelledMax = 64

// agentOp is the AI tool taking the keyboard for a command, or giving it back.
// Each acquisition carries an id, so a release names the acquisition it ends:
// a late one can't free a newer lease, and a request its client gave up on
// (the release can arrive first) is refused instead of granted to nobody.
func (h *hub) agentOp(p *peer, m msg) error {
	if _, here := h.peers[p]; !here || (p.agent && h.agent != p) {
		return errGone
	}
	switch {
	case !p.agent:
		return errors.New("only an AI tool takes the keyboard")
	case m.Op == opRelease:
		switch {
		case h.lease && h.leaseID == m.Lease:
			h.lease, h.leaseID = false, 0 // what it typed without Enter stays tracked, for its next command or a stop
			h.broadcastState()
		case m.Lease != 0 && !(h.lease && h.leaseID == m.Lease):
			// Not held now: either long over, or its request is still on the
			// way. Remember it, so the request is refused when it arrives.
			if h.cancelled == nil {
				h.cancelled = map[cancelKey]int{}
			}
			h.cancelSeq++
			h.cancelled[cancelKey{p, m.Lease}] = h.cancelSeq
			if len(h.cancelled) > cancelledMax {
				var oldest cancelKey
				at := 0
				for k, seq := range h.cancelled {
					if at == 0 || seq < at {
						oldest, at = k, seq
					}
				}
				delete(h.cancelled, oldest)
			}
		}
		return nil
	case h.lease:
		return ErrBusy
	}
	if _, gone := h.cancelled[cancelKey{p, m.Lease}]; gone && m.Lease != 0 {
		delete(h.cancelled, cancelKey{p, m.Lease})
		return errors.New("the lease request was cancelled")
	}
	if h.closed {
		return ErrClosed
	}
	if err := h.refuse(p); err != nil {
		return err
	}
	if h.pending() > 0 { // the user's typing (or an erase) is still going out: not yet the AI tool's line
		return ErrTyping
	}
	h.lease, h.leaseID, h.agentLine.entered = true, m.Lease, false
	h.broadcastState()
	return nil
}

// mapKeys fits an AI tool's write to the link, one key at a time. cuts are the
// offsets in b where a key starts: the tool pressed named keys, and "esc" then
// "enter" is two keys (ESC ESC CR on a VSP), not the Alt-Enter that the same
// bytes would read as. The whole call is checked before any of it is typed: if
// the link would drop any key, none is sent.
func (h *hub) mapKeys(b []byte, cuts []int) ([]byte, error) {
	prev, out := 0, make([]byte, 0, len(b)+len(cuts)+1)
	for _, at := range append(append([]int(nil), cuts...), len(b)) {
		if at < prev || at > len(b) {
			return nil, errors.New("bad key boundaries in write")
		}
		m, note := console.MapInput(h.tr.Link, b[prev:at])
		if note != "" {
			return nil, console.ErrKeysDropped
		}
		out = append(out, m...)
		prev = at
	}
	return out, nil
}

// agentWrite types the AI tool's bytes and replies once they are out. Only
// while it is the AI tool here and has the keyboard, and not once the user has
// taken it back. All of that is decided in one step with the queueing, so a
// write from a client that has left can't be typed under the lease of the one
// that replaced it.
func (h *hub) agentWrite(p *peer, m msg, reply func(error)) {
	b, err := h.mapKeys(m.Data, m.Cuts)
	_, here := h.peers[p]
	switch {
	case err != nil: // keys the link would drop: type none of it
	case !p.agent:
		err = errors.New("only an AI tool uses write")
	case !here || h.agent != p:
		err = errGone
	case h.closed:
		err = errors.New("the console has ended")
	case h.paused:
		err = ErrPaused
	case !h.lease:
		err = errStopped
	case h.busy > 0 || h.writing: // being stopped, or an exclusive operation is running, or its last write isn't out
		err = ErrBusy
	}
	if err != nil {
		reply(err)
		return
	}
	if h.agentLine.chars == 0 && len(h.agentLine.text) == 0 {
		// A new line starts with this write: whether it is for a login or a
		// password is decided now, before anything is sent.
		h.agentLine.sensitive = h.atLogin()
	}
	gen := h.gen
	h.writing = true
	h.enqueue(b, byAgent, func(done <-chan struct{}) {
		go func() {
			select {
			case <-done:
			case <-p.done:
			}
			h.post(func() {
				h.writing = false
				h.drain()
				switch {
				case h.gen != gen:
					reply(errStopped) // revoke accounted for what was typed
				case h.txErr != nil: // the typer stopped, and dropped the bytes
					reply(h.txErr)
				case h.closed: // the console ended, and the typer dropped what was left
					reply(ErrClosed)
				default:
					reply(nil)
				}
			})
		}()
	})
}

// linkOp runs an operation on the transport's link. It is admitted here; the
// link is used in a goroutine of its own, with the hub busy, and the result
// comes back as a message.
func (h *hub) linkOp(p *peer, m msg, reply func(msg, error)) {
	local := p == h.local
	switch m.Op {
	case opSizeLine:
		sl, ok := h.tr.Link.(sizeLiner)
		if !local || p.agent || !ok { // like resize and fit: a joined window doesn't change the host's console, and typing this line would
			reply(msg{}, nil)
			return
		}
		go func() {
			text, ok := sl.SizeLine(m.Rows, m.Cols)
			h.post(func() { reply(msg{Text: text, OK: ok}, nil) })
		}()
		return
	case opResize, opFit, opDisconnect:
		if !local || p.agent { // a joined window doesn't change the host's console
			if m.Op == opDisconnect {
				reply(msg{}, errors.New("only the oob that opened the console can end it"))
			} else {
				reply(msg{}, nil)
			}
			return
		}
	}
	if err := h.refuse(p); err != nil && m.Op != opDisconnect {
		reply(msg{}, err)
		return
	}
	dirty, clear := false, false
	switch m.Op {
	case opBreak:
		// A serial BREAK is sent after a carriage return (IPMI SOL's escape
		// starts with one), which the remote takes as Enter: with a paste part
		// way through, or text typed on the line, it would run what was typed
		// ("reboot" out of "reboot --help"). Dropping the queue wouldn't undo
		// what already went out. So refuse until the line is clean.
		if h.pending() > 0 || h.lineDirty {
			reply(msg{}, ErrLinePending)
			return
		}
	case opToggle:
		if h.tr.Toggle == nil {
			reply(msg{}, errors.New("this console has no BMC prompt"))
			return
		}
		// Toggling drops what is queued: the user's typing isn't the AI tool's to drop.
		if p.agent && h.pending() > 0 {
			reply(msg{}, ErrTyping)
			return
		}
		clear = true
	case opDisconnect:
		// The disconnect sequence starts with an Enter, which would submit
		// whatever the line holds. Look before the queue is cleared, and end
		// the session without typing when there is anything.
		dirty, clear = h.pending() > 0 || h.lineDirty, true
	}
	h.busy++
	if clear {
		h.clearQueue() // typed input is dropped before the link is used
	}
	go h.runLinkOp(m, dirty, reply)
}

// runLinkOp uses the link, outside the loop, and posts the result back.
func (h *hub) runLinkOp(m msg, dirty bool, reply func(msg, error)) {
	var r msg
	var err error
	switch m.Op {
	case opBreak:
		err = h.tr.Link.Break()
	case opToggle:
		r.Kind, r.AtPrompt, err = h.tr.Toggle()
	case opDisconnect:
		if hl, ok := h.tr.Link.(hangupper); ok && dirty {
			err = hl.Hangup()
		} else {
			err = h.tr.Link.Disconnect()
		}
	case opResize:
		err = h.tr.Link.Resize(m.Rows, m.Cols)
	case opFit:
		err = h.tr.Link.FitSize(m.Rows, m.Cols)
	default:
		err = errors.New("unknown operation " + m.Op)
	}
	h.post(func() {
		h.busy--
		switch {
		case m.Op == opBreak && err == nil:
			h.breakAt = time.Now()
			for q := range h.peers { // the AI tool's safety checks care whoever sent it
				h.queue(q, msg{T: tBreak})
			}
		case m.Op == opToggle:
			h.kind, h.atPrompt = r.Kind, r.AtPrompt
			h.broadcastState()
		}
		reply(r, err)
	})
}

// Messages to clients.

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
	if m.T == tData {
		m.Data = bytes.Clone(m.Data) // the peer's own, since merging appends to it
	}
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
