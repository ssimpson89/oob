package bmc

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/detect"
	"github.com/ssimpson89/oob/internal/sshx"
)

// Dell iDRAC support (type "idrac"). UNTESTED: written from Dell's racadm
// documentation, never run against a real iDRAC. State-changing actions print an
// "untested" note once.
//
// Unlike iLO 7, iDRAC runs SSH exec requests, so each command is a plain
// `racadm ...` run over its own SSH connection (sshExec). The console needs the
// interactive SMCLP shell (cliConsole in cli.go).

var (
	// racPowerRE reads `racadm serveraction powerstatus`, e.g. "Server power status: ON".
	racPowerRE = regexp.MustCompile(`(?i)power status:\s*(ON|OFF)`)
	// idracPrompt is the SMCLP prompt (/admin1->) or the racadm prompt (racadm>>).
	idracPrompt = regexp.MustCompile(`(?i)(?:admin1->|racadm>>)[ \t]*$`)
)

// idracPowerCmd maps a power action to the racadm serveraction argument.
func idracPowerCmd(act string) (string, error) {
	switch act {
	case "status":
		return "powerstatus", nil
	case "on":
		return "powerup", nil
	case "off":
		return "powerdown", nil
	case "cycle":
		return "powercycle", nil
	case "reset":
		return "hardreset", nil
	case "soft":
		return "graceshutdown", nil
	}
	return "", UnknownPowerError(act)
}

// idracPowerState extracts "on" or "off" from powerstatus output.
func idracPowerState(out string) (string, bool) {
	if m := racPowerRE.FindStringSubmatch(out); m != nil {
		return strings.ToLower(m[1]), true
	}
	return "", false
}

