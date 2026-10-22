package bmc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/sshx"
	"golang.org/x/crypto/ssh"
)

// HPE iLO support (type "ilo"): the driver and the parsing of the iLO CLI's
// answers. Power and boot use Redfish (redfish.go) first. Everything else, and
// the fallback, is the iLO CLI over SSH. iLO 7 has no SSH exec support (a
// remote command returns nothing and exit status 139) and may have IPMI over
// LAN disabled, so the CLI is driven interactively: wait for the prompt, type
// a command, read until the next prompt. The SSH CLI session and console link
// shared with the other vendors are in cli.go.

var (
	// iloPrompt matches the CLI prompt at the end of the text read so far. The
	// prompt follows the current CLI path, so it is "</>hpeiLO-> " at the root
	// and "</map1>hpeiLO-> " elsewhere.
	iloPrompt = regexp.MustCompile(`(?i)\S*hpeilo->[ \t]*$`)
	kvLine    = regexp.MustCompile(`(?i)^(status|status_tag|error_tag)=(.*)$`)
	powerNow  = regexp.MustCompile(`(?i)(?:power is(?: currently:)?|currently:)\s*(\w+)`)
)

// iloResult is one parsed CLI response.
type iloResult struct {
	Status    int // -1 when the response had no status line
	StatusTag string
	ErrorTag  string
	Body      string // everything else, trimmed
}

// parseILOResponse parses the text between sending cmd and the next prompt: the
// echoed command, then key=value lines (status, status_tag, error_tag), bare
// "COMMAND COMPLETED" / "COMMAND PROCESSING FAILED" lines, and any other text.
func parseILOResponse(cmd, raw string) iloResult {
	// Everything below is text the iLO sent: strip escape sequences and
	// control characters first (\r and \r\n become \n).
	raw = console.CleanRemoteN(raw, 64<<10)
	raw = iloPrompt.ReplaceAllString(raw, "")
	res := iloResult{Status: -1}
	var body []string
	echoSeen := false
	for _, l := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(l)
		if !echoSeen && t != "" {
			echoSeen = true
			if strings.Contains(strings.ToLower(t), strings.ToLower(strings.TrimSpace(cmd))) {
				continue // the terminal's echo of what we typed
			}
		}
		// iLO 7 (fw 1.22) prints a bare status tag line after some responses, with
		// no status= line. Treat it as the status; keep a status= line's value if
		// one comes too.
		switch strings.ToUpper(t) {
		case "COMMAND COMPLETED":
			res.StatusTag = t
			if res.Status < 0 {
				res.Status = 0
			}
			continue
		case "COMMAND PROCESSING FAILED":
			res.StatusTag = t
			if res.Status < 0 {
				res.Status = 2
			}
			continue
		}
		if m := kvLine.FindStringSubmatch(t); m != nil {
			v := strings.TrimSpace(m[2])
			switch strings.ToLower(m[1]) {
			case "status":
				if n, err := fmt.Sscanf(v, "%d", &res.Status); n != 1 || err != nil {
					res.Status = -1
				}
			case "status_tag":
				res.StatusTag = v
			case "error_tag":
				res.ErrorTag = v
			}
			continue
		}
		body = append(body, t)
	}
	res.Body = strings.TrimSpace(strings.Join(body, "\n"))
	return res
}

// Err returns the failure for a response with a non-zero status, carrying the
// iLO's own error text; nil otherwise.
func (r iloResult) Err(cmd string) error {
	if r.Status <= 0 {
		return nil
	}
	msg := r.StatusTag
	if msg == "" {
		msg = fmt.Sprintf("status=%d", r.Status)
	}
	if r.ErrorTag != "" {
		msg += ": " + r.ErrorTag
	}
	if r.Body != "" {
		msg += ": " + strings.Join(strings.Fields(r.Body), " ")
	}
	return fmt.Errorf("iLO CLI %q failed: %s", cmd, console.CleanRemoteLine(msg))
}

