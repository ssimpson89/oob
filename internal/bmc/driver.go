package bmc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/detect"
)

// A Driver is the per-vendor behavior behind the commands. The commands do the
// argument checks and confirmations; a driver does the work and prints its own
// output. Failures come back as errors (use app.Code() for exit statuses).
type Driver interface {
	// Power runs status, on, off, cycle, reset or soft and prints the result.
	Power(act string) error
	// PowerStatus reads the power state quietly, for the status table.
	PowerStatus(timeout time.Duration) StatusResult
	// Boot sets the next boot device (pxe, disk or bios).
	Boot(dev string, legacy, persistent bool) error
	// UID shows ("") or sets ("on", "off") the identify LED.
	UID(arg string) error
	NMI() error
	// SEL lists or clears the system event log.
	SEL(act string) error
	// MCReset resets the BMC ("warm" or "cold"). The command confirms first.
	MCReset(kind string) error
	// Console opens the serial console.
	Console(o SOLOptions) error
	// OpenConsole starts the serial console without a terminal (no bar, no
	// raw mode) and returns the transport; the caller owns it and must Close
	// it. Console runs the same setup under the bar.
	OpenConsole(o OpenConsoleOptions) (*ConsoleTransport, error)
	// ConsolePower is the console's p key: a power cycle over a second connection.
	ConsolePower() func() (string, error)
}

// SOLOptions are the flags of the sol command.
type SOLOptions struct {
	Force, Deact, Raw, SSH bool
}

// driverFor picks the driver for a host's type. "" and "ipmi" are generic IPMI.
func DriverFor(name string, h config.Host) Driver {
	h.Name = name
	switch h.Type {
	case detect.TypeILO:
		return newILODriver(name, h)
	case detect.TypeIDRAC:
		return newIDRACDriver(name, h)
	case detect.TypeSupermicro:
		return newSupermicroDriver(name, h)
	}
	return &ipmiDriver{name, h}
}

// untestedOut and untestedDone: one line per run, the first time a driver for
// an untested vendor changes state.
var (
	untestedOut  io.Writer = os.Stderr
	untestedMu   sync.Mutex
	untestedDone bool
)

// untestedFirst reports whether this is the first untested-vendor action of
// the run, and marks it seen.
func untestedFirst() bool {
	untestedMu.Lock()
	defer untestedMu.Unlock()
	first := !untestedDone
	untestedDone = true
	return first
}

const untestedMsg = "%s support is untested; please report results"

func untestedNote(vendor string) {
	if untestedFirst() {
		app.Notef(untestedOut, untestedMsg, vendor)
	}
}

// untestedNoteLater is untestedNote for use while the console bar owns the
// terminal: the note is queued and printed once the terminal is restored.
func untestedNoteLater(vendor string) {
	if untestedFirst() {
		app.AddNotice(fmt.Sprintf(untestedMsg, vendor))
	}
}

// UnknownPowerError is the error for a power action no driver knows.
func UnknownPowerError(act string) error { return fmt.Errorf("unknown power action %q", act) }

// consolePowerTimeout bounds the console's p key (a power cycle over a second
// connection).
const consolePowerTimeout = 30 * time.Second

// rejectSOLFlags refuses the IPMI SOL flags (--force, --deactivate, --raw) for
// a console that has no use for them. what says what the console does instead.
func rejectSOLFlags(o SOLOptions, what string) error {
	if o.Deact || o.Force || o.Raw {
		return errors.New("--force, --deactivate and --raw are for IPMI SOL; " + what)
	}
	return nil
}

// errNoSSHConsole is for --ssh on a host whose driver has no such fallback.
var errNoSSHConsole = errors.New("--ssh is only for Supermicro hosts (type supermicro)")

// ipmiDriver is generic IPMI through ipmitool. Other drivers embed it for what
// they do not override.
type ipmiDriver struct {
	name string
	h    config.Host
}

func (d *ipmiDriver) Power(act string) error {
	switch act {
	case "status":
		return app.Code(RunIPMI(d.h, "chassis", "power", act))
	case "on", "off", "cycle", "reset", "soft":
		return runIpmiPower(d.h, "chassis", "power", act)
	}
	return UnknownPowerError(act)
}

