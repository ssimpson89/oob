package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"
)

// Supermicro support (type "supermicro"). UNTESTED: written from vendor
// documentation, never run against a real BMC. State-changing actions print an
// "untested" note once.
//
// The main path is ipmitool, like generic IPMI, with two Supermicro tweaks:
// cipher suite selection (17 first, then 3) and always clearing a stale SOL
// session before activating. `oob sol --ssh` is a fallback through the BMC's
// SMASH-CLP shell.

const (
	// smcFirstCipher and smcSecondCipher are tried in order when the host has no
	// cipher set.
	smcFirstCipher  = "17"
	smcSecondCipher = "3"
)

// smcPrompt is a loose SMASH-CLP prompt match ("/->", "/system1/sol1->", ...).
// [unverified] Supermicro prompts were not checked on a real BMC.
var smcPrompt = regexp.MustCompile(`(?:->|[#>])[ \t]*$`)

// cipherFailureRE matches ipmitool output that may be a cipher problem, the
// cases where trying another cipher for this run is reasonable.
var cipherFailureRE = regexp.MustCompile(`(?i)no matching cipher suite|unable to establish IPMI v2 / RMCP\+ session|error in open session response`)

// cipherSpecificRE matches only output that names the cipher suite as the
// problem. Only this justifies saving a cipher on the host.
var cipherSpecificRE = regexp.MustCompile(`(?i)no matching cipher suite`)

// cipherFailure reports whether ipmitool output looks like a cipher or RMCP+
// session setup failure, the cases where another cipher may help.
func cipherFailure(out string) bool { return cipherFailureRE.MatchString(out) }

// cipherSpecific reports whether the output blames the cipher suite itself.
// "Unable to establish IPMI v2 / RMCP+ session" is not enough: it is also what
// a wrong password, a disabled IPMI over LAN or an unreachable BMC prints, so a
// retry that happens to work once must not rewrite the saved host.
func cipherSpecific(out string) bool { return cipherSpecificRE.MatchString(out) }

// ipmiRun is one ipmitool attempt.
type ipmiRun struct {
	code int
	out  string // tail of stderr, or combined output
	err  error
}

func (r ipmiRun) ok() bool { return r.code == 0 && r.err == nil }

// smcLog is where the retry notes and a withheld first-attempt error go.
var smcLog io.Writer = os.Stderr

// smcSwitch says what happened to the cipher in smcRetry.
type smcSwitch struct {
	used    bool // the second cipher was the one that worked
	persist bool // and the first failure was cipher specific, so save it
}

// smcRetry runs an ipmitool attempt with the host's cipher. With none set it
// tries cipher 17, and if that fails like a cipher problem, retries once with 3.
// It returns the host as it ended up (with the cipher used), the last attempt,
// and a smcSwitch saying whether the second cipher worked and may be saved.
// run gets quiet=true for the first attempt: it must not show errors then (it
// withholds ipmitool's stderr, since a retry may follow). show says what to do
// with the withheld output of a first failure that is not a cipher problem:
// true prints it with the hint (commands that write to the terminal), false
// leaves it to the caller (the status table reports the error itself).
func smcRetry(h Host, show bool, run func(h Host, quiet bool) ipmiRun) (Host, ipmiRun, smcSwitch) {
	if h.Cipher != "" {
		return h, run(h, false), smcSwitch{}
	}
	h1 := h
	h1.Cipher = smcFirstCipher
	r := run(h1, true)
	if r.ok() {
		return h1, r, smcSwitch{}
	}
	if !cipherFailure(r.out) {
		// A real error, not a cipher problem: show what we held back.
		if show {
			fmt.Fprint(smcLog, cleanRemote(r.out))
			if hint := ipmiHint(r.out, h1); hint != "" {
				fmt.Fprintln(smcLog, hint)
			}
		}
		return h1, r, smcSwitch{}
	}
	if !quietNotes.Load() {
		notef(smcLog, "cipher suite %s failed; retrying once with %s", smcFirstCipher, smcSecondCipher)
	}
	h2 := h
	h2.Cipher = smcSecondCipher
	r2 := run(h2, false)
	return h2, r2, smcSwitch{used: r2.ok(), persist: r2.ok() && cipherSpecific(r.out)}
}

// saveCipher stores the working cipher on the saved host (locked, atomic write)
// and queues a note. It leaves a host that already has a cipher alone, and
// does nothing when the defaults block already sets one (the host's merged
// cipher came from there, so the cipher in use is the defaults' choice).
func saveCipher(name, cipher string) error {
	saved := false
	err := UpdateConfig(func(c *Config) error {
		h, ok := c.Hosts[name]
		if !ok || h.Cipher != "" || c.Defaults.Cipher != "" {
			return errNoUpdate
		}
		h.Cipher = cipher
		c.Hosts[name] = h
		saved = true
		return nil
	})
	if err == nil && saved {
		addNotice(fmt.Sprintf("cipher suite %s worked for %s; saved it on the host", cipher, name))
	}
	return err
}

// saveCipherFn is replaced by tests.
var saveCipherFn = saveCipher

// afterSwitch saves the cipher that worked when that is justified. A failed
// save only produces a note: it must never fail the command that succeeded.
func afterSwitch(name string, h Host, sw smcSwitch) {
	if !sw.used {
		return
	}
	if !sw.persist {
		addNotice(fmt.Sprintf("cipher suite %s worked for %s this time but was not saved, because the failure with %s did not name the cipher. If it keeps needing %s, set it with `%s edit`", h.Cipher, name, smcFirstCipher, h.Cipher, AppName))
		return
	}
	if err := saveCipherFn(name, h.Cipher); err != nil {
		addNotice(fmt.Sprintf("could not save cipher suite %s for %s: %v", h.Cipher, name, cleanRemoteLine(err.Error())))
	}
}

