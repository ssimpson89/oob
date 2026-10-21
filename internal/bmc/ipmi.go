package bmc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/detect"
)

// errIpmitoolMissing is returned when the ipmitool binary cannot be found.
var errIpmitoolMissing = errors.New("ipmitool is not installed (or not in your PATH), and it is needed for this command")

// installHint returns platform-specific install instructions for ipmitool.
func installHint(goos string) string {
	switch goos {
	case "darwin":
		return "Install it with: brew install ipmitool"
	case "linux":
		return "Install it with: dnf install ipmitool (RHEL, Rocky, Fedora) or apt install ipmitool (Debian, Ubuntu)"
	}
	return "Install ipmitool with your package manager (macOS: brew install ipmitool, Linux: dnf install ipmitool or apt install ipmitool)"
}

func findIpmitool() (string, error) {
	if p, err := exec.LookPath("ipmitool"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/opt/homebrew/bin/ipmitool", "/usr/local/bin/ipmitool", "/usr/bin/ipmitool"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w.\n%s", errIpmitoolMissing, installHint(runtime.GOOS))
}

// checkCreds fails fast when the user or password is missing; who names the host.
func checkCreds(who string, h config.Host) error {
	if h.User == "" {
		return config.NoUserError(who)
	}
	if h.Pass == "" {
		return fmt.Errorf("%s: no password (set it on the host or in defaults). IPMI/Redfish need the BMC password even when you use an SSH key", who)
	}
	return nil
}

// Session setup tuning. ipmitool's lanplus defaults (-N 1 -R 4) took about 21s
// to give up on a silent BMC (measured against an unreachable address). With
// -N 2 -R 2 it takes about 11s, and each packet still gets 2s, so a slow or
// busy BMC is not mistaken for a dead one. -N 1 would fail in 7s but risks
// false failures on BMCs slow to answer the first RAKP message.
const (
	ipmiTimeout = "2"
	ipmiRetries = "2"
)

// ipmiSessionFailed is the text ipmitool prints when it cannot open a session.
const ipmiSessionFailed = "Unable to establish IPMI v2 / RMCP+ session"

// ipmiHint returns advice to append to an ipmitool failure, or "" when the
// output isn't a session failure.
func ipmiHint(out string, h config.Host) string {
	if !strings.Contains(strings.ToLower(out), strings.ToLower(ipmiSessionFailed)) {
		return ""
	}
	s := "Hint: IPMI over LAN may be disabled on the BMC"
	switch h.Type {
	case detect.TypeILO:
		s += " (iLO Settings > Access > IPMI/DCMI over LAN; it is off by default on iLO 7)"
	case detect.TypeIDRAC:
		// [unverified path] iDRAC Settings > Connectivity > Network > IPMI Settings
		s += " (enable IPMI over LAN in the iDRAC network settings)"
	case detect.TypeSupermicro:
		s += " (enable IPMI over LAN in the BMC network settings)"
	default:
		s += " (HPE iLO: iLO Settings > Access > IPMI/DCMI over LAN, off by default on iLO 7)"
	}
	s += ", or try a different `cipher`."
	if h.Type == "" {
		s += "\nIf this BMC is an HPE iLO, run `" + app.Name + " ssh " + hostLabel(h, "<name>") + "` once so " + app.Name + " detects it (or set \"type\": \"ilo\" with `" + app.Name + " edit`) to use SSH instead."
	}
	return s
}

// hostLabel names a host in messages: its config name, else def.
func hostLabel(h config.Host, def string) string {
	if h.Name != "" {
		return h.Name
	}
	return def
}

// tailBuf keeps the last bytes written to it.
type tailBuf struct {
	mu sync.Mutex
	b  []byte
}

const tailMax = 4096

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > tailMax {
		t.b = t.b[len(t.b)-tailMax:]
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// errorLines returns the lines of out that look like ipmitool errors, with
// terminal escapes and carriage returns removed.
func errorLines(out string) []string {
	out = console.CleanRemote(out)
	var all, lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			all = append(all, l)
		}
	}
	// Only the last few lines: ipmitool's own failure comes last, and a long
	// session's tail is mostly the remote console's output.
	if len(all) > 6 {
		all = all[len(all)-6:]
	}
	for _, l := range all {
		if strings.HasPrefix(l, "Error") {
			lines = append(lines, l)
		}
	}
	return lines
}

// reportIpmiFailure prints ipmitool's error lines and the hint to stderr. Call
// it after the terminal is restored so nothing is lost to the bar.
func reportIpmiFailure(out string, h config.Host) {
	for _, l := range errorLines(out) {
		fmt.Fprintln(os.Stderr, l)
	}
	if hint := ipmiHint(out, h); hint != "" {
		fmt.Fprintln(os.Stderr, hint)
	}
}

