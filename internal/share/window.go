// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.

package share

import "github.com/ssimpson89/oob/internal/console"

// RunWindow runs the console bar on c: a window on the shared console. The
// bar shows who else is on it, and the i key pauses the AI tool.
func RunWindow(c *Client, host string, power func() (string, error), connecting string) (int, error) {
	t := c.Transport()
	var l console.Link = t.Link
	if t.Toggle != nil {
		l = togglingLink{t.Link.(*link)}
	}
	st := c.Status()
	return console.Run(console.Setup{
		Host: host, Kind: t.Kind, Link: l, Out: t.Out, Done: t.Done,
		Power: power, Pacing: t.Pacing, Connecting: connecting, AtPrompt: st.AtPrompt,
		Queue: c, Shared: c.Window(),
	})
}
