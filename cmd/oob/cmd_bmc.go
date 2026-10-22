package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/bmc"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/share"
)

// destructiveActions is the one list of commands that change or erase state
// and so ask first. withHostD uses it to refuse --yes on a prefix match, and
// each command asks its own prompt from the same row (confirmDestructive).
var destructiveActions = []struct {
	cmd    string
	match  func(rest []string) bool
	prompt func(name string, h config.Host, rest []string) string
}{
	{"power", func(rest []string) bool {
		switch argOr(rest, "status") {
		case "off", "cycle", "reset", "soft":
			return true
		}
		return false
	}, func(name string, h config.Host, rest []string) string {
		return fmt.Sprintf("Power %s %s (%s)?", rest[0], name, h.IP)
	}},
	{"sel", func(rest []string) bool { return argOr(rest, "list") == "clear" },
		func(name string, _ config.Host, _ []string) string { return "Clear the SEL on " + name + "?" }},
	{"mcreset", func([]string) bool { return true }, func(name string, _ config.Host, rest []string) string {
		return fmt.Sprintf("%s reset BMC on %s?", argOr(rest, "cold"), name)
	}},
	{"nmi", func([]string) bool { return true }, func(name string, h config.Host, _ []string) string {
		return fmt.Sprintf("Send an NMI to %s (%s)? This can crash or panic the host OS.", name, h.IP)
	}},
	{"ipmi", func(rest []string) bool { _, risky := riskyIpmiArgs(rest); return risky },
		func(name string, h config.Host, rest []string) string {
			what, _ := riskyIpmiArgs(rest)
			return fmt.Sprintf("Run `ipmitool %s` on %s (%s)? This is a %s command.", strings.Join(rest, " "), name, h.IP, what)
		}},
}

// isDestructive reports whether cmd with these arguments is in the table.
func isDestructive(cmd string, rest []string) bool {
	for _, a := range destructiveActions {
		if a.cmd == cmd && a.match(rest) {
			return true
		}
	}
	return false
}

// confirmDestructive asks the table's prompt for cmd (honoring --yes) and does
// nothing when the arguments are not destructive.
func confirmDestructive(cmd, name string, h config.Host, rest []string) error {
	for _, a := range destructiveActions {
		if a.cmd == cmd && a.match(rest) {
			return confirm(a.prompt(name, h, rest))
		}
	}
	return nil
}

// takeConsole is for --raw, --force and --deactivate, which act on the BMC's
// session directly and would take the console from whoever has it. It takes the
// same ownership lock a sharing oob holds, so the check and the operation can't
// be split by another oob opening the console in between: release it when the
// operation is over. If another oob has the console, it says so.
func takeConsole(name string, h config.Host) (release func(), err error) {
	addr, _ := share.Endpoint(h)
	release, err = share.Exclusive(name, addr)
	if errors.Is(err, share.ErrShared) {
		return nil, fmt.Errorf("%s's console is open in another oob and shared; --raw, --force and --deactivate would take it from them. Run `%s sol %s` to join it", name, app.Name, name)
	}
	return release, err
}

// joinSharedConsole joins the console another oob has open for name, as a
// window. joined is false when nobody has it, and the caller opens it itself.
func joinSharedConsole(name string, h config.Host, o bmc.SOLOptions, drv bmc.Driver) (joined bool, err error) {
	if !console.UseBar() {
		return false, nil
	}
	code, err := share.RunConsole(h, name, share.Console{Power: drv.ConsolePower()})
	if errors.Is(err, share.ErrNoHub) {
		return false, nil
	}
	return true, app.Code(code, err)
}

