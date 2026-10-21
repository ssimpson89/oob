package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 4340518873: an approval is bound to the address and type it was asked about,
// so reloading the host file between ask and retry can't move it to a new host.
func TestApprovalIsBoundToAddressAndType(t *testing.T) {
	h := start(t, Options{}, "accept")
	s := h.srv
	caps := &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}}
	req := func(state string) *mcp.CallToolRequest {
		args, _ := json.Marshal(map[string]any{"host": "node01", "action": "cycle"})
		return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
			Meta: mcp.Meta{mcp.MetaKeyClientCapabilities: caps}, Name: "power", Arguments: args, RequestState: state,
			InputResponses: mcp.InputResponseMap{approvalID: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"allow": true}}},
		}}
	}
	why := []string{whyRestart}
	old := target{name: "node01", addr: "192.0.2.11", typ: "ipmi"}
	for _, moved := range []target{
		{name: "node01", addr: "192.0.2.99", typ: "ipmi"},
		{name: "node01", addr: "192.0.2.11", typ: "ilo"},
	} {
		asking, err := s.approve(req(""), old, "power cycle node01", why)
		if err != nil || asking == nil {
			t.Fatalf("first call should ask: %v %v", asking, err)
		}
		if res, err := s.approve(req(asking.RequestState), moved, "power cycle node01", why); err == nil || res != nil {
			t.Errorf("an approval for %+v was accepted for %+v", old, moved)
		}
	}
	asking, _ := s.approve(req(""), old, "power cycle node01", why)
	if res, err := s.approve(req(asking.RequestState), old, "power cycle node01", why); res != nil || err != nil {
		t.Errorf("the unchanged host should go ahead: %v %v", res, err)
	}
}

// 4340518873, console side: the session keeps the address it connected to, so
// the user is shown it even when the host file changes afterwards.
func TestSessionApprovalShowsTheConnectedAddress(t *testing.T) {
	h := start(t, Options{}, "decline")
	h.connect()
	moved := h.cfg.Hosts["node01"]
	moved.IP = "192.0.2.99"
	h.cfg.Hosts["node01"] = moved
	h.send("s1", "reboot")
	if asked := h.askedAll(); !strings.Contains(asked, "192.0.2.11") || strings.Contains(asked, "192.0.2.99") {
		t.Errorf("the user was shown %q", asked)
	}
}

// 4340518934
func TestAliasKeysAreNotOffered(t *testing.T) {
	h := start(t, Options{}, "accept")
	h.connect()
	for _, k := range []string{"ctrl+m", "ctrl+j", "ctrl+o", "ctrl+i"} {
		if _, ok := keyBytes[k]; ok {
			t.Errorf("%s is in keyBytes", k)
		}
		before := h.host("node01").rawSent()
		if out, isErr := h.keys("s1", "reboot", k); !isErr {
			t.Errorf("%s was accepted: %q", k, out)
		}
		if h.host("node01").rawSent() != before {
			t.Errorf("%s reached the host", k)
		}
	}
	if _, ok := keyBytes["ctrl+u"]; !ok {
		t.Error("ctrl+u is gone")
	}
	if strings.Contains(h.askedAll(), "reboot") {
		t.Error("the user was asked about a key that should have been rejected up front")
	}
}

