package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// What came back for the approval request decides what the model is told: a
// client that never answered must not be reported as a user who refused.
func TestApprovalOutcomesAreTold(t *testing.T) {
	h := start(t, Options{}, "accept")
	s := h.srv
	caps := &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}}
	req := func(state string, resp mcp.InputResponseMap) *mcp.CallToolRequest {
		args, _ := json.Marshal(map[string]any{"host": "node01", "action": "cycle"})
		return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
			Meta: mcp.Meta{mcp.MetaKeyClientCapabilities: caps}, Name: "power", Arguments: args, RequestState: state,
			InputResponses: resp,
		}}
	}
	for _, tc := range []struct {
		name    string
		resp    mcp.InputResponseMap
		ok      bool
		says    string
		noSays  string
		noteHas string
	}{
		{"no responses at all", nil, false, "no answer to the approval request came back", "refused", "no answer"},
		{"empty responses", mcp.InputResponseMap{}, false, "may not show these prompts", "refused", "no answer"},
		{"nil entry", mcp.InputResponseMap{approvalID: (*mcp.ElicitResult)(nil)}, false, "no answer", "refused", "no answer"},
		{"answer for another id", mcp.InputResponseMap{"other": &mcp.ElicitResult{Action: "accept"}}, false, "no answer", "refused", "no answer"},
		{"declined", mcp.InputResponseMap{approvalID: &mcp.ElicitResult{Action: "decline"}}, false, "the user declined", "no answer", "declined"},
		{"cancelled", mcp.InputResponseMap{approvalID: &mcp.ElicitResult{Action: "cancel"}}, false, "dismissed", "the user declined", "dismissed"},
		{"accepted with no content", mcp.InputResponseMap{approvalID: &mcp.ElicitResult{Action: "accept"}}, true, "", "", "user approved"},
		{"accepted", mcp.InputResponseMap{approvalID: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"allow": true}}}, true, "", "", "user approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var notes []string
			dev := target{name: "node01", mark: func(m string) { notes = append(notes, m) }}
			asking, err := s.approve(req("", nil), dev, "power cycle node01", []string{whyRestart})
			if err != nil || asking == nil {
				t.Fatalf("first call should ask: %v %v", asking, err)
			}
			res, err := s.approve(req(asking.RequestState, tc.resp), dev, "power cycle node01", []string{whyRestart})
			if tc.ok {
				if res != nil || err != nil {
					t.Fatalf("should go ahead: %v %v", res, err)
				}
			} else {
				if err == nil {
					t.Fatal("should not go ahead")
				}
				msg := err.Error()
				if !strings.Contains(msg, tc.says) || (tc.noSays != "" && strings.Contains(msg, tc.noSays)) {
					t.Errorf("said %q, want %q and not %q", msg, tc.says, tc.noSays)
				}
				if !strings.Contains(msg, whyRestart) {
					t.Errorf("didn't say why it needed approval: %q", msg)
				}
			}
			if len(notes) != 1 || !strings.Contains(notes[0], tc.noteHas) {
				t.Errorf("session notes = %q, want one with %q", notes, tc.noteHas)
			}
		})
	}
}

// A client that can't ask is told so, with the reason, and the session notes it.
func TestApprovalWithoutElicitationSaysWhy(t *testing.T) {
	h := start(t, Options{}, "")
	var notes []string
	dev := target{name: "node01", mark: func(m string) { notes = append(notes, m) }}
	args, _ := json.Marshal(map[string]any{"host": "node01", "action": "cycle"})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "power", Arguments: args}}
	_, err := h.srv.approve(req, dev, "power cycle node01", []string{whyRestart})
	if err == nil || !strings.Contains(err.Error(), "can't ask") || !strings.Contains(err.Error(), whyRestart) {
		t.Fatalf("got %v", err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "client can't ask") {
		t.Errorf("notes = %q", notes)
	}
}
