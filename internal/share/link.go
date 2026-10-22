// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.

package share

import (
	"context"
	"errors"
	"time"

	"github.com/ssimpson89/oob/internal/console"
)

// link is the console.Link a client gives its caller: the keys act on the hub's
// real link.
type link struct {
	c *Client
}

type togglingLink struct{ *link }

func (l togglingLink) Toggle() (string, bool, error) { return l.c.Toggle() }
func (l togglingLink) Observe(b []byte) (string, bool, bool) {
	return l.c.observe(b)
}

var _ console.Link = (*link)(nil)

// Send types b like a window would. Windows use the Queue instead; this keeps
// the Link whole.
func (l *link) Send(b []byte) error {
	<-l.c.Enqueue(b)
	return nil
}

func (l *link) Break() error { return l.c.Break(context.Background()) }

// Disconnect ends the console if this process opened it; a joined window only
// leaves, so the console stays for the others.
func (l *link) Disconnect() error {
	if l.c.Hosting() {
		_, err := l.c.call(context.Background(), msg{Op: opDisconnect})
		if errors.Is(err, ErrClosed) {
			// Ending the console is what was asked: when it ends before the
			// hub's reply is read, that is success, not a failure.
			return nil
		}
		return err
	}
	l.c.end(console.Result{})
	return nil
}

func (l *link) Resize(rows, cols int) error {
	_, err := l.c.call(context.Background(), msg{Op: opResize, Rows: rows, Cols: cols})
	return err
}

func (l *link) FitSize(rows, cols int) error {
	_, err := l.c.call(context.Background(), msg{Op: opFit, Rows: rows, Cols: cols})
	return err
}

func (l *link) SizeIsFree() bool { return l.c.Status().SizeFree }

// Serial reports whether typing goes to a serial line, so it is paced.
func (l *link) Serial() bool { return l.c.Status().Serial }

// SizeLine is the command that fits the size, if the console types one.
func (l *link) SizeLine(rows, cols int) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := l.c.call(ctx, msg{Op: opSizeLine, Rows: rows, Cols: cols})
	return r.Text, err == nil && r.OK
}