// bmcCmds returns the commands that act on a single BMC (over IPMI, SSH or
// Redfish, depending on its driver).
func bmcCmds() []*cobra.Command {
	var cmds []*cobra.Command

	// sol
	var o bmc.SOLOptions
	var charDelay, lineDelay time.Duration
	var sol *cobra.Command
	sol = &cobra.Command{
		Use:   "sol <name>",
		Short: "The server's own console: login prompt, GRUB, BIOS (ctrl+] for keys)",
		Long: "The server's own serial console (the machine itself, not the BMC): its login prompt,\n" +
			"GRUB, BIOS and kernel messages. Works even when the server's network or OS is down;\n" +
			"when the OS is healthy, plain ssh to the server is usually nicer. You log in with the\n" +
			"server's own user and password; SSH keys only cover the hop to the BMC.\n\n" +
			"Press ctrl+] for the key menu (size, log, BREAK, SysRq,\n" +
			"power cycle, disconnect). Generic IPMI hosts use Serial-over-LAN through ipmitool. An HPE iLO host\n" +
			"(type ilo) opens the iLO CLI over SSH and starts VSP, a Dell iDRAC host (type idrac, untested) starts\n" +
			"`console com2`; disconnecting leaves the console and logs out. Neither has BREAK.\n" +
			"A Supermicro host (type supermicro, untested) uses ipmitool, clearing any stale session first;\n" +
			"--ssh uses its SMASH-CLP shell instead.\n" +
			"Everything you type or paste goes through one queue, sent a byte at a time with a short gap\n" +
			"(--char-delay) and a longer one after Enter (--line-delay), so the BMC's small buffer never\n" +
			"overruns. Set 0 for no delay; the host's char_delay_ms and line_delay_ms set them per host.\n" +
			"Like ssh, ~. at the start of a line also disconnects and ~~ sends a single ~.\n" +
			"Inside an outer ssh session the outer ssh eats the first tilde, so type ~~. to reach oob.\n" +
			"With --raw, or when stdout is not a terminal, ipmitool runs directly and its own ~. applies.\n\n" +
			"The console is shared: if another oob (a second window, or `oob mcp` for an AI agent) already has\n" +
			"it open, this joins it instead of opening a second connection. ~. then leaves only this window.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: hostCompletion,
		RunE: withHost(func(name string, h config.Host, _ []string) error {
			if sol.Flags().Changed("char-delay") {
				h.CharDelayMS = ptrMS(charDelay)
			}
			if sol.Flags().Changed("line-delay") {
				h.LineDelayMS = ptrMS(lineDelay)
			}
			drv := bmc.DriverForProbed(name, h)
			if o.Raw || o.Deact || o.Force {
				release, err := takeConsole(name, h)
				if err != nil {
					return err
				}
				defer release() // held through the operation
				return drv.Console(o)
			}
			if joined, err := joinSharedConsole(name, h, o, drv); joined {
				return err
			}
			return drv.Console(o)
		}),
	}
	sol.Flags().DurationVar(&charDelay, "char-delay", console.DefaultCharDelay, "gap after each typed byte on a serial console (0 for none)")
	sol.Flags().DurationVar(&lineDelay, "line-delay", console.DefaultLineDelay, "gap after Enter on a serial console (0 for none)")
	sol.Flags().BoolVar(&o.Force, "force", false, "deactivate any stale SOL session first")
	sol.Flags().BoolVar(&o.Deact, "deactivate", false, "only deactivate the SOL session")
	sol.Flags().BoolVar(&o.Raw, "raw", false, "plain ipmitool passthrough: no bottom bar, ~. disconnects")
	sol.Flags().BoolVar(&o.SSH, "ssh", false, "Supermicro only: use the BMC's SSH shell instead of ipmitool")
	cmds = append(cmds, sol)

	// power
	cmds = append(cmds, &cobra.Command{
		Use:               "power <name> [status|on|off|cycle|reset|soft]",
		Short:             "Chassis power control (default: status)",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: argsThen([]string{"status", "on", "off", "cycle", "reset", "soft"}),
		RunE: withHostD("power", func(name string, h config.Host, rest []string) error {
			act := argOr(rest, "status")
			switch act {
			case "status", "on", "off", "cycle", "reset", "soft":
			default:
				return bmc.UnknownPowerError(act)
			}
			if err := confirmDestructive("power", name, h, rest); err != nil {
				return err
			}
			return bmc.DriverForProbed(name, h).Power(act)
		}),
	})

	// boot
	var legacy, persistent bool
	boot := &cobra.Command{
		Use:               "boot <name> <pxe|disk|bios>",
		Short:             "Set next boot device (UEFI by default; --legacy for BIOS boot)",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: argsThen([]string{"pxe", "disk", "bios"}),
		RunE: withHost(func(name string, h config.Host, rest []string) error {
			switch rest[0] {
			case "pxe", "disk", "bios":
			default:
				return errors.New("boot device must be pxe, disk or bios")
			}
			return bmc.DriverForProbed(name, h).Boot(rest[0], legacy, persistent)
		}),
	}
	boot.Flags().BoolVar(&legacy, "legacy", false, "legacy (BIOS) boot instead of the default UEFI")
	boot.Flags().BoolVar(&persistent, "persistent", false, "apply to all future boots")
	cmds = append(cmds, boot)

	// uid
	cmds = append(cmds, &cobra.Command{
		Use:               "uid <name> [on|off]",
		Short:             "Identify (UID) LED: show (iLO and iDRAC only), turn on or off",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: argsThen([]string{"on", "off"}),
		RunE: withHost(func(name string, h config.Host, rest []string) error {
			return bmc.DriverForProbed(name, h).UID(argOr(rest, ""))
		}),
	})

	// nmi
	cmds = append(cmds, &cobra.Command{
		Use:               "nmi <name>",
		Short:             "Send an NMI to the host (can crash or panic the OS)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: hostCompletion,
		RunE: withHostD("nmi", func(name string, h config.Host, rest []string) error {
			if err := confirmDestructive("nmi", name, h, rest); err != nil {
				return err
			}
			return bmc.DriverForProbed(name, h).NMI()
		}),
	})

	// sel
	cmds = append(cmds, &cobra.Command{
		Use:               "sel <name> [list|clear]",
		Short:             "System event log (default: list)",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: argsThen([]string{"list", "clear"}),
		RunE: withHostD("sel", func(name string, h config.Host, rest []string) error {
			act := argOr(rest, "list")
			switch act {
			case "list", "clear":
			default:
				return fmt.Errorf("unknown sel action %q", act)
			}
			if err := confirmDestructive("sel", name, h, rest); err != nil {
				return err
			}
			return bmc.DriverForProbed(name, h).SEL(act)
		}),
	})

	simple := func(use, short string, ipmiArgs ...string) *cobra.Command {
		return &cobra.Command{
			Use: use + " <name>", Short: short, Args: cobra.ExactArgs(1), ValidArgsFunction: hostCompletion,
			RunE: withHost(func(_ string, h config.Host, _ []string) error { return app.Code(bmc.RunIPMI(h, ipmiArgs...)) }),
		}
	}
	cmds = append(cmds,
		simple("sensors", "Sensor readings (ipmitool sensor)", "sensor"),
		simple("sdr", "Sensor data repository (ipmitool sdr)", "sdr"),
		simple("fru", "FRU inventory", "fru"),
		simple("info", "BMC info (mc info)", "mc", "info"),
	)
	cmds = append(cmds, &cobra.Command{
		Use: "mcreset <name> [warm|cold]", Short: "Reset the BMC (default: cold)",
		Args: cobra.RangeArgs(1, 2), ValidArgsFunction: argsThen([]string{"warm", "cold"}),
		RunE: withHostD("mcreset", func(name string, h config.Host, rest []string) error {
			kind := argOr(rest, "cold")
			if kind != "warm" && kind != "cold" {
				return errors.New("mcreset takes warm or cold")
			}
			if err := confirmDestructive("mcreset", name, h, rest); err != nil {
				return err
			}
			return bmc.DriverForProbed(name, h).MCReset(kind)
		}),
	})
	cmds = append(cmds, &cobra.Command{
		Use: "ipmi <name> -- <ipmitool args...>", Short: "Run raw ipmitool args against a host",
		Long: "Run ipmitool with the host's address and credentials. It asks for confirmation (unless -y) when the\n" +
			"arguments include chassis power off|cycle|reset|soft|diag, mc reset, sel clear or raw. That list is\n" +
			"best effort, not a guarantee: other commands can also change or erase state.",
		Args: cobra.MinimumNArgs(2), ValidArgsFunction: hostCompletion,
		RunE: withHostD("ipmi", func(name string, h config.Host, rest []string) error {
			if err := confirmDestructive("ipmi", name, h, rest); err != nil {
				return err
			}
			return app.Code(bmc.RunIPMI(h, rest...))
		}),
	})

	cmds = append(cmds, &cobra.Command{
		Use: "web <name>", Short: "Open the BMC web UI", Args: cobra.ExactArgs(1), ValidArgsFunction: hostCompletion,
		RunE: withHost(func(_ string, h config.Host, _ []string) error {
			opener := "xdg-open"
			if runtime.GOOS == "darwin" {
				opener = "open"
			}
			url := webURL(h.IP)
			bin, err := exec.LookPath(opener)
			if err != nil {
				fmt.Println(url)
				return nil
			}
			fmt.Fprintf(os.Stderr, "opening %s\n", url)
			return exec.Command(bin, url).Start()
		}),
	})
	return cmds
}

