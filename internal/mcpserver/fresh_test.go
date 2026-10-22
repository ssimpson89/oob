package mcpserver

import (
	"strings"
	"testing"
)

// A console just opened through a BMC prints its banner and no prompt. Enter
// and a read-only command there must not need approval: nothing risky can
// happen.
func TestFreshConsoleNeedsNoApproval(t *testing.T) {
	banner := "Virtual Serial Port Active: COM2\r\nStarting virtual serial port.\r\nPress 'ESC (' to return to the CLI Session.\r\n"
	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"enter", "send_keys", map[string]any{"keys": []string{"enter"}}},
		{"cat", "send_command", map[string]any{"line": "cat -A /etc/hostname"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, Options{MaxSessions: 4}, "")
			h.initial = &banner
			h.setup = func(f *fakeHost) { f.toggle = true }
			id := h.connect()
			tc.args["session"] = id
			out, isErr := h.call(tc.tool, tc.args)
			if isErr {
				t.Fatalf("%s needed approval on a fresh console: %s", tc.name, out)
			}
			if strings.Contains(h.askedAll(), "type at") {
				t.Fatalf("user was asked: %s", h.askedAll())
			}
		})
	}
}
