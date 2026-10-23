// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.

package share

import (
	"context"
	"os"

	"golang.org/x/term"

	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
)

// Console says how RunConsole opens a console and what goes with the bar.
type Console struct {
	// Open starts the console when no other oob has it, sized for the terminal.
	// Nil means only join: ErrNoHub if nobody has it.
	Open  func(rows, cols int) (*console.Transport, error)
	Power func() (string, error)
	// Connecting is shown on the bar until the console first outputs. It is not
	// shown when this joins a console that is already open.
	Connecting string
	// Failed runs when the window ended with a non-zero code and this oob opened
	// the console, once the terminal is restored and while the console is up.
	Failed func(c *Client)
}

// RunConsole is a window on h's console: it joins the console another oob has
// open, or opens it and shares it, and runs the console bar until it ends. The
// bar shows who else is on it, and the i key pauses the AI tool.
func RunConsole(h config.Host, name string, o Console) (int, error) {
	w, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 1, err
	}
	opt := Options{Host: name, NoLinger: true}
	opt.Addr, opt.Type = Endpoint(h)
	if o.Open != nil {
		opt.Open = func() (*console.Transport, error) { return o.Open(console.RemoteRows(rows), w) }
	}
	c, err := Open(context.Background(), opt)
	if err != nil {
		return 1, err
	}
	defer c.Release() // after Close: ends the BMC's session if this oob opened it
	defer c.Close()
	connecting := o.Connecting
	if c.Joined() {
		connecting = ""
	}
	t := c.Transport()
	var l console.Link = t.Link
	if t.Toggle != nil {
		l = togglingLink{t.Link.(*link)}
	}
	code, err := console.Run(console.Setup{
		Host: name, Kind: t.Kind, Link: l, Out: t.Out, Done: t.Done,
		Power: o.Power, Pacing: t.Pacing, Connecting: connecting, AtPrompt: c.Status().AtPrompt,
		Queue: c, Shared: c.Window(), SizeOnce: c.Hosting(),
	})
	if code != 0 && o.Failed != nil && c.Hosting() {
		o.Failed(c)
	}
	return code, err
}
