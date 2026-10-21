package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// A driver is the per-vendor behavior behind the commands. The commands do the
// argument checks and confirmations; a driver does the work and prints its own
// output. Failures come back as errors (use code() for exit statuses).
type driver interface {
	// Power runs status, on, off, cycle, reset or soft and prints the result.
	Power(act string) error
	// PowerStatus reads the power state quietly, for the status table.
	PowerStatus(timeout time.Duration) statusResult
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
	Console(o solOpts) error
	// ConsolePower is the console's p key: a power cycle over a second connection.
	ConsolePower() func() (string, error)
}

// solOpts are the flags of the sol command.
type solOpts struct {
	force, deact, raw, ssh bool
}

// driverFor picks the driver for a host's type. "" and "ipmi" are generic IPMI.
func driverFor(name string, h Host) driver {
	h.Name = name
	switch h.Type {
	case typeILO:
		return newILODriver(name, h)
	case typeIDRAC:
		return newIDRACDriver(name, h)
	case typeSupermicro:
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
		notef(untestedOut, untestedMsg, vendor)
	}
}

// untestedNoteLater is untestedNote for use while the console bar owns the
// terminal: the note is queued and printed once the terminal is restored.
func untestedNoteLater(vendor string) {
	if untestedFirst() {
		addNotice(fmt.Sprintf(untestedMsg, vendor))
	}
}

// resetUntestedNote lets tests see the note again.
func resetUntestedNote() {
	untestedMu.Lock()
	untestedDone = false
	untestedMu.Unlock()
}

// errUnknownPower is the error for a power action no driver knows.
func errUnknownPower(act string) error { return fmt.Errorf("unknown power action %q", act) }

// consolePowerTimeout bounds the console's p key (a power cycle over a second
// connection).
const consolePowerTimeout = 30 * time.Second

// rejectSOLFlags refuses the IPMI SOL flags (--force, --deactivate, --raw) for
// a console that has no use for them. what says what the console does instead.
func rejectSOLFlags(o solOpts, what string) error {
	if o.deact || o.force || o.raw {
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
	h    Host
}

func (d *ipmiDriver) Power(act string) error {
	switch act {
	case "status":
		return code(runIpmi(d.h, "chassis", "power", act))
	case "on", "off", "cycle", "reset", "soft":
		return runIpmiPower(d.h, "chassis", "power", act)
	}
	return errUnknownPower(act)
}

func (d *ipmiDriver) PowerStatus(timeout time.Duration) statusResult {
	out, err := ipmiOutput(d.h, timeout, "chassis", "power", "status")
	return ipmiStatusResult(out, err)
}

func (d *ipmiDriver) Boot(dev string, legacy, persistent bool) error {
	return code(runIpmi(d.h, bootArgs(dev, legacy, persistent)...))
}

func (d *ipmiDriver) UID(arg string) error {
	args, err := ipmiUIDArgs(arg)
	if err != nil {
		return err
	}
	return code(runIpmi(d.h, args...))
}

func (d *ipmiDriver) NMI() error { return code(runIpmi(d.h, "chassis", "power", "diag")) }

func (d *ipmiDriver) SEL(act string) error { return code(runIpmi(d.h, "sel", act)) }

func (d *ipmiDriver) MCReset(kind string) error { return code(runIpmi(d.h, "mc", "reset", kind)) }

func (d *ipmiDriver) Console(o solOpts) error {
	if o.ssh {
		return errNoSSHConsole
	}
	if o.deact || o.force {
		if n, err := runIpmi(d.h, "sol", "deactivate"); err != nil {
			return err
		} else if o.deact {
			return code(n, nil)
		}
	}
	if !o.raw && useBar() {
		return code(solConsole(d.h, d.name, d.ConsolePower()))
	}
	fmt.Fprintln(os.Stderr, "Connecting to SOL on "+d.name+". Disconnect with ~. (tilde, dot) at the start of a line (~~. inside an outer ssh session).")
	return code(runIpmi(d.h, "sol", "activate"))
}

func (d *ipmiDriver) ConsolePower() func() (string, error) { return powerCycleFn(d.name, d.h) }

// ipmiStatusResult turns ipmitool's output into a status table cell.
func ipmiStatusResult(out string, err error) statusResult {
	out = trimLast(out)
	if err == nil {
		return statusResult{text: cleanRemoteLine(out)}
	}
	var te *timeoutErr
	switch {
	case errors.As(err, &te):
		out = te.Error()
	case out == "":
		out = err.Error()
	}
	return statusFail(out)
}
