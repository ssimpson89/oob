// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// Adapted from bauddy (internal/mcpserver/guard.go): approval through MCP
// elicitation. The first call returns a result that has the client ask the
// user; the client repeats the call with their answer, which is honored once,
// for that same call only. Unlike bauddy there is no switch to skip the
// question: a client that can't ask is refused.

package mcpserver

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// target is what an approval is for: the host to name to the user, and where
// to note the answer in the session log (nil for none).
type target struct {
	name, addr, typ string // the host as it was when the user is asked
	mark            func(string)
}

func (t target) note(msg string) {
	if t.mark != nil {
		t.mark(msg)
	}
}

func (t target) label() string {
	if t.addr == "" {
		return t.name
	}
	return fmt.Sprintf("%s (%s)", t.name, t.addr)
}

// approvalID names the approval among a call's input requests.
const approvalID = "approve"

// askedMax bounds approvals awaiting an answer.
const askedMax = 16

// approve checks that the user approved doing what, which needs it for the
// reasons in whys. The model can't answer for them: the first time, approve
// returns a result that has the client ask the user, and the client then
// repeats the call with their answer, which is honored once, for that same
// call, text and host only. A client that can't ask is refused. A nil result
// and error means go ahead.
func (s *server) approve(req *mcp.CallToolRequest, dev target, what string, whys []string) (*mcp.CallToolResult, error) {
	call, err := callKey(req.Params)
	if err != nil {
		return nil, err
	}
	// The address and type are part of what is approved: the host list is read
	// again on every call, and an approval for one address must not carry over
	// to another.
	slices.Sort(whys)
	why := strings.Join(slices.Compact(whys), "; it ")
	// The reasons are part of what is approved too: the console can move on
	// between the question and the repeated call (to a bootloader, to the BMC's
	// prompt), and an approval for a command at a shell must not cover it there.
	call += "\x00" + dev.name + "\x00" + dev.addr + "\x00" + dev.typ + "\x00" + what + "\x00" + why

	s.askMu.Lock()
	defer s.askMu.Unlock()
	if state := req.Params.RequestState; state != "" {
		asked, ok := s.asked[state]
		delete(s.asked, state)
		if !ok || asked != call {
			return nil, fmt.Errorf("refused, so nothing was done: that approval has expired or was for a different call; call again to ask the user "+
				"(it needs approval because it %s)", why)
		}
		return nil, s.answered(req.Params.InputResponses[approvalID], dev, what, why)
	}

	if e := req.ClientCapabilities(); e == nil || e.Elicitation == nil || (e.Elicitation.Form == nil && e.Elicitation.URL != nil) {
		dev.note("needs approval, client can't ask (it " + why + "): " + visible(what))
		return nil, fmt.Errorf("refused, so nothing was done: this needs the user's approval, since it %s, and this MCP "+
			"client can't ask for it (it doesn't offer MCP elicitation). Tell the user what you wanted to do and why; "+
			"they can run it themselves with the oob command line", why)
	}
	if len(s.asked) >= askedMax {
		clear(s.asked) // calls the client never repeated
	}
	state := rand.Text()
	s.asked[state] = call
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{approvalID: &mcp.ElicitParams{
			Message: fmt.Sprintf("An AI agent wants to do this on %s:\n\n%s\n\nIt %s.", visible(dev.label()), visible(what), why),
			RequestedSchema: &jsonschema.Schema{
				Type: "object",
				// No fields: the prompt is just allow (accept) or deny (decline).
				Properties: map[string]*jsonschema.Schema{},
			},
		}},
		RequestState: state,
	}, nil
}

// visible makes s safe to show a person who is deciding whether to allow it:
// any character that doesn't print as itself (control characters, escape
// sequences, bidirectional overrides such as U+202E, zero-width and other
// invisible or non-ASCII space characters, invalid UTF-8) is written as a
// Go-style escape (\u202e, \x1b), so nothing can hide or reorder what the
// text says. Line breaks are kept, since the text may be several lines. The
// approval itself is still matched on the raw text.
func visible(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == utf8.RuneError && !strings.HasPrefix(s[i:], string(utf8.RuneError)):
			fmt.Fprintf(&b, "\\x%02x", s[i]) // an invalid byte, not a real U+FFFD
		case strconv.IsPrint(r):
			b.WriteRune(r)
		default:
			q := strconv.QuoteRune(r) // 'x' with the quotes
			b.WriteString(q[1 : len(q)-1])
		}
	}
	return b.String()
}

// callKey identifies a call by its tool and arguments, with the arguments
// re-encoded so that key order and spacing don't matter.
func callKey(p *mcp.CallToolParamsRaw) (string, error) {
	var args any
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return "", err
		}
	}
	b, err := json.Marshal(args)
	return p.Name + "\x00" + string(b), err
}

// answered turns the client's answer to the approval request into nil (go
// ahead) or the error to give the model. The answers are told apart so the
// model, and through it the user, isn't told the user refused when they were
// never asked: a client may not show the prompt, or may answer it by itself.
// why is what made the call need approval, so it can be named.
func (s *server) answered(resp mcp.InputResponse, dev target, what, why string) error {
	r, ok := resp.(*mcp.ElicitResult)
	if !ok || r == nil {
		dev.note("no answer to the approval request came back (it " + why + "): " + visible(what))
		return fmt.Errorf("no answer to the approval request came back from the client (it may not show these prompts), so nothing "+
			"was done. The call needed approval because it %s. Tell the user what you wanted to do; they can approve it through a "+
			"client that supports MCP elicitation, or run it with the oob command line", why)
	}
	switch {
	case r.Action == "accept":
		dev.note("user approved: " + visible(what))
		return nil
	case r.Action == "decline":
		dev.note("user declined (it " + why + "): " + visible(what))
		return fmt.Errorf("the user declined, so nothing was done (the call needed approval because it %s). Don't do it another way; "+
			"ask the user how they want to go on", why)
	case r.Action == "cancel":
		dev.note("approval dismissed (it " + why + "): " + visible(what))
		return fmt.Errorf("the approval prompt was dismissed without an answer, so nothing was done (the call needed approval because it %s). "+
			"Ask the user whether they want it done", why)
	}
	dev.note("approval answered with " + strconv.Quote(r.Action) + " (it " + why + "): " + visible(what))
	return fmt.Errorf("the approval prompt came back with an unexpected answer (%q), so nothing was done (the call needed approval "+
		"because it %s). Ask the user how they want to go on", r.Action, why)
}