// errCLINoReply marks a CLI command that was sent but never answered: it may
// have been applied.
var errCLINoReply = errors.New("no answer after the command was sent")

// iloRunner runs one CLI command and returns its parsed response.
type iloRunner interface {
	Run(cmd string, timeout time.Duration) (iloResult, error)
}

// Run types cmd at the prompt and returns the parsed response. It expects the
// session to be sitting at a prompt.
func (s *cliSession) Run(cmd string, timeout time.Duration) (iloResult, error) {
	if err := s.Send(cmd + "\r"); err != nil {
		return iloResult{Status: -1}, fmt.Errorf("send %q: %w", cmd, err)
	}
	raw, err := s.expectPrompt(timeout)
	if err != nil {
		return iloResult{Status: -1}, fmt.Errorf("iLO CLI %q: %w: %w", cmd, errCLINoReply, err)
	}
	res := parseILOResponse(cmd, raw)
	return res, res.Err(cmd)
}

// iloDial connects to an iLO and waits for its first prompt.
func iloDial(name string, h config.Host, rows, cols int) (*cliSession, *ssh.Session, io.WriteCloser, error) {
	return cliDial(name, h, iloSpec, rows, cols)
}

// iloDo opens a CLI session, runs cmd and closes it.
func iloDo(name string, h config.Host, cmd string) (iloResult, error) {
	s, _, _, err := iloDial(name, h, 24, 200)
	if err != nil {
		return iloResult{Status: -1}, err
	}
	defer s.Close()
	res, err := s.Run(cmd, cliCmdTimeout)
	_ = s.Send("exit\r")
	return res, err
}

// Command mapping for the iLO CLI.

// errILOSoft explains that the iLO CLI has no graceful shutdown.
var errILOSoft = errors.New("the iLO CLI has no graceful shutdown (soft). Use Redfish (GracefulShutdown, needs port 443), or `power off` for a hard power off, or enable IPMI over LAN on the iLO (iLO Settings > Access > IPMI/DCMI over LAN) and set the host type to \"ipmi\" (oob edit)")

// errILOCycleCmd is for cycle and reset, which the driver builds from off,
// wait, on (iloCycle) because the iLO CLI has no single command for them.
var errILOCycleCmd = errors.New("the iLO CLI has no single cycle or reset command; the driver runs power off, waits for off, then power on")

// iloPowerCmd maps a power action to the iLO CLI command.
func iloPowerCmd(act string) (string, error) {
	switch act {
	case "status":
		return "power", nil
	case "on":
		return "power on", nil
	case "off":
		return "power off", nil
	case "cycle", "reset":
		return "", errILOCycleCmd
	case "soft":
		return "", errILOSoft
	}
	return "", UnknownPowerError(act)
}

// iloBootCmd maps a boot device to the iLO CLI command.
func iloBootCmd(dev string) (string, error) {
	if dev == "pxe" {
		return "onetimeboot netdev1", nil
	}
	return "", fmt.Errorf("boot %s is not supported via the iLO CLI (only pxe, as onetimeboot netdev1)", dev)
}

// iloUIDCmd maps a uid argument ("", "on", "off") to the iLO CLI command.
func iloUIDCmd(arg string) (string, error) {
	switch arg {
	case "":
		return "uid", nil
	case "on", "off":
		return "uid " + arg, nil
	}
	return "", fmt.Errorf("uid takes on or off, got %q", arg)
}

const iloNMICmd = "nmi server"

// powerState extracts "on" or "off" from a `power` response body.
func powerState(r iloResult) (string, bool) {
	if m := powerNow.FindStringSubmatch(r.Body); m != nil {
		return strings.ToLower(m[1]), true
	}
	return "", false
}

// iloPowerText renders a power response like ipmitool does for status, and
// falls back to the iLO's own text.
func iloPowerText(act string, r iloResult) string {
	if act == "status" {
		if st, ok := powerState(r); ok {
			return "Chassis Power is " + st
		}
	}
	if r.Body != "" {
		return console.CleanRemote(r.Body)
	}
	return console.CleanRemote(r.StatusTag)
}

