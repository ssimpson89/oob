package bmc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/detect"
	"github.com/ssimpson89/oob/internal/share"
)

// Only the start of the output counts: up to the first prompt or the first 4KB.
// Redfish.

type rfCall struct{ method, path, body, user string }

func fakeRedfish(t *testing.T, status int, power string) (*redfishClient, *[]rfCall, func()) {
	t.Helper()
	var mu sync.Mutex
	var calls []rfCall
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u, _, _ := r.BasicAuth()
		mu.Lock()
		calls = append(calls, rfCall{r.Method, r.URL.Path, string(b), u})
		mu.Unlock()
		if status != 200 {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"@Message.ExtendedInfo":[{"Message":"nope"}]}}`)
			return
		}
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"PowerState":"`+power+`"}`)
			return
		}
		io.WriteString(w, `{}`)
	}))
	return newRedfishClient(srv.URL, "admin", "secret"), &calls, srv.Close
}

func TestRedfishPowerState(t *testing.T) {
	c, calls, done := fakeRedfish(t, 200, "On")
	defer done()
	st, err := c.PowerState(context.Background())
	if err != nil || st != "On" {
		t.Fatalf("%q %v", st, err)
	}
	if got := (*calls)[0]; got.method != "GET" || got.path != "/redfish/v1/Systems/1" || got.user != "admin" {
		t.Errorf("%+v", got)
	}
}

func TestRedfishResetBody(t *testing.T) {
	c, calls, done := fakeRedfish(t, 200, "On")
	defer done()
	if err := c.Reset(context.Background(), "ForceRestart"); err != nil {
		t.Fatal(err)
	}
	got := (*calls)[0]
	if got.method != "POST" || got.path != "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset" || got.body != `{"ResetType":"ForceRestart"}` {
		t.Errorf("%+v", got)
	}
}

func TestRedfishBootBody(t *testing.T) {
	c, calls, done := fakeRedfish(t, 200, "On")
	defer done()
	if err := c.BootOnce(context.Background(), "Pxe", false); err != nil {
		t.Fatal(err)
	}
	if err := c.BootOnce(context.Background(), "Pxe", true); err != nil {
		t.Fatal(err)
	}
	if err := c.BootOnce(context.Background(), "BiosSetup", false); err != nil {
		t.Fatal(err)
	}
	// The mode is sent only for --legacy; otherwise the BMC keeps its own.
	for i, want := range []string{
		`{"Boot":{"BootSourceOverrideEnabled":"Once","BootSourceOverrideTarget":"Pxe"}}`,
		`{"Boot":{"BootSourceOverrideEnabled":"Once","BootSourceOverrideMode":"Legacy","BootSourceOverrideTarget":"Pxe"}}`,
		`{"Boot":{"BootSourceOverrideEnabled":"Once","BootSourceOverrideTarget":"BiosSetup"}}`,
	} {
		got := (*calls)[i]
		if got.method != "PATCH" || got.path != "/redfish/v1/Systems/1" || got.body != want {
			t.Errorf("%d: %+v", i, got)
		}
	}
	for dev, want := range map[string]string{"pxe": "Pxe", "disk": "Hdd", "bios": "BiosSetup"} {
		if got, ok := redfishBootTarget(dev); !ok || got != want {
			t.Errorf("%s: %q %v", dev, got, ok)
		}
	}
}

func TestRedfishFallbackClassification(t *testing.T) {
	c, _, done := fakeRedfish(t, 401, "")
	_, err := c.PowerState(context.Background())
	done()
	if err == nil || !redfishFallback(err) || !strings.Contains(err.Error(), "nope") {
		t.Errorf("401 should fall back and carry the message: %v", err)
	}
	c, _, done = fakeRedfish(t, 500, "")
	err = c.Reset(context.Background(), "On")
	done()
	if err == nil || redfishFallback(err) {
		t.Errorf("500 must not fall back: %v", err)
	}
	// Nothing listening: connection refused.
	c, _, done = fakeRedfish(t, 200, "On")
	done()
	_, err = c.PowerState(context.Background())
	if err == nil || !redfishFallback(err) {
		t.Errorf("refused connection should fall back: %v", err)
	}
	if redfishFallback(errors.New("boom")) {
		t.Error("unknown error should not fall back")
	}
	// 404 falls back only when the service root is missing too.
	if redfishFallback(&redfishError{Path: "/redfish/v1/Systems/1", Status: 404}) {
		t.Error("404 on the system path must not fall back")
	}
	if !redfishFallback(&redfishError{Path: "/redfish/v1", Status: 404}) || !redfishFallback(&redfishError{Path: "/redfish/v1/Systems/1", Status: 404, NoService: true}) {
		t.Error("no Redfish service should fall back")
	}
}

