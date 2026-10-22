package mcpserver

import (
	"context"
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ssimpson89/oob/internal/bmc"
	"github.com/ssimpson89/oob/internal/config"
)

// The values the state-changing tools accept.
var (
	powerActions = []string{"on", "off", "cycle", "reset", "soft"}
	bootDevices  = []string{"pxe", "disk", "bios"}
	uidStates    = []string{"on", "off"}
	resetKinds   = []string{"warm", "cold"}
)

type powerIn struct {
	Host   string `json:"host" jsonschema:"the exact host name from list_hosts"`
	Action string `json:"action" jsonschema:"on, off (hard power off), cycle (off then on), reset (hard reset), or soft (graceful OS shutdown)"`
}

type bootIn struct {
	Host       string `json:"host" jsonschema:"the exact host name from list_hosts"`
	Device     string `json:"device" jsonschema:"the next boot device"`
	Legacy     bool   `json:"legacy,omitempty" jsonschema:"legacy (BIOS) boot instead of the default UEFI"`
	Persistent bool   `json:"persistent,omitempty" jsonschema:"apply to every future boot, not only the next"`
}

type uidIn struct {
	Host  string `json:"host" jsonschema:"the exact host name from list_hosts"`
	State string `json:"state" jsonschema:"on or off"`
}

type mcResetIn struct {
	Host string `json:"host" jsonschema:"the exact host name from list_hosts"`
	Kind string `json:"kind" jsonschema:"warm or cold"`
}

// addChangeTools registers the tools that change state. Every one but uid (an
// LED) asks the user first, and none can be told to skip it.
func (s *server) addChangeTools(srv *mcp.Server) {
	change := &mcp.ToolAnnotations{DestructiveHint: &yes, OpenWorldHint: &yes}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "power",
		Description: "Change a host's chassis power: on, off, cycle, reset or soft. Always asks the user to approve first; " +
			"refused if their client can't ask. Exact host name only. Use status to check the result.",
		InputSchema: schemaFor[powerIn](map[string][]string{"action": powerActions}),
		Annotations: change,
	}, s.power)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "boot",
		Description: "Set the host's next boot device (pxe, disk or bios), UEFI unless legacy. One-time unless " +
			"persistent. Always asks the user to approve first. Exact host name only.",
		InputSchema: schemaFor[bootIn](map[string][]string{"device": bootDevices}),
		Annotations: change,
	}, s.boot)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "uid",
		Description: "Turn the host's identify (UID) LED on or off. Always asks the user to approve first. Exact host name only.",
		InputSchema: schemaFor[uidIn](map[string][]string{"state": uidStates}),
		Annotations: change,
	}, s.uid)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "nmi",
		Description: "Send a non-maskable interrupt to the host, which can crash or panic its OS. Always asks the user " +
			"to approve first. Exact host name only.",
		Annotations: change,
	}, s.nmi)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "sel_clear",
		Description: "Erase the host's system event log. The entries can't be recovered. Always asks the user to " +
			"approve first. Exact host name only.",
		Annotations: change,
	}, s.selClear)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "mc_reset",
		Description: "Reset the host's BMC (warm or cold). The BMC drops off the network for a while. Always asks the " +
			"user to approve first. Exact host name only.",
		InputSchema: schemaFor[mcResetIn](map[string][]string{"kind": resetKinds}),
		Annotations: change,
	}, s.mcReset)
}

// change is the one path every state-changing tool takes: find the exact host,
// ask the user, and only then run fn against the host's driver with its
// output captured. what is shown to the user; why says why it needs asking.
func (s *server) change(ctx context.Context, req *mcp.CallToolRequest, host, what, why string, fn func(bmc.Driver) error) (*mcp.CallToolResult, any, error) {
	name, h, err := s.hostFor(host, true)
	if err != nil {
		return fail(err)
	}
	if res, err := s.approve(req, target{name: name, addr: h.IP, typ: h.Type}, what, []string{why}); res != nil || err != nil {
		if err != nil {
			return fail(err)
		}
		return res, nil, nil
	}
	return s.runChange(ctx, name, h, what, fn)
}

