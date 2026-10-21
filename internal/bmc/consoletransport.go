package bmc

import (
	"io"

	"github.com/ssimpson89/oob/internal/console"
)

// ConsoleTransport is a running serial console connection with no terminal
// attached: the link to type at, the remote's output, and how it ends. The
// console bar (oob sol) and headless front ends (the MCP server) both run on
// one, so the transport setup lives in one place per vendor.
type ConsoleTransport struct {
	// Kind labels the console ("sol", "vsp", "com2").
	Kind string
	// Link sends keys and BREAK and ends the session.
	Link console.Link
	// Out is the remote's raw output. Read it until it ends.
	Out io.Reader
	// Done receives once when the transport has ended.
	Done <-chan console.Result
	// Tail returns the last bytes ipmitool printed, for failure messages (SOL
	// only; nil otherwise).
	Tail func() string
	// Toggle switches between the host console and the BMC's own CLI prompt,
	// and Observe lets the transport follow the output to keep that state
	// right. Both are nil where the console has no BMC prompt (IPMI SOL).
	Toggle  func() (kind string, atPrompt bool, err error)
	Observe func(b []byte) (kind string, atPrompt, changed bool)
	// Close tears the transport down and releases everything. Safe to call
	// after the session ended.
	Close func()
}

// OpenConsoleOptions are the inputs of Driver.OpenConsole.
type OpenConsoleOptions struct {
	// Rows and Cols are the window size the remote is told.
	Rows, Cols int
	// Force clears a stale IPMI SOL session first (the same as sol --force).
	Force bool
}
