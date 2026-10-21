// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// Adapted from bauddy (internal/mcpserver/shared.go): when the console is open
// in an oob window too, the user is watching, and the AI tool takes the keyboard
// for each command. The user can pause it at any time.

package mcpserver

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ssimpson89/oob/internal/share"
)

// keyboard takes the keyboard for one tool call. The returned context ends if
// the user stops the tool; why then says how it was stopped. Call release when
// done. A console no one else can reach needs no lease.
func (c *session) keyboard(ctx context.Context) (_ context.Context, why func() string, release func(), err error) {
	if c.sh == nil {
		return ctx, func() string { return "" }, func() {}, nil
	}
	ctx, why, release, err = c.sh.Lease(ctx)
	if err != nil {
		return nil, nil, nil, shareError(err)
	}
	return ctx, why, release, nil
}

// pausedText is what the AI tool is told while the user has it paused.
const pausedText = "Paused by the user in oob, so nothing was sent. Tell the user what you want to do; " +
	"they can let you type again in their oob window (ctrl+] then i). Don't retry until they do."

// shareError says what a refusal from a shared console means for the AI tool.
func shareError(err error) error {
	switch {
	case errors.Is(err, share.ErrPaused):
		return errors.New(pausedText)
	case errors.Is(err, share.ErrTyping):
		return errors.New("The user is typing on this console in oob, so nothing was sent. Don't retry in a loop: " +
			"tell the user you're waiting, or ask them to finish.")
	case errors.Is(err, share.ErrBusy):
		return errors.New("The console is busy, so nothing was sent. Try again in a moment.")
	case errors.Is(err, share.ErrClosed):
		return errors.New("The shared console closed (the oob that opened it ended). Disconnect, then connect again.")
	}
	return err
}

// stoppedText tells the AI tool the user stopped it, and what that did.
func stoppedText(why string) string {
	what := map[string]string{
		share.StopCleared: "Your unsent command was erased; Enter wasn't pressed.",
		share.StopLeft:    "Your typed text is still on the line; Enter wasn't pressed.",
		share.StopRunning: "Your command was entered and may still be running.",
	}[why]
	if why == "disconnected" {
		return "The connection to the console dropped, so nothing more was sent. The line may be part-typed."
	}
	return strings.TrimSpace("The user stopped you in oob. "+what) +
		" You can't type until they let you (in their oob window: ctrl+] then i). Don't retry; tell the user and wait."
}

// knownClients names MCP clients that don't send a title.
var knownClients = map[string]string{"claude-code": "Claude Code", "codex": "Codex", "claude-ai": "Claude"}

// clientName is the AI tool's name as its MCP client reports it, made safe for
// the user's bar.
func clientName(req *mcp.CallToolRequest) string {
	name := ""
	if req != nil {
		if info := req.Session.InitializeParams(); info != nil && info.ClientInfo != nil {
			ci := info.ClientInfo
			name = cmp.Or(ci.Title, knownClients[ci.Name], ci.Name)
		}
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if r := []rune(strings.TrimSpace(name)); len(r) > 20 {
		name = string(r[:20])
	}
	return cmp.Or(strings.TrimSpace(name), "AI tool")
}