func TestRedfishResetTypes(t *testing.T) {
	for act, want := range map[string]string{"on": "On", "off": "ForceOff", "cycle": "ForceRestart", "reset": "ForceRestart", "soft": "GracefulShutdown"} {
		if got, ok := redfishResetType(act); !ok || got != want {
			t.Errorf("%s: %q %v", act, got, ok)
		}
	}
	if _, ok := redfishResetType("status"); ok {
		t.Error("status has no reset type")
	}
}

// The iLO driver falls back to the SSH CLI only when Redfish is unusable.
func TestILODriverViaRedfish(t *testing.T) {
	c, _, done := fakeRedfish(t, 401, "")
	defer done()
	d := &iloDriver{ipmiDriver: ipmiDriver{"x", config.Host{IP: "192.0.2.1", User: "u", Pass: "p"}}, rf: c}
	handled, err := d.viaRedfish(context.Background(), true, func(ctx context.Context) error { _, err := c.PowerState(ctx); return err })
	if handled || err != nil {
		t.Errorf("401: handled=%v err=%v", handled, err)
	}
	c2, _, done2 := fakeRedfish(t, 500, "")
	defer done2()
	handled, err = d.viaRedfish(context.Background(), true, func(ctx context.Context) error { return c2.Reset(ctx, "On") })
	if !handled || err == nil {
		t.Errorf("500: handled=%v err=%v", handled, err)
	}
	c3, _, done3 := fakeRedfish(t, 200, "Off")
	defer done3()
	d.rf = c3
	if r := d.PowerStatus(5 * time.Second); r.Failed || r.Text != "Chassis Power is off" {
		t.Errorf("%+v", r)
	}
	d.h.Pass = ""
	t.Setenv("SSH_AUTH_SOCK", "")
	if handled, err = d.viaRedfish(context.Background(), true, func(context.Context) error { return nil }); !handled || err == nil {
		t.Errorf("missing password should be an error, not a fallback: %v %v", handled, err)
	}
	// with an SSH key the SSH CLI can log in, so it takes over
	d.h.Key = "/some/key"
	if handled, err = d.viaRedfish(context.Background(), true, func(context.Context) error { return nil }); handled || err != nil {
		t.Errorf("key and no password should use the SSH CLI: %v %v", handled, err)
	}
}

// iDRAC.