type supermicroDriver struct {
	ipmiDriver
}

func newSupermicroDriver(name string, h Host) *supermicroDriver {
	return &supermicroDriver{ipmiDriver{name, h}}
}

// run runs ipmitool on the terminal with the cipher fallback.
func (d *supermicroDriver) run(args ...string) (int, string, error) {
	h, r, sw := smcRetry(d.h, true, func(h Host, quiet bool) ipmiRun {
		code, out, err := runIpmiTail(h, quiet, args...)
		return ipmiRun{code, out, err}
	})
	afterSwitch(d.name, h, sw)
	return r.code, r.out, r.err
}

func (d *supermicroDriver) ipmi(args ...string) error {
	n, _, err := d.run(args...)
	return code(n, err)
}

// ipmiPower is ipmi for a state-changing power command: a timeout is reported
// as an unknown result.
func (d *supermicroDriver) ipmiPower(args ...string) error {
	n, tail, err := d.run(args...)
	return powerResult(d.h, n, tail, err)
}

// output runs ipmitool with a timeout and the cipher fallback, returning its output.
func (d *supermicroDriver) output(timeout time.Duration, args ...string) (string, error) {
	h, r, sw := smcRetry(d.h, false, func(h Host, _ bool) ipmiRun {
		out, err := ipmiOutput(h, timeout, args...)
		return ipmiRun{out: out, err: err}
	})
	afterSwitch(d.name, h, sw)
	return r.out, r.err
}

func (d *supermicroDriver) Power(act string) error {
	switch act {
	case "status":
		return d.ipmi("chassis", "power", act)
	case "on", "off", "cycle", "reset", "soft":
		untestedNote(vendorName(typeSupermicro))
		return d.ipmiPower("chassis", "power", act)
	}
	return errUnknownPower(act)
}

func (d *supermicroDriver) PowerStatus(timeout time.Duration) statusResult {
	out, err := d.output(timeout, "chassis", "power", "status")
	return ipmiStatusResult(out, err)
}

func (d *supermicroDriver) Boot(dev string, legacy, persistent bool) error {
	if persistent {
		return errors.New("--persistent is not supported on Supermicro: the BMC tends to revert persistent boot flags, so use the one-time boot (the default)")
	}
	untestedNote(vendorName(typeSupermicro))
	return d.ipmi(bootArgs(dev, legacy, false)...)
}

func (d *supermicroDriver) UID(arg string) error {
	args, err := ipmiUIDArgs(arg)
	if err != nil {
		return err
	}
	untestedNote(vendorName(typeSupermicro))
	return d.ipmi(args...)
}

func (d *supermicroDriver) NMI() error {
	untestedNote(vendorName(typeSupermicro))
	return d.ipmi("chassis", "power", "diag")
}

func (d *supermicroDriver) SEL(act string) error {
	if act == "clear" {
		untestedNote(vendorName(typeSupermicro))
	}
	return d.ipmi("sel", act)
}

func (d *supermicroDriver) MCReset(kind string) error {
	untestedNote(vendorName(typeSupermicro))
	return d.ipmi("mc", "reset", kind)
}

func (d *supermicroDriver) ConsolePower() func() (string, error) {
	return func() (string, error) {
		untestedNoteLater(vendorName(typeSupermicro))
		out, err := d.output(consolePowerTimeout, "chassis", "power", "cycle")
		return ipmiPowerOutcome(d.name, out, err)
	}
}

func (d *supermicroDriver) Console(o solOpts) error {
	if o.ssh {
		return d.sshConsole(o)
	}
	// Always clear a stale SOL session first, so --force is already what this
	// does and is honored by default. Errors are ignored, but this also
	// settles which cipher works, so the activate below uses it.
	h, r, sw := smcRetry(d.h, true, func(h Host, quiet bool) ipmiRun {
		code, out, err := runIpmiTail(h, quiet, "sol", "deactivate")
		return ipmiRun{code, out, err}
	})
	afterSwitch(d.name, h, sw)
	if o.deact {
		return code(r.code, r.err)
	}
	if !r.ok() && cipherFailure(r.out) {
		return code(r.code, r.err) // activate would fail the same way
	}
	return (&ipmiDriver{d.name, h}).Console(solOpts{raw: o.raw})
}

// sshConsole is the SMASH-CLP fallback: cd to the SOL target and start it.
func (d *supermicroDriver) sshConsole(o solOpts) error {
	if err := rejectSOLFlags(o, "--ssh uses the BMC's SMASH-CLP shell instead"); err != nil {
		return err
	}
	if !useBar() {
		return errors.New("the SSH console needs an interactive terminal")
	}
	return code(cliConsole(d.name, d.h, smcSpec, d.ConsolePower()))
}

// smcSpec: the SMASH-CLP fallback. No resync: the prompt pattern also matches
// a Linux shell on the host.
var smcSpec = cliSpec{
	kind: "sol", promptKind: "smash", prompt: smcPrompt, start: []string{"cd /system1/sol1", "start"},
	// [unverified] ESC ( is the escape iLO uses; the Supermicro SMASH-CLP
	// escape was not confirmed. If it does not work, the 2s fallback closes
	// the session anyway, and the c key reports that no prompt appeared.
	esc: []byte{0x1b, '('}, breakMsg: "the SMASH-CLP SOL has no BREAK", label: "Supermicro SMASH-CLP",
}