// alreadyOffRE matches the iLO's refusal to power off a host that is off.
var alreadyOffRE = regexp.MustCompile(`(?i)already\s+(?:powered\s+)?off`)

// iloCycle powers off, waits for the host to report off, then powers on. The
// iLO CLI has no cycle, so this is the long way round. name is for messages.
// The errors say what state the host is left in.
func iloCycle(name string, r iloRunner, sleep func(time.Duration)) (string, error) {
	check := console.ResultUnknown(name)
	if _, err := r.Run("power off", cliCmdTimeout); err != nil && !alreadyOffRE.MatchString(err.Error()) {
		if errors.Is(err, errCLINoReply) {
			return "", fmt.Errorf("power off: %w; host state unknown, %s", err, check)
		}
		return "", err // the iLO refused: nothing changed
	}
	off := false
	for i := 0; i < 15 && !off; i++ {
		sleep(2 * time.Second)
		res, err := r.Run("power", cliCmdTimeout)
		if err != nil {
			return "", fmt.Errorf("could not read the power state after power off: %w; host state unknown, %s", err, check)
		}
		st, ok := powerState(res)
		off = ok && st == "off"
	}
	if !off {
		return "", fmt.Errorf("host did not report off within 30s; not powering on. Host state unknown, %s", check)
	}
	sleep(3 * time.Second)
	if _, err := r.Run("power on", cliCmdTimeout); err != nil {
		return "", fmt.Errorf("host is OFF; power on failed: %w", err)
	}
	return "powered off, then on", nil
}

// iloPowerCycleFn is the console's p key for an iLO: a second SSH connection,
// so the VSP session stays up.
func iloPowerCycleFn(name string, h config.Host) func() (string, error) {
	return func() (string, error) {
		s, _, _, err := iloDial(name, h, 24, 200)
		if err != nil {
			return "", err
		}
		defer s.Close()
		out, err := iloCycle(name, s, time.Sleep)
		_ = s.Send("exit\r")
		return out, err
	}
}

var iloSpec = cliSpec{
	kind: "vsp", promptKind: "ilo cli", prompt: iloPrompt, resync: true, start: []string{"vsp"},
	entered: regexp.MustCompile(`(?i)Virtual Serial Port Active`),
	esc:     []byte{0x1b, '('}, breakMsg: "VSP has no BREAK", label: "iLO CLI", vspEsc: true,
}

// iloDriver prefers Redfish (power and boot) and falls back to the SSH CLI when
// Redfish is unreachable or refuses the login. Everything else, and the console,
// use the SSH CLI. sel inherits generic IPMI.
type iloDriver struct {
	ipmiDriver
	rf *redfishClient
}

func newILODriver(name string, h config.Host) *iloDriver {
	h.Name = name
	return &iloDriver{ipmiDriver: ipmiDriver{name, h}, rf: redfishFor(name, h)}
}

// viaRedfish runs fn. When Redfish is unusable it says why on stderr (unless
// quiet) and reports handled=false so the caller can use the SSH CLI. Any other
// error is the answer.
func (d *iloDriver) viaRedfish(ctx context.Context, quiet bool, fn func(ctx context.Context) error) (handled bool, err error) {
	if d.h.Pass == "" && d.h.User != "" && (d.h.Key != "" || sshx.AgentUsable()) {
		// Redfish needs the password; the SSH CLI can log in with the key.
		return false, nil
	}
	if err := checkCreds(d.name, d.h); err != nil {
		return true, err
	}
	err = fn(ctx)
	if err != nil && redfishFallback(err) {
		if !quiet && !app.Quiet() {
			app.Notef(os.Stderr, "iLO Redfish unavailable (%v); using the SSH CLI", err)
		}
		return false, nil
	}
	return true, err
}