// ipmiCmd builds an ipmitool command. The password is passed via -E and the
// IPMI_PASSWORD env var so it never appears in argv.
func ipmiCmd(ctx context.Context, h config.Host, args ...string) (*exec.Cmd, error) {
	bin, err := findIpmitool()
	if err != nil {
		return nil, err
	}
	if h.IP == "" {
		return nil, errors.New("host has no ip")
	}
	if err := checkCreds(hostLabel(h, h.IP), h); err != nil {
		return nil, err
	}
	full := []string{"-I", "lanplus", "-H", h.IP, "-U", h.User, "-E", "-N", ipmiTimeout, "-R", ipmiRetries}
	if h.Cipher != "" {
		full = append(full, "-C", h.Cipher)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Env = append(os.Environ(), "IPMI_PASSWORD="+h.Pass)
	return cmd, nil
}

// RunIPMI runs ipmitool attached to the terminal and returns its exit code.
func RunIPMI(h config.Host, args ...string) (int, error) {
	code, _, err := runIpmiTail(h, false, args...)
	return code, err
}

// runIpmiTail is RunIPMI that also returns the tail of ipmitool's stderr. With
// quiet, stderr and the hint are withheld from the terminal so the caller can
// decide whether to show them (used when a retry may follow). Output is
// sanitized on its way to the terminal (FRU, SEL and sensor text comes from
// the BMC), except for `sol activate`, which is a console and stays raw.
func runIpmiTail(h config.Host, quiet bool, args ...string) (int, string, error) {
	cmd, err := ipmiCmd(runCtx(), h, args...)
	if err != nil {
		return 1, "", err
	}
	app.Progressf("connecting to %s via IPMI...", hostLabel(h, h.IP))
	var tail tailBuf
	var out io.Writer = Stdout()
	var errw io.Writer = io.MultiWriter(stderrW(), &tail)
	if !isSolActivate(args) {
		so, se := console.NewSanitizeWriter(Stdout()), console.NewSanitizeWriter(stderrW())
		defer so.Flush()
		defer se.Flush()
		out, errw = so, io.MultiWriter(se, &tail)
	}
	if quiet {
		errw = &tail
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR(), out, errw
	if capturing.Load() != nil {
		cmd.WaitDelay = 2 * time.Second // a killed ipmitool's children must not hold the pipes open
	}
	err = cmd.Run()
	if hint := ipmiHint(tail.String(), h); hint != "" && !quiet {
		fmt.Fprintln(stderrW(), hint)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitCodeOf(ee), tail.String(), nil
		}
		return 1, tail.String(), err
	}
	return 0, tail.String(), nil
}

// isSolActivate reports whether args start an interactive SOL session.
func isSolActivate(args []string) bool {
	for i, a := range args {
		if a == "sol" && i+1 < len(args) && args[i+1] == "activate" {
			return true
		}
	}
	return false
}

// powerTimeoutRE matches ipmitool's own timeout wording.
var powerTimeoutRE = regexp.MustCompile(`(?i)time(?:d)?[ -]?out|no response`)

// runIpmiPower runs a state-changing ipmitool power command. If ipmitool fails
// with a timeout the action may or may not have happened, and the error says so.
func runIpmiPower(h config.Host, args ...string) error {
	n, tail, err := runIpmiTail(h, false, args...)
	return powerResult(h, n, tail, err)
}

func powerResult(h config.Host, n int, tail string, err error) error {
	if err == nil && n != 0 && powerTimeoutRE.MatchString(tail) {
		return fmt.Errorf("ipmitool timed out; %s", console.ResultUnknown(hostLabel(h, "<name>")))
	}
	return app.Code(n, err)
}

// exitCodeOf returns the child's exit code. A child killed by a signal reports
// -1, which maps to 130 for SIGINT and 1 otherwise.
func exitCodeOf(ee *exec.ExitError) int {
	if n := ee.ExitCode(); n >= 0 {
		return n
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGINT {
		return 130
	}
	return 1
}

// ipmiOutput runs ipmitool with a timeout and returns combined output.
func ipmiOutput(h config.Host, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd, err := ipmiCmd(ctx, h, args...)
	if err != nil {
		return "", err
	}
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = &console.TimeoutError{Host: hostLabel(h, h.IP), Via: "IPMI", After: max(time.Since(start), timeout), Cause: ctx.Err()}
	}
	return string(out), err
}

// bootArgs builds the ipmitool args for setting the boot device. UEFI
// (options=efiboot) is the default; legacy omits it.
func bootArgs(dev string, legacy, persistent bool) []string {
	args := []string{"chassis", "bootdev", dev}
	var opts []string
	if persistent {
		opts = append(opts, "persistent")
	}
	if !legacy {
		opts = append(opts, "efiboot")
	}
	if len(opts) > 0 {
		args = append(args, "options="+strings.Join(opts, ","))
	}
	return args
}

// ipmiUIDArgs maps a uid argument to ipmitool args. IPMI has no way to read the
// LED state, so a bare `uid` only works on an iLO or iDRAC.
func ipmiUIDArgs(arg string) ([]string, error) {
	switch arg {
	case "on":
		return []string{"chassis", "identify", "force"}, nil
	case "off":
		return []string{"chassis", "identify", "0"}, nil
	case "":
		return nil, errors.New("reading the UID state is only supported on iLO and iDRAC hosts (type ilo or idrac); use `uid <name> on` or `off`")
	}
	return nil, fmt.Errorf("uid takes on or off, got %q", arg)
}
