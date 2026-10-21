// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// Adapted from bauddy (internal/share/client.go): every process, the one that
// opened the console included, talks to the hub through a Client: the host over
// an in-memory pipe, the rest over a unix socket.

// Package share lets several oob windows, and an AI agent through oob mcp, be
// on one server console at once.
//
// An iLO allows one virtual serial port session, so two independent
// connections can't work: one process opens the console and shares it. A lock
// file decides which; the OS releases it if that process dies. The others join
// over a unix socket only this user can reach. Unlike bauddy there is no
// takeover: reopening a console means logging in to the BMC again, so when the
// process that opened it ends, the others are told the console closed.
//
// The hub owns the transport and one paced typer, so what every client types is
// serialized and paced together. Output goes to every client.
package share

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ssimpson89/oob/internal/console"
)

var (
	// ErrPaused means the user paused the AI tool.
	ErrPaused = errors.New("the user paused the AI")
	// ErrTyping means the user is typing, so the AI tool must wait.
	ErrTyping = errors.New("the user is typing on this console")
	// ErrBusy means another operation has the console.
	ErrBusy = errors.New("the console is busy")
	// ErrStopped means the user took the keyboard back from the AI tool.
	ErrStopped = errStopped
	// ErrNoHub means nobody has the console open.
	ErrNoHub = errors.New("nobody has this console open")
	// ErrClosed means the console ended or this client left it.
	ErrClosed = errors.New("the shared console closed")
)

const (
	retryEvery = 250 * time.Millisecond
	// joinWait is how long a second opener waits for the first to finish
	// opening (an SSH login and VSP start take a few seconds).
	joinWait = 45 * time.Second
	writeMax = 16 << 10 // bytes per write message, well under maxMsg
)

// Options say which console to open and as what.
type Options struct {
	Host  string
	Agent bool   // an AI tool (oob mcp), not a window
	Name  string // the AI tool's name
	// Open opens the console when no one else has it. Nil means only join.
	Open func() (*console.Transport, error)
	// NoLinger ends the console when this client leaves, even if other windows
	// are on it. oob sol sets it: its process is about to exit.
	NoLinger bool
}

// Client is this process's side of a shared console.
type Client struct {
	opt   Options
	notes chan console.Note
	out   *outBuf
	doneC chan console.Result
	done  chan struct{}

	mu       sync.Mutex
	w        *wire
	link     net.Conn
	holder   *holder
	status   Status
	joined   bool
	pending  map[int]chan msg
	nextID   int
	lease    context.CancelCauseFunc
	progress func(pending, total int)
	err      error
	closed   bool
	ended    bool
	obsKind  string
	obsAt    bool
}

// holder is the hub, socket and lock of the process that opened the console.
type holder struct {
	hub *hub
	ln  net.Listener
	// lock is released when the hub ends.
	lock *os.File
}

// Exists reports whether another oob has a hub for host, by reaching its
// socket and hanging up (the hub ignores that). It takes no lock and never
// touches the console.
func Exists(host string) bool {
	sock, _, ok := paths(host)
	if !ok || checkSocket(sock) != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if err != nil {
		return false // nobody, or a socket left by an oob that died
	}
	conn.Close()
	return true
}