// racadmError returns the lines of out that start with ERROR as an error, or
// nil. The text is sanitized and loses its "ERROR:" label, which the caller adds
// again when it prints.
func racadmError(out string) error {
	var bad []string
	for _, l := range strings.Split(console.CleanRemote(out), "\n") {
		l = strings.TrimSpace(l)
		if len(l) >= 5 && strings.EqualFold(l[:5], "ERROR") {
			bad = append(bad, strings.TrimSpace(strings.TrimPrefix(l[5:], ":")))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return errors.New("racadm: " + strings.Join(bad, "; "))
}

// idracDriver runs racadm over SSH exec. run is a field so tests can fake it.
type idracDriver struct {
	ipmiDriver // unused except for the host; every action is overridden
	run        func(cmd string, timeout time.Duration, noPrompt bool) (string, error)
}

func newIDRACDriver(name string, h config.Host) *idracDriver {
	return &idracDriver{ipmiDriver: ipmiDriver{name, h}, run: func(cmd string, timeout time.Duration, noPrompt bool) (string, error) {
		return sshExec(name, h, cmd, timeout, noPrompt)
	}}
}

// racadm runs one racadm command and returns its output. Output with an ERROR
// line is a failure even when the exit status is 0.
func (d *idracDriver) racadm(timeout time.Duration, args ...string) (string, error) {
	return d.racadmOpt(timeout, false, args...)
}

// racadmOpt is racadm with noPrompt for runs that must not ask the user for
// anything (the status table, bulk runs).
func (d *idracDriver) racadmOpt(timeout time.Duration, noPrompt bool, args ...string) (string, error) {
	out, err := d.run("racadm "+strings.Join(args, " "), timeout, noPrompt)
	if e := racadmError(out); e != nil {
		return out, e
	}
	if err != nil {
		if s := console.CleanRemoteLine(out); s != "" {
			return out, fmt.Errorf("racadm %s: %w: %s", args[0], err, s)
		}
		return out, fmt.Errorf("racadm %s: %w", args[0], err)
	}
	return out, nil
}

// racadmPrint runs a command and prints what it said.
func (d *idracDriver) racadmPrint(args ...string) error {
	out, err := d.racadm(cliCmdTimeout, args...)
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(console.CleanRemote(out)); s != "" {
		fmt.Fprintln(Stdout(), s)
	}
	return nil
}

// unknownOnTimeout says a state-changing racadm command that timed out may
// have been applied.
func (d *idracDriver) unknownOnTimeout(err error) error {
	var te *console.TimeoutError
	if errors.As(err, &te) {
		return fmt.Errorf("%w; %s", err, console.ResultUnknown(d.name))
	}
	return err
}

func (d *idracDriver) Power(act string) error {
	arg, err := idracPowerCmd(act)
	if err != nil {
		return err
	}
	if act != "status" {
		untestedNote(detect.VendorName(detect.TypeIDRAC))
	}
	out, err := d.racadm(cliCmdTimeout, "serveraction", arg)
	if err != nil {
		if act != "status" {
			err = d.unknownOnTimeout(err)
		}
		return err
	}
	if act == "status" {
		if st, ok := idracPowerState(out); ok {
			fmt.Fprintln(Stdout(), "Chassis Power is "+st)
			return nil
		}
	}
	fmt.Fprintln(Stdout(), strings.TrimSpace(console.CleanRemote(out)))
	return nil
}

func (d *idracDriver) PowerStatus(timeout time.Duration) StatusResult {
	out, err := d.racadmOpt(timeout, true, "serveraction", "powerstatus")
	if err != nil {
		return statusFail(err.Error())
	}
	if st, ok := idracPowerState(out); ok {
		return StatusResult{Text: "Chassis Power is " + st}
	}
	return StatusResult{Text: console.CleanRemoteLine(trimLast(out))}
}

func (d *idracDriver) Boot(dev string, legacy, persistent bool) error {
	if dev != "pxe" {
		return fmt.Errorf("boot %s is not supported on iDRAC by oob (only pxe)", dev)
	}
	if legacy {
		return errors.New("--legacy is not supported on iDRAC: the boot mode is a BIOS setting")
	}
	untestedNote(detect.VendorName(detect.TypeIDRAC))
	// Step 1 is BootOnce, so that the device never becomes the permanent first
	// boot device by accident: Enabled makes the next setting one-time,
	// Disabled makes it stick (--persistent). Step 2 sets the device.
	once := "Enabled"
	if persistent {
		once = "Disabled"
	}
	if _, err := d.racadm(cliCmdTimeout, "set", "iDRAC.ServerBoot.BootOnce", once); err != nil {
		return d.unknownOnTimeout(err)
	}
	if _, err := d.racadm(cliCmdTimeout, "set", "iDRAC.ServerBoot.FirstBootDevice", "PXE"); err != nil {
		return fmt.Errorf("step 1 was applied (iDRAC.ServerBoot.BootOnce is now %s) but setting FirstBootDevice to PXE failed: %w", once, d.unknownOnTimeout(err))
	}
	if persistent {
		fmt.Fprintln(Stdout(), "PXE is now the first boot device for every boot (applies on next boot)")
		return nil
	}
	fmt.Fprintln(Stdout(), "PXE set for the next boot only (applies on next boot)")
	return nil
}

func (d *idracDriver) UID(arg string) error {
	switch arg {
	case "":
		return d.racadmPrint("getled")
	case "on", "off":
		untestedNote(detect.VendorName(detect.TypeIDRAC))
		n := "0"
		if arg == "on" {
			n = "1"
		}
		return d.racadmPrint("setled", "-l", n)
	}
	return fmt.Errorf("uid takes on or off, got %q", arg)
}

func (d *idracDriver) NMI() error {
	untestedNote(detect.VendorName(detect.TypeIDRAC))
	return d.racadmPrint("serveraction", "nmi")
}

func (d *idracDriver) SEL(act string) error {
	switch act {
	case "list":
		return d.racadmPrint("getsel")
	case "clear":
		untestedNote(detect.VendorName(detect.TypeIDRAC))
		return d.racadmPrint("clrsel")
	}
	return fmt.Errorf("unknown sel action %q", act)
}

// MCReset runs `racadm racreset`. It takes no warm or cold argument here, and
// the SSH session may drop as the iDRAC restarts.
func (d *idracDriver) MCReset(kind string) error {
	untestedNote(detect.VendorName(detect.TypeIDRAC))
	app.Notef(os.Stderr, "racadm racreset has no warm or cold choice; running a plain racreset (asked for %s)", kind)
	out, err := d.racadm(cliCmdTimeout, "racreset")
	if err != nil {
		return d.unknownOnTimeout(err)
	}
	if s := strings.TrimSpace(console.CleanRemote(out)); s != "" {
		fmt.Fprintln(Stdout(), s)
	}
	return nil
}

func (d *idracDriver) Console(o SOLOptions) error {
	if o.SSH {
		return errNoSSHConsole
	}
	if err := rejectSOLFlags(o, "an iDRAC uses its serial console (console com2) over SSH"); err != nil {
		return err
	}
	if !console.UseBar() {
		return errors.New("the iDRAC console needs an interactive terminal; use `" + app.Name + " ssh " + d.name + "` and type console com2")
	}
	return app.Code(cliConsole(d.name, d.h, idracSpec, d.ConsolePower()))
}

var idracSpec = cliSpec{
	kind: "com2", promptKind: "idrac cli", prompt: idracPrompt, resync: true, start: []string{"console com2"},
	esc: []byte{0x1c}, breakMsg: "the iDRAC serial console has no BREAK", label: "iDRAC",
}

func (d *idracDriver) OpenConsole(o OpenConsoleOptions) (*ConsoleTransport, error) {
	untestedNote(detect.VendorName(detect.TypeIDRAC))
	return cliOpen(d.name, d.h, idracSpec, o.Rows, o.Cols)
}

// ConsolePower power cycles with a separate racadm call (its own SSH
// connection), so the console session stays up.
func (d *idracDriver) ConsolePower() func() (string, error) {
	return func() (string, error) {
		untestedNoteLater(detect.VendorName(detect.TypeIDRAC))
		out, err := d.racadm(consolePowerTimeout, "serveraction", "powercycle")
		return console.CleanRemoteLine(out), d.unknownOnTimeout(err)
	}
}

// sshExec runs one remote command over SSH (no PTY) and returns its combined
// output. timeout is the caller's whole budget: it bounds the connect and the
// command together. noPrompt keeps the login from asking for a key passphrase.
// It also feeds the output to vendor detection.
func sshExec(name string, h config.Host, cmd string, timeout time.Duration, noPrompt bool) (string, error) {
	if err := sshx.CheckCreds(name, h); err != nil {
		return "", err
	}
	kh, err := sshx.KnownHostsPath()
	if err != nil {
		return "", err
	}
	app.Progressf("connecting to %s via SSH...", name)
	det := detect.NewDetector(name, h)
	deadline := time.Now().Add(timeout)
	cl, err := sshx.ConnectWith(h, kh, sshx.Options{Banner: det.Banner, Timeout: timeout, NoPrompt: noPrompt})
	if err != nil {
		return "", fmt.Errorf("%s (%s): %w", name, h.IP, sshx.NameConnErr(err, name))
	}
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	type res struct {
		out []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := sess.CombinedOutput(cmd)
		ch <- res{b, err}
	}()
	select {
	case r := <-ch:
		det.Feed(r.out) // an empty answer still says the login worked
		return string(r.out), console.CleanErr(r.err)
	case <-time.After(max(time.Until(deadline), 0)):
		return "", &console.TimeoutError{Host: name, Via: "SSH", After: timeout}
	}
}