func (d *ipmiDriver) PowerStatus(timeout time.Duration) StatusResult {
	out, err := ipmiOutput(d.h, timeout, "chassis", "power", "status")
	return ipmiStatusResult(out, err)
}

func (d *ipmiDriver) Boot(dev string, legacy, persistent bool) error {
	return app.Code(RunIPMI(d.h, bootArgs(dev, legacy, persistent)...))
}

func (d *ipmiDriver) UID(arg string) error {
	args, err := ipmiUIDArgs(arg)
	if err != nil {
		return err
	}
	return app.Code(RunIPMI(d.h, args...))
}

func (d *ipmiDriver) NMI() error { return app.Code(RunIPMI(d.h, "chassis", "power", "diag")) }

func (d *ipmiDriver) SEL(act string) error { return app.Code(RunIPMI(d.h, "sel", act)) }

func (d *ipmiDriver) MCReset(kind string) error { return app.Code(RunIPMI(d.h, "mc", "reset", kind)) }

func (d *ipmiDriver) Console(o SOLOptions) error {
	if o.SSH {
		return errNoSSHConsole
	}
	if o.Deact || o.Force {
		if n, err := RunIPMI(d.h, "sol", "deactivate"); err != nil {
			return err
		} else if o.Deact {
			return app.Code(n, nil)
		}
	}
	if !o.Raw && console.UseBar() {
		return app.Code(solConsole(d.h, d.name, d.ConsolePower(), nil))
	}
	fmt.Fprintln(os.Stderr, "Connecting to SOL on "+d.name+". Disconnect with ~. (tilde, dot) at the start of a line (~~. inside an outer ssh session).")
	return app.Code(RunIPMI(d.h, "sol", "activate"))
}

// OpenConsole starts IPMI SOL. With Force a stale session is cleared first.
func (d *ipmiDriver) OpenConsole(o OpenConsoleOptions) (*ConsoleTransport, error) {
	if o.Force {
		if out, err := ipmiOutput(d.h, ipmiQuickTimeout, "sol", "deactivate"); err != nil && !isExitError(err) {
			return nil, fmt.Errorf("sol deactivate: %w: %s", err, console.CleanRemoteLine(out))
		}
	}
	return openSOL(d.h, o.Rows, o.Cols)
}

// ipmiQuickTimeout bounds the short ipmitool calls made while opening a console.
const ipmiQuickTimeout = 20 * time.Second

// ipmiRunQuiet is one ipmitool attempt whose output is collected, not shown.
// A non-zero exit is a result (code), not an error.
func ipmiRunQuiet(h config.Host, timeout time.Duration, args ...string) ipmiRun {
	out, err := ipmiOutput(h, timeout, args...)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return ipmiRun{0, out, nil}
	case errors.As(err, &ee):
		return ipmiRun{exitCodeOf(ee), out, nil}
	}
	return ipmiRun{1, out, err}
}

// isExitError reports whether err is ipmitool exiting non-zero (as opposed to
// failing to run at all).
func isExitError(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

func (d *ipmiDriver) ConsolePower() func() (string, error) { return powerCycleFn(d.name, d.h) }

// ipmiStatusResult turns ipmitool's output into a status table cell.
func ipmiStatusResult(out string, err error) StatusResult {
	out = trimLast(out)
	if err == nil {
		return StatusResult{Text: console.CleanRemoteLine(out)}
	}
	var te *console.TimeoutError
	switch {
	case errors.As(err, &te):
		out = te.Error()
	case out == "":
		out = err.Error()
	}
	return statusFail(out)
}

// StatusResult is one host's outcome in the status table.
type StatusResult struct {
	Text   string
	Failed bool
}

// trimLast returns the last non-empty line of out.
func trimLast(out string) string {
	out = strings.TrimSpace(out)
	if i := strings.LastIndex(out, "\n"); i >= 0 {
		out = out[i+1:]
	}
	return out
}

// statusFail is a failed status table cell.
func statusFail(text string) StatusResult {
	return StatusResult{Text: "ERROR: " + console.CleanRemoteLine(console.StripErrorPrefix(text)), Failed: true}
}