// runChange runs fn against the host's driver with its output captured.
func (s *server) runChange(ctx context.Context, name string, h config.Host, what string, fn func(bmc.Driver) error) (*mcp.CallToolResult, any, error) {
	out, err := s.runBMC(ctx, changeTimeout, func() error { return fn(s.be.Driver(name, h)) })
	if err != nil {
		return bmcResult(name, what, out, err, changeTimeout, false, true)
	}
	if out == "" {
		out = "done: " + what
	}
	return bmcResult(name, what, out, nil, changeTimeout, false, true)
}

func (s *server) power(ctx context.Context, req *mcp.CallToolRequest, in powerIn) (*mcp.CallToolResult, any, error) {
	if !slices.Contains(powerActions, in.Action) {
		return fail(fmt.Sprintf("action must be one of %v", powerActions))
	}
	why := map[string]string{
		"on":    "powers the host on",
		"off":   "cuts the host's power immediately, with no OS shutdown, which can lose unsaved work and corrupt data",
		"cycle": "cuts the host's power and turns it back on, with no OS shutdown, which can lose unsaved work and corrupt data",
		"reset": "hard resets the host, with no OS shutdown, which can lose unsaved work and corrupt data",
		"soft":  "asks the host's OS to shut down, ending everything running on it",
	}[in.Action]
	return s.change(ctx, req, in.Host, "power "+in.Action+" "+in.Host, why, func(d bmc.Driver) error { return d.Power(in.Action) })
}

func (s *server) boot(ctx context.Context, req *mcp.CallToolRequest, in bootIn) (*mcp.CallToolResult, any, error) {
	if !slices.Contains(bootDevices, in.Device) {
		return fail(fmt.Sprintf("device must be one of %v", bootDevices))
	}
	what := fmt.Sprintf("boot %s: next boot device %s", in.Host, in.Device)
	if in.Legacy {
		what += ", legacy (BIOS) mode"
	}
	if in.Persistent {
		what += ", for every future boot"
	}
	return s.change(ctx, req, in.Host, what, "changes where the host boots from, which can send it to the network or into firmware setup on its next boot",
		func(d bmc.Driver) error { return d.Boot(in.Device, in.Legacy, in.Persistent) })
}

func (s *server) uid(ctx context.Context, req *mcp.CallToolRequest, in uidIn) (*mcp.CallToolResult, any, error) {
	if !slices.Contains(uidStates, in.State) {
		return fail(fmt.Sprintf("state must be one of %v", uidStates))
	}
	name, h, err := s.hostFor(in.Host, true)
	if err != nil {
		return fail(err)
	}
	return s.runChange(ctx, name, h, "uid "+in.Host+" "+in.State, func(d bmc.Driver) error { return d.UID(in.State) })
}

func (s *server) nmi(ctx context.Context, req *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	return s.change(ctx, req, in.Host, "nmi "+in.Host, "sends a non-maskable interrupt, which can crash or panic the host's OS",
		func(d bmc.Driver) error { return d.NMI() })
}

func (s *server) selClear(ctx context.Context, req *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	return s.change(ctx, req, in.Host, "sel clear "+in.Host, "erases the system event log, which can't be recovered and may be the evidence for a hardware fault",
		func(d bmc.Driver) error { return d.SEL("clear") })
}

func (s *server) mcReset(ctx context.Context, req *mcp.CallToolRequest, in mcResetIn) (*mcp.CallToolResult, any, error) {
	if !slices.Contains(resetKinds, in.Kind) {
		return fail(fmt.Sprintf("kind must be one of %v", resetKinds))
	}
	return s.change(ctx, req, in.Host, in.Kind+" BMC reset "+in.Host, "resets the BMC, which drops its network, console and remote management for a while",
		func(d bmc.Driver) error { return d.MCReset(in.Kind) })
}