// webURL is the BMC's https URL, with an IPv6 address in brackets.
func webURL(ip string) string {
	if strings.Contains(ip, ":") && !strings.HasPrefix(ip, "[") {
		ip = "[" + ip + "]"
	}
	return "https://" + ip + "/"
}

// riskyIpmiArgs reports whether passthrough ipmitool arguments contain a
// command that powers the host off or resets something, and names it. It is a
// best-effort list: chassis power off|cycle|reset|soft|diag (and the `power`
// alias), mc reset (and `bmc reset`), sel clear, and raw.
func riskyIpmiArgs(args []string) (string, bool) {
	var t []string
	for _, a := range args {
		t = append(t, strings.Fields(strings.ToLower(a))...)
	}
	for i, w := range t {
		next := func(k int) string {
			if i+k < len(t) {
				return t[i+k]
			}
			return ""
		}
		switch w {
		case "raw":
			return "raw", true
		case "chassis":
			if next(1) == "power" && riskyPower[next(2)] {
				return "chassis power " + next(2), true
			}
		case "power":
			if riskyPower[next(1)] {
				return "chassis power " + next(1), true
			}
		case "mc", "bmc":
			if next(1) == "reset" {
				return "mc reset", true
			}
		case "sel":
			if next(1) == "clear" {
				return "sel clear", true
			}
		}
	}
	return "", false
}

var riskyPower = map[string]bool{"off": true, "cycle": true, "reset": true, "soft": true, "diag": true}

// ptrMS is a delay in the host config's unit: whole milliseconds.
func ptrMS(d time.Duration) *int {
	ms := int(max(0, d).Milliseconds())
	return &ms
}
