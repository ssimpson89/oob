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
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// target is what an approval is for: the host to name to the user, and where
// to note the answer in the session log (nil for none).
type target struct {
	name, addr string
	mark       func(string)
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
	call += "\x00" + dev.name + "\x00" + what

	s.askMu.Lock()
	defer s.askMu.Unlock()
	if state := req.Params.RequestState; state != "" {
		asked, ok := s.asked[state]
		delete(s.asked, state)
		if !ok || asked != call {
			return nil, errors.New("refused, so nothing was done: that approval has expired or was for a different call; call again to ask the user")
		}
		if r, ok := req.Params.InputResponses[approvalID].(*mcp.ElicitResult); ok && r.Action == "accept" && r.Content["allow"] == true {
			dev.note("user approved: " + what)
			return nil, nil
		}
		dev.note("user refused: " + what)
		return nil, errors.New("the user refused, so nothing was done. Don't do it another way; ask the user how they want to go on")
	}

	slices.Sort(whys)
	why := strings.Join(slices.Compact(whys), "; it ")
	if e := req.ClientCapabilities(); e == nil || e.Elicitation == nil || (e.Elicitation.Form == nil && e.Elicitation.URL != nil) {
		return nil, fmt.Errorf("refused, so nothing was done: this needs the user's approval, since it %s, and this MCP "+
			"client can't ask for it. Tell the user what you wanted to do and why; they can run it themselves with the oob command line", why)
	}
	if len(s.asked) >= askedMax {
		clear(s.asked) // calls the client never repeated
	}
	state := rand.Text()
	s.asked[state] = call
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{approvalID: &mcp.ElicitParams{
			Message: fmt.Sprintf("An AI agent wants to do this on %s:\n\n%s\n\nIt %s.", dev.label(), what, why),
			RequestedSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"allow": {Type: "boolean", Title: "Allow it", Default: json.RawMessage("false")},
				},
				Required: []string{"allow"},
			},
		}},
		RequestState: state,
	}, nil
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