// Open joins the console for opt.Host if another oob has it open, or else
// opens it with opt.Open and shares it.
func Open(ctx context.Context, opt Options) (*Client, error) {
	c := &Client{
		opt: opt, notes: make(chan console.Note, 64), out: newOutBuf(),
		doneC: make(chan console.Result, 1), done: make(chan struct{}), pending: map[int]chan msg{},
	}
	sock, lockPath, ok := paths(opt.Host)
	if !ok {
		return c.openPrivately()
	}
	deadline := time.Now().Add(joinWait)
	for {
		f, err := lock(lockPath)
		switch {
		case err == nil:
			if opt.Open == nil {
				unlock(f)
				return nil, ErrNoHub
			}
			if err := c.host(f, sock); err != nil {
				return nil, err
			}
			return c, nil
		case errors.Is(err, errHeld):
			err := c.join(sock)
			if err == nil {
				return c, nil
			}
			if c.refused() || time.Now().After(deadline) {
				if !c.refused() {
					err = fmt.Errorf("this console is open in another oob that isn't answering: %w", err)
				}
				return nil, err
			}
		default:
			return c.openPrivately() // no lock to be had: just open it, unshared
		}
		select {
		case <-time.After(retryEvery):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// openPrivately opens the console with a hub no one else can reach, when it
// can't be shared here.
func (c *Client) openPrivately() (*Client, error) {
	if c.opt.Open == nil {
		return nil, ErrNoHub
	}
	if err := c.host(nil, ""); err != nil {
		return nil, err
	}
	return c, nil
}

// host opens the console and shares it, with this process as the first client.
func (c *Client) host(lockFile *os.File, sock string) error {
	tr, err := c.opt.Open()
	if err != nil {
		if lockFile != nil {
			unlock(lockFile)
		}
		return err
	}
	h := newHub(tr)
	ho := &holder{hub: h, lock: lockFile}
	if lockFile != nil {
		_ = os.Remove(sock) // left by an oob that died; ours now that we hold the lock
		ln, err := net.Listen("unix", sock)
		if err == nil {
			err = os.Chmod(sock, 0o600)
		}
		if err != nil { // holding the lock without listening would leave the others unable to join
			if ln != nil {
				ln.Close()
			}
			h.shutdown()
			unlock(lockFile)
			return fmt.Errorf("can't share this console: %w", err)
		}
		ho.ln = ln
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go h.serve(conn, false)
			}
		}()
	}
	h.onClose = func() {
		if ho.ln != nil {
			ho.ln.Close()
		}
		if ho.lock != nil {
			unlock(ho.lock)
		}
	}
	mine, theirs := net.Pipe()
	go h.serve(theirs, true)
	c.mu.Lock()
	c.holder = ho
	c.mu.Unlock()
	if err := c.hello(mine); err != nil {
		c.mu.Lock()
		c.holder = nil
		c.mu.Unlock()
		h.shutdown()
		return err
	}
	go c.run()
	return nil
}

func (c *Client) join(sock string) error {
	if err := checkSocket(sock); err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return err
	}
	if err := c.hello(conn); err != nil {
		return err
	}
	c.mu.Lock()
	c.joined = true
	c.mu.Unlock()
	go c.run()
	return nil
}

// hello introduces this client and waits for the console's state.
func (c *Client) hello(conn net.Conn) error {
	w := newWire(conn)
	if err := w.send(msg{T: tHello, Version: version, Agent: c.opt.Agent, Name: c.opt.Name, Replay: !c.opt.Agent}); err != nil {
		conn.Close()
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(helloTimeout))
	m, err := w.recv()
	_ = conn.SetReadDeadline(time.Time{})
	switch {
	case err != nil:
		conn.Close()
		return err
	case m.T == tBye:
		conn.Close()
		err := errors.New(m.Text)
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		return err
	case m.T != tState:
		conn.Close()
		return fmt.Errorf("unexpected first message %q", m.T)
	}
	c.mu.Lock()
	c.w, c.link = w, conn
	c.mu.Unlock()
	c.handle(m)
	return nil
}

func (c *Client) refused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err != nil
}

// run reads from the hub until the link drops, then ends the client.
func (c *Client) run() {
	for {
		c.mu.Lock()
		w := c.w
		c.mu.Unlock()
		m, err := w.recv()
		if err != nil {
			c.end(console.Result{Err: ErrClosed})
			return
		}
		c.handle(m)
	}
}

// end finishes the client: output ends, waiters fail, Done fires. Once.
func (c *Client) end(res console.Result) {
	c.mu.Lock()
	if c.ended {
		c.mu.Unlock()
		return
	}
	c.ended = true
	link := c.link
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	if c.lease != nil {
		c.lease(stopReason("disconnected"))
		c.lease = nil
	}
	c.mu.Unlock()
	c.out.close()
	c.doneC <- res
	if link != nil {
		link.Close()
	}
	c.notify(console.Note{})
}