// redfishReset sends the reset for a power action and returns the ResetType
// used. cycle becomes PowerCycle only when the BMC lists it (one GET), else
// ForceRestart. on and off that the BMC refuses as an invalid operation for
// the current state (matched by MessageId, then confirmed by reading the
// state) count as done: alreadyMsg says so.
func (d *iloDriver) redfishReset(ctx context.Context, act string) (rt, alreadyMsg string, err error) {
	rt, ok := redfishResetType(act)
	if !ok {
		return "", "", UnknownPowerError(act)
	}
	if act == "cycle" {
		allowed, err := d.rf.ResetTypes(ctx)
		if err != nil {
			return "", "", err
		}
		if slices.Contains(allowed, "PowerCycle") {
			rt = "PowerCycle"
		}
	}
	err = d.rf.Reset(ctx, rt)
	var re *redfishError
	if err != nil && (act == "on" || act == "off") && errors.As(err, &re) && re.hasMessageID("InvalidOperationForSystemState") {
		want := map[string]string{"on": "On", "off": "Off"}[act]
		if st, serr := d.rf.PowerState(ctx); serr == nil && strings.EqualFold(st, want) {
			return rt, "Chassis Power is already " + act, nil
		}
	}
	return rt, "", err
}

func (d *iloDriver) Power(act string) error {
	if act == "status" {
		r := d.PowerStatus(cliCmdTimeout)
		if r.Failed {
			return errors.New(strings.TrimPrefix(r.Text, "ERROR: "))
		}
		fmt.Fprintln(Stdout(), r.Text)
		return nil
	}
	if _, ok := redfishResetType(act); !ok {
		return UnknownPowerError(act)
	}
	var rt, already string
	handled, err := d.viaRedfish(context.Background(), false, func(ctx context.Context) error {
		var err error
		rt, already, err = d.redfishReset(ctx, act)
		return err
	})
	if handled {
		switch {
		case err != nil:
		case already != "":
			fmt.Fprintln(Stdout(), already)
		default:
			fmt.Fprintf(Stdout(), "Chassis Power Control: %s (Redfish %s)\n", act, rt)
		}
		return err
	}
	switch act {
	case "cycle", "reset":
		out, err := iloPowerCycleFn(d.name, d.h)()
		if err == nil {
			fmt.Fprintln(Stdout(), out)
		}
		return err
	}
	cmd, err := iloPowerCmd(act) // on, off; soft is refused with an explanation
	if err != nil {
		return err
	}
	app.Progressf("connecting to %s via SSH (iLO CLI)...", d.name)
	res, err := iloDo(d.name, d.h, cmd)
	if err != nil {
		if errors.Is(err, errCLINoReply) {
			return fmt.Errorf("%w; %s", err, console.ResultUnknown(d.name))
		}
		return err
	}
	fmt.Fprintln(Stdout(), iloPowerText(act, res))
	return nil
}

// PowerStatus reads the state with one deadline for the whole attempt:
// Redfish first, then the SSH CLI if Redfish is unusable.
func (d *iloDriver) PowerStatus(timeout time.Duration) StatusResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var state string
	handled, err := d.viaRedfish(ctx, true, func(ctx context.Context) error {
		var err error
		state, err = d.rf.PowerState(ctx)
		return err
	})
	if handled {
		if err != nil {
			return statusFail(err.Error())
		}
		return StatusResult{Text: "Chassis Power is " + strings.ToLower(state)}
	}
	slow := func() StatusResult {
		return statusFail((&console.TimeoutError{Host: d.name, Via: "iLO CLI", After: timeout}).Error())
	}
	if ctx.Err() != nil {
		return slow()
	}
	type out struct {
		res iloResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, err := iloDo(d.name, d.h, "power")
		ch <- out{r, err}
	}()
	select {
	case o := <-ch:
		if o.err != nil {
			return statusFail(o.err.Error())
		}
		return StatusResult{Text: console.CleanRemoteLine(iloPowerText("status", o.res))}
	case <-ctx.Done():
		return slow()
	}
}

