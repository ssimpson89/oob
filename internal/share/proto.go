// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// Adapted from bauddy (internal/share/proto.go): the wire format, reworked for
// oob's console transports.

package share

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
)

// version is the protocol version. An oob speaking another version can't join.
const version = 1

// maxMsg caps one message, so a confused peer can't make us buffer without
// limit. The largest is the replay: 64 KB of output, base64.
const maxMsg = 256 << 10

// Message types.
const (
	tHello   = "hello"   // client to hub, first: who it is
	tData    = "data"    // hub to client: console output
	tState   = "state"   // hub to client: Status changed
	tNote    = "note"    // hub to windows: something for the bar
	tMark    = "mark"    // hub to windows: a line for history and the log
	tStopped = "stopped" // hub to agent: the user stopped it
	tProg    = "prog"    // hub to windows: typing queue progress
	tEnd     = "end"     // hub to client: the console ended
	tWrite   = "write"   // window to hub: typed bytes, queued behind the rest
	tUrgent  = "urgent"  // window to hub: bytes that jump the queue (ctrl+c)
	tClear   = "clear"   // window to hub: drop what is queued
	tReq     = "req"     // client to hub: an operation, answered by tReply
	tReply   = "reply"   // hub to client
	tBye     = "bye"     // hub to client: refused; Text says why
)

// Operations a client can ask for.
const (
	opWrite      = "write"      // agent: type these bytes, reply when they are out
	opLease      = "lease"      // agent: take the keyboard
	opRelease    = "release"    // agent: give it back
	opPause      = "pause"      // window: pause the agent, or let it type again
	opBreak      = "break"      // send BREAK
	opToggle     = "toggle"     // switch between the host console and the BMC prompt
	opDisconnect = "disconnect" // host window: end the console
	opResize     = "resize"     // host window: the terminal changed size
	opFit        = "fit"        // host window: make the remote agree with the size
	opSizeLine   = "sizeline"   // the command that fits the size, if it is typed
)

// msg is one message on the socket: a line of JSON. Only the fields its type
// uses are set.
type msg struct {
	T string `json:"t"`

	Version int    `json:"v,omitempty"`      // hello
	Agent   bool   `json:"agent,omitempty"`  // hello: an AI tool, not a window
	Name    string `json:"name,omitempty"`   // hello: the AI tool's name
	Replay  bool   `json:"replay,omitempty"` // hello: send recent output; data: this is it

	Data  []byte  `json:"data,omitempty"`  // data, write, urgent, req write
	Text  string  `json:"text,omitempty"`  // note, mark, stopped, bye, end, reply
	State *Status `json:"state,omitempty"` // state

	ID       int    `json:"id,omitempty"` // req, reply
	Op       string `json:"op,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	Cols     int    `json:"cols,omitempty"`
	Err      string `json:"err,omitempty"`
	Kind     string `json:"kind,omitempty"` // reply to toggle
	AtPrompt bool   `json:"atPrompt,omitempty"`
	OK       bool   `json:"ok,omitempty"` // reply to sizeline
	Code     int    `json:"code,omitempty"`
	Pending  int    `json:"pending,omitempty"` // prog
	Total    int    `json:"total,omitempty"`
}

// Status is a shared console as everyone sees it.
type Status struct {
	Kind      string `json:"kind"`     // "sol", "vsp", "com2", or the BMC prompt's label
	AtPrompt  bool   `json:"atPrompt"` // at the BMC's own prompt, not the host console
	Serial    bool   `json:"serial"`   // what is typed goes to a serial line, so it is paced
	SizeFree  bool   `json:"sizeFree"` // resizing types nothing (ssh window change)
	HasToggle bool   `json:"hasToggle"`
	CharNS    int64  `json:"charNS"` // the hub's typing delays
	LineNS    int64  `json:"lineNS"`
	Windows   int    `json:"windows"` // windows on it, the receiver's included
	Agent     string `json:"agent"`   // the connected AI tool's name, or ""
	Working   bool   `json:"working"` // the AI tool has the keyboard
	Paused    bool   `json:"paused"`  // the user paused the AI tool
}

// wire reads and writes messages on one connection. Writes may come from
// several goroutines.
type wire struct {
	r  *bufio.Reader
	wm sync.Mutex
	w  io.Writer
}

func newWire(rw io.ReadWriter) *wire {
	return &wire{r: bufio.NewReaderSize(rw, maxMsg), w: rw}
}

func (c *wire) send(m msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wm.Lock()
	defer c.wm.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// recv reads the next message. A line longer than maxMsg is an error.
func (c *wire) recv() (msg, error) {
	var m msg
	line, err := c.r.ReadSlice('\n')
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(line, &m)
}