func (c *Client) handle(m msg) {
	switch m.T {
	case tData:
		c.out.write(m.Data)
	case tState:
		c.mu.Lock()
		c.status = *m.State
		c.mu.Unlock()
		c.notify(console.Note{})
	case tNote, tMark:
		c.notify(console.Note{Text: m.Text, Mark: m.T == tMark})
	case tProg:
		c.mu.Lock()
		f := c.progress
		c.mu.Unlock()
		if f != nil {
			f(m.Pending, m.Total)
		}
	case tStopped:
		c.mu.Lock()
		if c.lease != nil {
			c.lease(stopReason(m.Text))
			c.lease = nil
		}
		c.mu.Unlock()
	case tEnd:
		var err error
		if m.Text != "" {
			err = errors.New(m.Text)
		}
		c.end(console.Result{Code: m.Code, Err: err})
	case tReply:
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

func (c *Client) notify(n console.Note) {
	select {
	case c.notes <- n:
	default:
	}
}

type stopReason string

func (r stopReason) Error() string { return string(r) }

func (c *Client) send(m msg) error {
	c.mu.Lock()
	w := c.w
	c.mu.Unlock()
	if w == nil {
		return ErrClosed
	}
	return w.send(m)
}

// call makes a request and waits for its reply, or for ctx.
func (c *Client) call(ctx context.Context, m msg) (msg, error) {
	ch := make(chan msg, 1)
	c.mu.Lock()
	if c.w == nil || c.ended {
		c.mu.Unlock()
		return msg{}, ErrClosed
	}
	c.nextID++
	m.T, m.ID = tReq, c.nextID
	c.pending[m.ID] = ch
	w := c.w
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, m.ID)
		c.mu.Unlock()
	}
	if err := w.send(m); err != nil {
		forget()
		return msg{}, err
	}
	select {
	case reply, ok := <-ch:
		switch {
		case !ok:
			return msg{}, ErrClosed
		case reply.Err != "":
			for _, e := range []error{ErrPaused, ErrTyping, ErrBusy, errStopped} {
				if reply.Err == e.Error() {
					return reply, e
				}
			}
			return reply, errors.New(reply.Err)
		}
		return reply, nil
	case <-ctx.Done():
		forget()
		return msg{}, ctx.Err()
	}
}

// Status is the console as everyone sees it.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Joined reports whether the console was already open in another oob.
func (c *Client) Joined() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.joined
}

// Err says why the hub turned this client away, if it did.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Tail is what ipmitool last printed, when this process opened the console.
func (c *Client) Tail() string {
	c.mu.Lock()
	h := c.holder
	c.mu.Unlock()
	if h != nil && h.hub.tr.Tail != nil {
		return h.hub.tr.Tail()
	}
	return ""
}

// Hosting reports whether this process opened the console.
func (c *Client) Hosting() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.holder != nil
}

// Done fires once when the console ends or this client is cut off.
func (c *Client) Done() <-chan console.Result { return c.doneC }

// Queue: where a window's typing goes (console.Queue).

// Enqueue queues typed bytes at the hub, behind everything already queued.
// It returns at once; the hub paces them.
func (c *Client) Enqueue(b []byte) <-chan struct{} {
	done := make(chan struct{})
	close(done)
	for n := 0; n < len(b); n += writeMax {
		if c.send(msg{T: tWrite, Data: b[n:min(len(b), n+writeMax)]}) != nil {
			break
		}
	}
	return done
}

// Urgent drops the queue and types b now.
func (c *Client) Urgent(b []byte) { _ = c.send(msg{T: tUrgent, Data: b}) }

// Clear drops what is queued at the hub.
func (c *Client) Clear() { _ = c.send(msg{T: tClear}) }

// OnProgress registers f for the hub's typing progress.
func (c *Client) OnProgress(f func(pending, total int)) {
	c.mu.Lock()
	c.progress = f
	c.mu.Unlock()
}

// window is a Client as the console bar sees it.
type window struct{ c *Client }

// Window returns the client as a console.Shared, for a window.
func (c *Client) Window() console.Shared { return window{c} }

func (w window) Notes() <-chan console.Note { return w.c.notes }
func (w window) Status() console.SharedStatus {
	s := w.c.Status()
	return console.SharedStatus{Windows: max(0, s.Windows-1), Agent: s.Agent, Working: s.Working, Paused: s.Paused}
}
func (w window) TogglePause() error {
	_, err := w.c.call(context.Background(), msg{Op: opPause})
	return err
}

// Notes delivers what the hub wants a window to show.
func (c *Client) Notes() <-chan console.Note { return c.notes }

// Type types b as the AI tool and returns once it is out. The hub refuses it
// unless the tool holds the keyboard (see Lease).
func (c *Client) Type(ctx context.Context, b []byte) error {
	_, err := c.call(ctx, msg{Op: opWrite, Data: b})
	return err
}