// 4340519114: only exact read-only forms of the partitioning tools skip approval.
func TestListOnlyIsExact(t *testing.T) {
	for _, l := range []string{
		"fdisk -l", "fdisk -l /dev/sda", "sfdisk --list", "sfdisk -l /dev/sda", "gdisk -l /dev/sda", "sgdisk -p /dev/sda",
		"sgdisk --print /dev/sda", "parted -l", "parted /dev/sda print", "parted -s /dev/sda print", "parted -s /dev/sda unit gb print free",
	} {
		if why := risk(l); why != "" {
			t.Errorf("%q flagged (%s)", l, why)
		}
	}
	for _, l := range []string{
		"sgdisk --zap-all --print /dev/sda", "sgdisk -p --zap-all /dev/sda", "sgdisk -p /dev/sda -o", "sgdisk -l backup.gpt /dev/sda",
		"parted -s /dev/sda rm 1 print", "parted -s /dev/sda mklabel gpt print", "parted /dev/sda print mklabel gpt",
		"parted /dev/sda print; parted /dev/sda rm 1", "fdisk -l; fdisk /dev/sda", "fdisk -l /dev/sda > /dev/sdb",
		"sfdisk -l --delete /dev/sda 1", "sfdisk /dev/sda < layout", "gdisk /dev/sda", "gdisk -l /dev/sda -x", "fdisk /dev/sda",
		"parted -s /dev/sda print resizepart 1 100%", "fdisk -l /dev/sda $(fdisk /dev/sdb)",
	} {
		if risk(l) == "" {
			t.Errorf("%q was exempted", l)
		}
	}
}

// 4340519156: any key that edits or recalls the line leaves it unknown, so the
// next Enter needs approval, also within one batch of keys.
func TestEditingKeysMakeTheLineUnknown(t *testing.T) {
	for _, k := range []string{"home", "end", "delete", "left", "right", "up", "down", "tab", "ctrl+a", "ctrl+e", "ctrl+k",
		"ctrl+w", "ctrl+y", "ctrl+p", "ctrl+n", "ctrl+r", "ctrl+h", "esc"} {
		var ty typing
		ty.add("ls", "", 1)
		ty.add("", k, 1)
		if _, unknown := ty.pending(1); !unknown {
			t.Errorf("%s left the line known", k)
		}
		ty.add("", "ctrl+u", 1)
		if text, unknown := ty.pending(1); unknown || text != "" {
			t.Errorf("ctrl+u after %s left %q %v", k, text, unknown)
		}
	}
	var ty typing
	ty.add("", "left", 1)
	if _, unknown := ty.pending(2); unknown {
		t.Error("a new prompt didn't clear it")
	}
}

func TestEnterAfterAnEditingKeyNeedsApproval(t *testing.T) {
	for _, k := range []string{"home", "ctrl+a", "ctrl+w", "left", "ctrl+r"} {
		// Across calls, with no way to ask: refused, and nothing is entered.
		h := start(t, Options{}, "")
		h.connect()
		if out, isErr := h.keys("s1", "", k); isErr {
			t.Fatalf("%s: %q", k, out)
		}
		if out, isErr := h.keys("s1", "", "enter"); !isErr || !strings.Contains(out, "approval") {
			t.Errorf("%s then enter: %q %v", k, out, isErr)
		}
		if got := h.host("node01").entered(); len(got) != 0 {
			t.Errorf("%s then enter entered %q", k, got)
		}
		// Cleared with ctrl+u, enter goes through.
		h.keys("s1", "", "ctrl+u")
		if out, isErr := h.keys("s1", "", "enter"); isErr {
			t.Errorf("%s, ctrl+u, enter: %q", k, out)
		}
		// Within one batch.
		h = start(t, Options{}, "")
		h.connect()
		if out, isErr := h.keys("s1", "uname -r", k, "enter"); !isErr || !strings.Contains(out, "approval") {
			t.Errorf("batch %s: %q %v", k, out, isErr)
		}
		if got := h.host("node01").entered(); len(got) != 0 {
			t.Errorf("batch %s entered %q", k, got)
		}
	}
	// send_command refuses to run over an edited line, too.
	h := start(t, Options{}, "")
	h.connect()
	h.keys("s1", "", "home")
	if out, isErr := h.send("s1", "uname -r"); !isErr || !strings.Contains(out, "nothing was sent") {
		t.Errorf("send_command over an edited line: %q %v", out, isErr)
	}
	if got := h.host("node01").entered(); len(got) != 0 {
		t.Errorf("send_command entered %q", got)
	}
}
