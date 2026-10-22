package console

import "io"

// Transport is a running serial console connection with no terminal
// attached: the link to type at, the remote's output, and how it ends. The
// console bar (oob sol) and headless front ends (the MCP server) both run on
// one, and the share hub owns one and hands the same shape to its clients.
type Transport struct {
	// Kind labels the console ("sol", "vsp", "com2").
	Kind string
	// Link sends keys and BREAK and ends the session.
	Link Link
	// Out is the remote's raw output. Read it until it ends.
	Out io.Reader
	// Done receives once when the transport has ended.
	Done <-chan Result
	// Tail returns the last bytes ipmitool printed, for failure messages (SOL
	// only; nil otherwise).
	Tail func() string
	// Toggle switches between the host console and the BMC's own CLI prompt,
	// and Observe lets the transport follow the output to keep that state
	// right. Both are nil where the console has no BMC prompt (IPMI SOL).
	Toggle  func() (kind string, atPrompt bool, err error)
	Observe func(b []byte) (kind string, atPrompt, changed bool)
	// Pacing is the host's typing delays for this console.
	Pacing Pacing
	// Close tears the transport down and releases everything. Safe to call
	// after the session ended.
	Close func()
	// Release, if set, ends the session at the BMC's end after Close, without
	// typing anything at the console (IPMI SOL: ipmitool sol deactivate).
	// It can take a while; callers bound their wait.
	Release func()
}

// Queue is where a window's typing goes. A local console types through its
// own paced typer; a console shared with other processes hands everything to
// the one typer at the hub, so input from every client is serialized.
type Queue interface {
	Enqueue(b []byte) <-chan struct{}
	Urgent(b []byte)
	Clear()
	// OnProgress registers f, told how many bytes are still queued and how
	// many the burst held in all.
	OnProgress(f func(pending, total int))
}

// Note is something the hub wants a window to show: a bar note, a line for
// history and the log (Mark), or with no text, just a change in who is
// attached to redraw for.
type Note struct {
	Text string
	Mark bool
}

// SharedStatus is who else is on a shared console.
type SharedStatus struct {
	Windows int    // other windows besides this one
	Agent   string // the AI tool's name, or ""
	Working bool   // the AI tool has the keyboard
	Paused  bool   // the user paused the AI tool
}

// Shared is a console other oob processes are on too, seen from one window.
type Shared interface {
	Notes() <-chan Note
	Status() SharedStatus
	// TogglePause pauses the AI tool, or lets it type again.
	TogglePause() error
}