func (d *iloDriver) Boot(dev string, legacy, persistent bool) error {
	target, ok := redfishBootTarget(dev)
	if !ok {
		return fmt.Errorf("unknown boot device %q", dev)
	}
	if persistent {
		return errors.New("--persistent is not supported on iLO (one-time boot only)")
	}
	handled, err := d.viaRedfish(context.Background(), false, func(ctx context.Context) error { return d.rf.BootOnce(ctx, target, legacy) })
	if handled {
		if err == nil {
			mode := ""
			if legacy {
				mode = ", Legacy"
			}
			fmt.Fprintf(Stdout(), "Boot override set: %s once%s (Redfish)\n", target, mode)
		}
		return err
	}
	cliCmd, cliErr := iloBootCmd(dev)
	if cliErr != nil {
		return fmt.Errorf("%w; Redfish is unavailable here and boot %s needs it", cliErr, dev)
	}
	if legacy {
		return errors.New("--legacy needs Redfish, which is unavailable; the iLO CLI cannot choose the boot mode")
	}
	return iloPrint(d.name, d.h, cliCmd)
}

// SEL lists through IPMI like a generic host. Clearing is not offered by the
// iLO CLI, so it needs IPMI over LAN too, and says so when that is off.
func (d *iloDriver) SEL(act string) error {
	if act != "clear" {
		return d.ipmiDriver.SEL(act)
	}
	n, tail, err := runIpmiTail(d.h, false, "sel", "clear")
	if err == nil && n != 0 && strings.Contains(strings.ToLower(tail), strings.ToLower(ipmiSessionFailed)) {
		return errors.New("sel clear is unsupported over the iLO CLI and IPMI over LAN is not reachable; enable IPMI over LAN (iLO Settings > Access > IPMI/DCMI over LAN) and retry")
	}
	return app.Code(n, err)
}

func (d *iloDriver) UID(arg string) error {
	cmd, err := iloUIDCmd(arg)
	if err != nil {
		return err
	}
	return iloPrint(d.name, d.h, cmd)
}

func (d *iloDriver) NMI() error { return iloPrint(d.name, d.h, iloNMICmd) }

func (d *iloDriver) Console(o SOLOptions) error {
	if o.SSH {
		return errNoSSHConsole
	}
	if err := rejectSOLFlags(o, "an iLO uses the virtual serial port (VSP) over SSH"); err != nil {
		return err
	}
	if !console.UseBar() {
		return errors.New("iLO VSP needs an interactive terminal; use `" + app.Name + " ssh " + d.name + "` and type VSP")
	}
	return app.Code(cliConsole(d.name, d.h, iloSpec, d.ConsolePower()))
}

func (d *iloDriver) OpenConsole(o OpenConsoleOptions) (*ConsoleTransport, error) {
	return cliOpen(d.name, d.h, iloSpec, o.Rows, o.Cols)
}

// ConsolePower is a Redfish power cycle when available, else the SSH CLI's
// off, wait, on over a second connection.
func (d *iloDriver) ConsolePower() func() (string, error) {
	cli := iloPowerCycleFn(d.name, d.h)
	return func() (string, error) {
		var rt string
		handled, err := d.viaRedfish(context.Background(), true, func(ctx context.Context) error {
			var err error
			rt, _, err = d.redfishReset(ctx, "cycle")
			return err
		})
		if handled {
			if err != nil {
				return "", err
			}
			return rt + " sent (Redfish)", nil
		}
		return cli()
	}
}

// iloPrint runs one iLO CLI command and prints the response text.
func iloPrint(name string, h config.Host, cmd string) error {
	app.Progressf("connecting to %s via SSH (iLO CLI)...", name)
	res, err := iloDo(name, h, cmd)
	if err != nil {
		return err
	}
	if res.Body != "" {
		fmt.Fprintln(Stdout(), console.CleanRemote(res.Body))
	} else if res.StatusTag != "" {
		fmt.Fprintln(Stdout(), console.CleanRemote(res.StatusTag))
	}
	return nil
}