func TestIDRACParsing(t *testing.T) {
	for in, want := range map[string]string{
		"Server power status: ON\n":        "on",
		"server Power Status:  off":        "off",
		"\r\nServer power status: OFF\r\n": "off",
	} {
		if got, ok := idracPowerState(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	if _, ok := idracPowerState("garbage"); ok {
		t.Error("garbage parsed")
	}
	if err := racadmError("OK\nERROR: Unable to perform requested operation\n"); err == nil || !strings.Contains(err.Error(), "Unable to perform") {
		t.Errorf("%v", err)
	}
	if err := racadmError("Server power operation initiated successfully\nNo errors here\n"); err != nil {
		t.Errorf("false error: %v", err)
	}
	for act, want := range map[string]string{"status": "powerstatus", "on": "powerup", "off": "powerdown", "cycle": "powercycle", "reset": "hardreset", "soft": "graceshutdown"} {
		if got, err := idracPowerCmd(act); err != nil || got != want {
			t.Errorf("%s: %q %v", act, got, err)
		}
	}
	if _, err := idracPowerCmd("bogus"); err == nil {
		t.Error("bogus accepted")
	}
	for s, want := range map[string]bool{"/admin1-> ": true, "admin1->": true, "racadm>> ": true, "</>hpeiLO-> ": false, "$ ": false} {
		if idracPrompt.MatchString(s) != want {
			t.Errorf("prompt %q want %v", s, want)
		}
	}
}

type fakeRacadm struct {
	cmds      []string
	timeouts  []time.Duration
	noPrompts []bool
	out       map[string]string
	err       error
}

func (f *fakeRacadm) run(cmd string, to time.Duration, noPrompt bool) (string, error) {
	f.cmds = append(f.cmds, cmd)
	f.timeouts = append(f.timeouts, to)
	f.noPrompts = append(f.noPrompts, noPrompt)
	return f.out[cmd], f.err
}

func newFakeIDRAC(f *fakeRacadm) *idracDriver {
	return &idracDriver{ipmiDriver: ipmiDriver{"d", config.Host{IP: "192.0.2.9"}}, run: f.run}
}

func TestIDRACCommands(t *testing.T) {
	swap(t, &untestedDone, false)
	var buf strings.Builder
	swap[io.Writer](t, &untestedOut, &buf)

	f := &fakeRacadm{out: map[string]string{"racadm serveraction powerstatus": "Server power status: ON\n"}}
	d := newFakeIDRAC(f)
	if r := d.PowerStatus(time.Second); r.Failed || r.Text != "Chassis Power is on" {
		t.Errorf("%+v", r)
	}
	// The status read passes its budget on and never prompts; actions may.
	if len(f.timeouts) != 1 || f.timeouts[0] != time.Second || !f.noPrompts[0] {
		t.Errorf("status run: timeouts %v noPrompts %v", f.timeouts, f.noPrompts)
	}
	for _, c := range []struct {
		do   func() error
		want []string
	}{
		{func() error { return d.Power("cycle") }, []string{"racadm serveraction powercycle"}},
		{func() error { return d.Power("soft") }, []string{"racadm serveraction graceshutdown"}},
		{func() error { return d.Boot("pxe", false, false) }, []string{"racadm set iDRAC.ServerBoot.BootOnce Enabled", "racadm set iDRAC.ServerBoot.FirstBootDevice PXE"}},
		{func() error { return d.Boot("pxe", false, true) }, []string{"racadm set iDRAC.ServerBoot.BootOnce Disabled", "racadm set iDRAC.ServerBoot.FirstBootDevice PXE"}},
		{func() error { return d.MCReset("cold") }, []string{"racadm racreset"}},
		{func() error { return d.UID("on") }, []string{"racadm setled -l 1"}},
		{func() error { return d.UID("off") }, []string{"racadm setled -l 0"}},
		{func() error { return d.UID("") }, []string{"racadm getled"}},
		{func() error { return d.NMI() }, []string{"racadm serveraction nmi"}},
		{func() error { return d.SEL("list") }, []string{"racadm getsel"}},
		{func() error { return d.SEL("clear") }, []string{"racadm clrsel"}},
	} {
		f.cmds = nil
		if err := c.do(); err != nil {
			t.Errorf("%v: %v", c.want, err)
		}
		if strings.Join(f.cmds, "|") != strings.Join(c.want, "|") {
			t.Errorf("got %v want %v", f.cmds, c.want)
		}
	}
	if err := d.Boot("pxe", true, false); err == nil {
		t.Error("--legacy accepted")
	}
	if err := d.Boot("disk", false, false); err == nil {
		t.Error("disk accepted")
	}
	if got := strings.Count(buf.String(), "untested"); got != 1 || !strings.HasPrefix(buf.String(), "note: Dell iDRAC support is untested") {
		t.Errorf("untested note printed %d times: %q", got, buf.String())
	}

	// Step 2 failing says step 1 was applied.
	f = &fakeRacadm{out: map[string]string{"racadm set iDRAC.ServerBoot.FirstBootDevice PXE": "ERROR: RAC947: Invalid object value\n"}}
	if err := newFakeIDRAC(f).Boot("pxe", false, false); err == nil || !strings.Contains(err.Error(), "step 1 was applied") || len(f.cmds) != 2 {
		t.Errorf("step 2 failure: %v %v", err, f.cmds)
	}
	// Step 1 failing stops before step 2.
	f = &fakeRacadm{out: map[string]string{"racadm set iDRAC.ServerBoot.BootOnce Enabled": "ERROR: nope\n"}}
	if err := newFakeIDRAC(f).Boot("pxe", false, false); err == nil || len(f.cmds) != 1 {
		t.Errorf("step 1 failure: %v %v", err, f.cmds)
	}
	// A timed-out state change says the result is unknown.
	f = &fakeRacadm{err: &console.TimeoutError{Host: "d", Via: "SSH", After: 20 * time.Second}}
	if err := newFakeIDRAC(f).Power("cycle"); err == nil || !strings.Contains(err.Error(), "result unknown; check `oob power d`") {
		t.Errorf("timeout: %v", err)
	}
	// An ERROR line fails the command even with a zero exit status.
	f = &fakeRacadm{out: map[string]string{"racadm serveraction powerup": "ERROR: Server is already powered ON\n"}}
	if err := newFakeIDRAC(f).Power("on"); err == nil || !strings.Contains(err.Error(), "already powered") {
		t.Errorf("%v", err)
	}
	f = &fakeRacadm{err: errors.New("exit status 1")}
	if r := newFakeIDRAC(f).PowerStatus(time.Second); !r.Failed {
		t.Errorf("%+v", r)
	}
	// No doubled labels, no escape sequences in the table cell.
	f = &fakeRacadm{out: map[string]string{"racadm serveraction powerstatus": "ERROR: \x1b]0;pwn\x07bad\x1b[2J thing\n"}}
	if r := newFakeIDRAC(f).PowerStatus(time.Second); !r.Failed || r.Text != "ERROR: racadm: bad thing" {
		t.Errorf("%q", r.Text)
	}
}

// Supermicro.

func TestCipherFailure(t *testing.T) {
	for s, want := range map[string]bool{
		"Error: Unable to establish IPMI v2 / RMCP+ session":                true,
		"no matching cipher suite":                                          true,
		"Error in open session response message : no matching cipher suite": true,
		"Error: Unauthorized name":                                          false,
		"":                                                                  false,
	} {
		if cipherFailure(s) != want {
			t.Errorf("%q want %v", s, want)
		}
	}
	for s, want := range map[string]bool{
		"Error: Unable to establish IPMI v2 / RMCP+ session":                false,
		"Error in open session response message : no matching cipher suite": true,
		"": false,
	} {
		if cipherSpecific(s) != want {
			t.Errorf("specific %q want %v", s, want)
		}
	}
}

func TestSmcRetry(t *testing.T) {
	swap[io.Writer](t, &smcLog, io.Discard)

	type call struct {
		cipher string
		quiet  bool
	}
	run := func(results map[string]ipmiRun, calls *[]call) func(config.Host, bool) ipmiRun {
		return func(h config.Host, quiet bool) ipmiRun {
			*calls = append(*calls, call{h.Cipher, quiet})
			return results[h.Cipher]
		}
	}
	generic := ipmiRun{code: 1, out: "Error: Unable to establish IPMI v2 / RMCP+ session"}
	specific := ipmiRun{code: 1, out: "Error in open session response message : no matching cipher suite\nError: Unable to establish IPMI v2 / RMCP+ session"}
	good := ipmiRun{}

	var calls []call
	h, r, sw := smcRetry(config.Host{}, true, run(map[string]ipmiRun{"17": good}, &calls))
	if h.Cipher != "17" || !r.ok() || sw.used || len(calls) != 1 || !calls[0].quiet {
		t.Errorf("17 works: %v %v %v %v", h.Cipher, r, sw, calls)
	}

	// A cipher specific failure: use 3 and save it.
	calls = nil
	h, r, sw = smcRetry(config.Host{}, true, run(map[string]ipmiRun{"17": specific, "3": good}, &calls))
	if h.Cipher != "3" || !r.ok() || !sw.used || !sw.persist || len(calls) != 2 || calls[1].quiet {
		t.Errorf("fallback to 3: %v %v %v %v", h.Cipher, r, sw, calls)
	}

	// A generic session failure: 3 may be tried for this run, but never saved.
	calls = nil
	h, r, sw = smcRetry(config.Host{}, true, run(map[string]ipmiRun{"17": generic, "3": good}, &calls))
	if h.Cipher != "3" || !r.ok() || !sw.used || sw.persist || len(calls) != 2 {
		t.Errorf("generic failure must not persist: %v %v %v %v", h.Cipher, r, sw, calls)
	}

	calls = nil
	h, r, sw = smcRetry(config.Host{}, true, run(map[string]ipmiRun{"17": specific, "3": generic}, &calls))
	if r.ok() || sw.used || len(calls) != 2 {
		t.Errorf("both fail: %v %v %v", r, sw, calls)
	}

	calls = nil
	other := ipmiRun{code: 1, out: "Error: Unauthorized name"}
	_, r, sw = smcRetry(config.Host{}, false, run(map[string]ipmiRun{"17": other}, &calls))
	if r.ok() || sw.used || len(calls) != 1 {
		t.Errorf("non-cipher error must not retry: %v %v %v", r, sw, calls)
	}

	calls = nil
	h, _, sw = smcRetry(config.Host{Cipher: "8"}, true, run(map[string]ipmiRun{"8": generic}, &calls))
	if h.Cipher != "8" || sw.used || len(calls) != 1 || calls[0].quiet {
		t.Errorf("explicit cipher must be used as is: %v %v", calls, sw)
	}
}

// afterSwitch saves only a justified cipher, and a failed save never escapes.
func TestAfterSwitch(t *testing.T) {
	var saved []string
	swap(t, &saveCipherFn, func(name, cipher string) error {
		saved = append(saved, name+":"+cipher)
		return errors.New("disk full")
	})
	clearNotices(t)

	afterSwitch("a", config.Host{Cipher: "3"}, smcSwitch{used: true, persist: false})
	if len(saved) != 0 {
		t.Errorf("saved without justification: %v", saved)
	}
	afterSwitch("a", config.Host{Cipher: "3"}, smcSwitch{})
	if len(saved) != 0 {
		t.Errorf("saved with no switch: %v", saved)
	}
	afterSwitch("a", config.Host{Cipher: "3"}, smcSwitch{used: true, persist: true}) // must not panic or fail
	if len(saved) != 1 || saved[0] != "a:3" {
		t.Errorf("saved %v", saved)
	}
	notices.Lock()
	defer notices.Unlock()
	if len(notices.msgs) != 2 || !strings.Contains(notices.msgs[1], "could not save") {
		t.Errorf("notices %q", notices.msgs)
	}
}

func TestSupermicroDriverRules(t *testing.T) {
	d := newSupermicroDriver("s", config.Host{IP: "192.0.2.5"})
	if err := d.Boot("pxe", false, true); err == nil || !strings.Contains(err.Error(), "revert") {
		t.Errorf("--persistent: %v", err)
	}
	if err := d.Console(SOLOptions{SSH: true, Raw: true}); err == nil {
		t.Error("--ssh with --raw accepted")
	}
	if err := (&ipmiDriver{"g", config.Host{}}).Console(SOLOptions{SSH: true}); err == nil {
		t.Error("--ssh on generic IPMI accepted")
	}
}

func TestSaveCipher(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts.json")
	c := &config.Config{Hosts: map[string]config.Host{"a": {IP: "192.0.2.1", Pass: "pw"}, "b": {IP: "192.0.2.2", Cipher: "8"}}}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	swap(t, &config.Override, func() string { return p })
	clearNotices(t)
	if err := saveCipher("a", "3"); err != nil {
		t.Fatal(err)
	}
	if err := saveCipher("b", "3"); err != nil {
		t.Fatal(err)
	}
	got, _ := config.LoadConfig(p, nil)
	if got.Hosts["a"].Cipher != "3" || got.Hosts["a"].Pass != "pw" || got.Hosts["b"].Cipher != "8" {
		t.Errorf("%+v", got.Hosts)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}

	// A defaults block that sets a cipher means the host is not written.
	c2 := &config.Config{Defaults: config.Host{Cipher: "17"}, Hosts: map[string]config.Host{"c": {IP: "192.0.2.3"}}}
	if err := c2.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := saveCipher("c", "3"); err != nil {
		t.Fatal(err)
	}
	if got, _ := config.LoadConfig(p, nil); got.Hosts["c"].Cipher != "" {
		t.Errorf("wrote a host cipher although defaults has one: %+v", got.Hosts["c"])
	}
}

func TestDriverFor(t *testing.T) {
	for typ, want := range map[string]string{
		"": "*bmc.ipmiDriver", detect.TypeIPMI: "*bmc.ipmiDriver", detect.TypeILO: "*bmc.iloDriver",
		detect.TypeIDRAC: "*bmc.idracDriver", detect.TypeSupermicro: "*bmc.supermicroDriver",
	} {
		got := typeName(DriverFor("x", config.Host{Type: typ, IP: "192.0.2.1"}))
		if got != want {
			t.Errorf("%q: %s want %s", typ, got, want)
		}
	}
	if !detect.ValidType("idrac") || !detect.ValidType("supermicro") || detect.ValidType("bogus") {
		t.Error("validType")
	}
}

func typeName(v any) string { return fmt.Sprintf("%T", v) }

// Without a terminal Console has no elected opener to clear the stale session
// in, so it must hold the ownership lock itself: with another oob sharing the
// console it refuses instead of ending that session.
func TestSupermicroConsoleWithoutTerminalDoesNotClearASharedConsole(t *testing.T) {
	d, err := os.MkdirTemp("/tmp", "oob") // short: socket paths are limited to about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	t.Setenv("XDG_STATE_HOME", d)
	h := config.Host{IP: "192.0.2.5"}
	addr, _ := share.Endpoint(h)
	release, err := share.Exclusive("s", addr) // another oob has the console
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = newSupermicroDriver("s", h).Console(SOLOptions{})
	if err == nil || !strings.Contains(err.Error(), "another oob") {
		t.Errorf("Console = %v, want a refusal naming the other oob", err)
	}
}

// The untested-vendor note and the racreset note go to the captured output, so
// an MCP caller sees them, not only the terminal.
func TestIDRACNotesAreCaptured(t *testing.T) {
	swap(t, &untestedDone, false)
	d := newFakeIDRAC(&fakeRacadm{out: map[string]string{}})
	out, err := Capture(context.Background(), func() error { return d.MCReset("cold") })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no warm or cold choice") || !strings.Contains(out, "support is untested") {
		t.Errorf("captured output %q", out)
	}
}