// Lease gives an AI tool the keyboard for one command. The returned context
// ends if the user stops it; why then says how (one of the Stop constants, or
// "disconnected"). If the user is typing it waits, up to typingWindow, for them
// to finish. Call release when done.
func (c *Client) Lease(ctx context.Context) (_ context.Context, why func() string, release func(), err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	// Ready for a stop before asking: the user may stop it the moment the hub
	// says yes, before the reply arrives.
	c.mu.Lock()
	c.lease = cancel
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		c.lease = nil
		c.mu.Unlock()
		cancel(nil)
	}
	giveUpAt := time.Now().Add(typingWindow)
	for {
		_, err = c.call(ctx, msg{Op: opLease})
		if !errors.Is(err, ErrTyping) || time.Now().After(giveUpAt) {
			break
		}
		select {
		case <-time.After(retryEvery):
		case <-ctx.Done():
			err = ctx.Err()
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		forget()
		return nil, nil, nil, err
	}
	why = func() string {
		var r stopReason
		if errors.As(context.Cause(ctx), &r) {
			return string(r)
		}
		return ""
	}
	return ctx, why, func() {
		forget()
		_, _ = c.call(context.Background(), msg{Op: opRelease})
	}, nil
}

// Break sends BREAK through the hub.
func (c *Client) Break(ctx context.Context) error {
	_, err := c.call(ctx, msg{Op: opBreak})
	return err
}

// Toggle switches between the host console and the BMC prompt.
func (c *Client) Toggle() (string, bool, error) {
	r, err := c.call(context.Background(), msg{Op: opToggle})
	return r.Kind, r.AtPrompt, err
}

// observe follows the hub's state as the transport's Observe does.
func (c *Client) observe([]byte) (string, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	changed := s.Kind != c.obsKind || s.AtPrompt != c.obsAt
	c.obsKind, c.obsAt = s.Kind, s.AtPrompt
	return s.Kind, s.AtPrompt, changed
}

// Close leaves the console. If this process opened it, the console closes with
// it, unless other windows are on it and the host may keep it for them (oob
// mcp), in which case it closes when the last window leaves.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.done)
	link, ho := c.link, c.holder
	c.mu.Unlock()
	if link != nil {
		link.Close()
	}
	if ho != nil && !ho.hub.hostLeft(c.opt.NoLinger) {
		ho.hub.shutdown()
	}
	c.end(console.Result{Err: ErrClosed})
	return nil
}

// Release ends the session at the BMC's end after Close, if this process
// closed the console. A lingering hub does it itself when it ends.
func (c *Client) Release() {
	c.mu.Lock()
	ho := c.holder
	c.mu.Unlock()
	if ho == nil {
		return
	}
	ho.hub.mu.Lock()
	linger := ho.hub.orphan && !ho.hub.closed
	ho.hub.mu.Unlock()
	if !linger {
		ho.hub.release()
	}
}

// outBuf is the console's output as a stream, filled by the link's reader
// without ever waiting for the consumer.
type outBuf struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

const outBufMax = 8 << 20

func newOutBuf() *outBuf {
	o := &outBuf{}
	o.cond = sync.NewCond(&o.mu)
	return o
}

func (o *outBuf) write(b []byte) {
	o.mu.Lock()
	o.buf = append(o.buf, b...)
	if over := len(o.buf) - outBufMax; over > 0 {
		o.buf = o.buf[over:]
	}
	o.mu.Unlock()
	o.cond.Broadcast()
}

func (o *outBuf) close() {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
	o.cond.Broadcast()
}

func (o *outBuf) Read(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for len(o.buf) == 0 {
		if o.closed {
			return 0, io.EOF
		}
		o.cond.Wait()
	}
	n := copy(p, o.buf)
	o.buf = o.buf[n:]
	return n, nil
}

// Transport returns the console as a transport, for code that works with one
// (the MCP session, the console bar). Its Link, Out and Done are this client's;
// Close leaves the console.
func (c *Client) Transport() *console.Transport {
	st := c.Status()
	var pacing console.Pacing
	char, line := time.Duration(st.CharNS), time.Duration(st.LineNS)
	pacing = console.Pacing{Char: &char, Line: &line}
	l := &link{c: c}
	t := &console.Transport{
		Kind: st.Kind, Link: l, Out: c.out, Done: c.doneC, Tail: c.Tail, Pacing: pacing,
		Close: func() { _ = c.Close() }, Release: c.Release,
	}
	if st.HasToggle {
		t.Toggle = c.Toggle
		t.Observe = c.observe
	}
	return t
}

// Link returns the client as a console.Link; see Transport.
func (c *Client) Link() console.Link { return c.Transport().Link }
