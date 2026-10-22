package bmc

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const okPower = "power\r\n\r\nstatus=0\r\nstatus_tag=COMMAND COMPLETED\r\n\r\npower: server power is currently: On\r\n\r\n</>hpeiLO-> "

func TestParseILOResponse(t *testing.T) {
	tests := []struct {
		name, cmd, raw string
		status         int
		tag, etag      string
		body           string
		fails          bool
	}{
		{"ok", "power", okPower, 0, "COMMAND COMPLETED", "", "power: server power is currently: On", false},
		{"failed", "power on",
			"power on\r\nstatus=2\r\nstatus_tag=COMMAND PROCESSING FAILED\r\nerror_tag=SERVER ALREADY POWERED ON\r\n</>hpeiLO-> ",
			2, "COMMAND PROCESSING FAILED", "SERVER ALREADY POWERED ON", "", true},
		{"ansi and lone CR", "uid", "\x1b[0muid\rstatus=0\rstatus_tag=COMMAND COMPLETED\r\nuid: UID is Off\r\n</map1>hpeiLO-> ", 0, "COMMAND COMPLETED", "", "uid: UID is Off", false},
		{"live iLO 7 power", "power", "power\r\nThe host power is ON.\r\nCOMMAND COMPLETED\r\n\r\n</>hpeiLO-> ", 0, "COMMAND COMPLETED", "", "The host power is ON.", false},
		{"live iLO 7 uid", "uid", "uid\r\nThe UID light is currently: Off\r\n\r\n\r\n</>hpeiLO-> ", -1, "", "", "The UID light is currently: Off", false},
		{"bare failure", "power on", "power on\r\nSomething went wrong.\r\nCOMMAND PROCESSING FAILED\r\n</>hpeiLO-> ", 2, "COMMAND PROCESSING FAILED", "", "Something went wrong.", true},
		{"no status", "help", "help\r\nsome text\r\n</>hpeiLO-> ", -1, "", "", "some text", false},
		{"echo case", "POWER", "power\r\nstatus=0\r\nstatus_tag=X\r\n</>hpeiLO-> ", 0, "X", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := parseILOResponse(tc.cmd, tc.raw)
			if r.Status != tc.status || r.StatusTag != tc.tag || r.ErrorTag != tc.etag || r.Body != tc.body {
				t.Errorf("got %+v", r)
			}
			if err := r.Err(tc.cmd); (err != nil) != tc.fails {
				t.Errorf("Err = %v, want failure=%v", err, tc.fails)
			}
		})
	}
	err := parseILOResponse("power on", "power on\r\nstatus=2\r\nstatus_tag=COMMAND PROCESSING FAILED\r\nerror_tag=SERVER ALREADY POWERED ON\r\n</>hpeiLO-> ").Err("power on")
	if err == nil || !strings.Contains(err.Error(), "SERVER ALREADY POWERED ON") || !strings.Contains(err.Error(), "COMMAND PROCESSING FAILED") {
		t.Errorf("error text lost the iLO's reason: %v", err)
	}
}

func TestPowerState(t *testing.T) {
	for body, want := range map[string]string{
		"The host power is ON.":                 "on",
		"The host power is OFF.":                "off",
		"power: server power is currently: On":  "on",
		"power: server power is currently: Off": "off",
	} {
		if got, ok := powerState(iloResult{Body: body}); !ok || got != want {
			t.Errorf("%q: got %q %v", body, got, ok)
		}
	}
	if _, ok := powerState(iloResult{Body: "nothing useful"}); ok {
		t.Error("matched garbage")
	}
}

func TestPromptRegexp(t *testing.T) {
	for s, want := range map[string]bool{
		"</>hpeiLO-> ":                  true,
		"</map1>hpeiLO-> ":              true,
		"text\r\n</>HPEILO->":           true,
		"</>hpeiLO-> typed":             false,
		"mentions hpeiLO-> in the text": false,
	} {
		if got := iloPrompt.MatchString(s); got != want {
			t.Errorf("%q: got %v want %v", s, got, want)
		}
	}
}

func TestILOCommandMapping(t *testing.T) {
	for act, want := range map[string]string{"status": "power", "on": "power on", "off": "power off"} {
		if got, err := iloPowerCmd(act); err != nil || got != want {
			t.Errorf("power %s: %q, %v", act, got, err)
		}
	}
	// cycle and reset never reach iloPowerCmd (the driver runs iloCycle), so
	// they are unknown to it.
	for _, act := range []string{"cycle", "reset"} {
		if _, err := iloPowerCmd(act); err == nil {
			t.Errorf("power %s accepted", act)
		}
	}
	if _, err := iloPowerCmd("soft"); err == nil || !strings.Contains(err.Error(), "graceful") || !strings.Contains(err.Error(), "IPMI over LAN") {
		t.Errorf("power soft: unhelpful error %v", err)
	}
	if _, err := iloPowerCmd("bogus"); err == nil {
		t.Error("bogus power action accepted")
	}
	if got, err := iloBootCmd("pxe"); err != nil || got != "onetimeboot netdev1" {
		t.Errorf("boot pxe: %q, %v", got, err)
	}
	for _, d := range []string{"disk", "bios"} {
		if _, err := iloBootCmd(d); err == nil {
			t.Errorf("boot %s should be unsupported", d)
		}
	}
	for arg, want := range map[string]string{"": "uid", "on": "uid on", "off": "uid off"} {
		if got, err := iloUIDCmd(arg); err != nil || got != want {
			t.Errorf("uid %q: %q, %v", arg, got, err)
		}
	}
	if _, err := iloUIDCmd("blink"); err == nil {
		t.Error("uid blink accepted")
	}
	if iloNMICmd != "nmi server" {
		t.Error(iloNMICmd)
	}
	if a, err := ipmiUIDArgs("on"); err != nil || strings.Join(a, " ") != "chassis identify force" {
		t.Errorf("%v %v", a, err)
	}
	if _, err := ipmiUIDArgs(""); err == nil {
		t.Error("uid status on generic host should explain it is unsupported")
	}
}

// fakeILO plays an iLO CLI over a pipe pair: it prints a banner and prompt,
// echoes each command, and answers from a table. It records what it received.
type fakeILO struct {
	mu   sync.Mutex
	got  []string
	resp map[string]string
	wr   *io.PipeWriter // to the client
	rd   *io.PipeReader // from the client
}

func newFakeILO(resp map[string]string) (*cliSession, *fakeILO) {
	cr, sw := io.Pipe() // server -> client
	sr, cw := io.Pipe() // client -> server
	f := &fakeILO{resp: resp, wr: sw, rd: sr}
	go func() {
		io.WriteString(sw, "Integrated Lights-Out 7\r\n</>hpeiLO-> ")
		buf := make([]byte, 256)
		var line string
		for {
			n, err := sr.Read(buf)
			if err != nil {
				sw.Close()
				return
			}
			for _, c := range string(buf[:n]) {
				if c != '\r' {
					line += string(c)
					continue
				}
				f.mu.Lock()
				f.got = append(f.got, line)
				body, ok := f.resp[strings.ToLower(line)]
				f.mu.Unlock()
				if !ok {
					body = "status=2\r\nstatus_tag=COMMAND PROCESSING FAILED\r\nerror_tag=COMMAND NOT RECOGNIZED\r\n"
				}
				// Deliver in small pieces to exercise split reads.
				out := line + "\r\n" + body + "</>hpeiLO-> "
				for i := 0; i < len(out); i += 7 {
					io.WriteString(sw, out[i:min(i+7, len(out))])
				}
				line = ""
			}
		}
	}()
	s := newCLISession(cr, cw, iloSpec)
	s.closer = func() { cw.Close(); cr.Close() }
	return s, f
}

func ok(body string) string {
	return "status=0\r\nstatus_tag=COMMAND COMPLETED\r\n" + body
}

func TestCLISessionRun(t *testing.T) {
	s, f := newFakeILO(map[string]string{
		"power":      ok("power: server power is currently: On\r\n"),
		"power on":   "status=2\r\nstatus_tag=COMMAND PROCESSING FAILED\r\nerror_tag=SERVER ALREADY POWERED ON\r\n",
		"uid":        ok("uid: UID is Off\r\n"),
		"nmi server": ok(""),
	})
	defer s.Close()
	var banner strings.Builder
	s.onOut = func(b []byte) { banner.Write(b) }
	if _, err := s.expectPrompt(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(banner.String(), "Integrated Lights-Out") {
		t.Error("banner not passed to onOut")
	}

	r, err := s.Run("power", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := powerState(r); st != "on" {
		t.Errorf("state %q from %+v", st, r)
	}
	if got := iloPowerText("status", r); got != "Chassis Power is on" {
		t.Errorf("status text %q", got)
	}

	_, err = s.Run("power on", 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "SERVER ALREADY POWERED ON") {
		t.Errorf("failure text not returned: %v", err)
	}
	// The session is still usable after a failed command.
	if r, err := s.Run("uid", 2*time.Second); err != nil || !strings.Contains(r.Body, "UID is Off") {
		t.Errorf("uid: %+v %v", r, err)
	}
	if _, err := s.Run("nmi server", 2*time.Second); err != nil {
		t.Error(err)
	}
	if _, err := s.Run("bogus", 2*time.Second); err == nil || !strings.Contains(err.Error(), "COMMAND NOT RECOGNIZED") {
		t.Errorf("unknown command: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.got, "|") != "power|power on|uid|nmi server|bogus" {
		t.Errorf("fake received %q", f.got)
	}
}

func TestCLISessionTimeoutAndClose(t *testing.T) {
	cr, _ := io.Pipe()
	_, cw := io.Pipe()
	s := newCLISession(cr, cw, iloSpec)
	defer s.Close()
	if _, err := s.expectPrompt(50 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("want timeout, got %v", err)
	}

	cr2, sw2 := io.Pipe()
	s2 := newCLISession(cr2, io.Discard, iloSpec)
	sw2.Close()
	if _, err := s2.expectPrompt(time.Second); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("want closed, got %v", err)
	}
}

// After the prompt, the session is the console's reader and loses nothing.
func TestCLISessionHandover(t *testing.T) {
	s, f := newFakeILO(map[string]string{"vsp": "Starting virtual serial port.\r\n"})
	defer s.Close()
	if _, err := s.expectPrompt(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.Send("vsp\r"); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	buf := make([]byte, 64)
	for !strings.Contains(sb.String(), "virtual serial port.") {
		n, err := s.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(buf[:n])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) != 1 || f.got[0] != "vsp" {
		t.Errorf("got %q", f.got)
	}
}

type fakeRunner struct {
	cmds    []string
	states  []string // power states returned by successive `power`
	failOn  string
	failErr error // returned for failOn; "boom" if nil
}

func (r *fakeRunner) Run(cmd string, _ time.Duration) (iloResult, error) {
	r.cmds = append(r.cmds, cmd)
	if cmd == r.failOn {
		if r.failErr != nil {
			return iloResult{Status: 2}, r.failErr
		}
		return iloResult{Status: 2}, errors.New("boom")
	}
	if cmd == "power" {
		st := "Off"
		if len(r.states) > 0 {
			st, r.states = r.states[0], r.states[1:]
		}
		return iloResult{Body: "power: server power is currently: " + st}, nil
	}
	return iloResult{}, nil
}

func TestILOCycle(t *testing.T) {
	r := &fakeRunner{states: []string{"On", "On", "Off"}}
	var slept time.Duration
	msg, err := iloCycle("x", r, func(d time.Duration) { slept += d })
	if err != nil || msg == "" {
		t.Fatal(msg, err)
	}
	if got := strings.Join(r.cmds, "|"); got != "power off|power|power|power|power on" {
		t.Errorf("sequence %q", got)
	}
	if slept < 3*time.Second {
		t.Errorf("no settle delay: %v", slept)
	}

	// Never reports off: must not power on, and the state is called unknown.
	r = &fakeRunner{states: []string{"On", "On", "On", "On", "On", "On", "On", "On", "On", "On", "On", "On", "On", "On", "On", "On"}}
	_, err = iloCycle("x", r, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "state unknown") || !strings.Contains(err.Error(), "`oob power x`") {
		t.Errorf("expected unknown-state error when host stays on: %v", err)
	}
	for _, c := range r.cmds {
		if c == "power on" {
			t.Error("powered on although the host never went off")
		}
	}

	// The iLO refuses power off: nothing changed, stop.
	r = &fakeRunner{failOn: "power off"}
	if _, err := iloCycle("x", r, func(time.Duration) {}); err == nil || len(r.cmds) != 1 {
		t.Errorf("failed off should stop: %v %v", err, r.cmds)
	}

	// Already off is fine: carry on to power on.
	r = &fakeRunner{failOn: "power off", failErr: errors.New(`iLO CLI "power off" failed: COMMAND PROCESSING FAILED: SERVER ALREADY POWERED OFF`), states: []string{"Off"}}
	if _, err := iloCycle("x", r, func(time.Duration) {}); err != nil {
		t.Errorf("already off should go on: %v", err)
	}
	if got := strings.Join(r.cmds, "|"); got != "power off|power|power on" {
		t.Errorf("already off sequence %q", got)
	}

	// Off confirmed, power on fails: the host is off and the error says so.
	r = &fakeRunner{failOn: "power on", states: []string{"Off"}}
	_, err = iloCycle("x", r, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "host is OFF; power on failed: boom") {
		t.Errorf("power on failure: %v", err)
	}

	// No answer to power off: it may have worked.
	r = &fakeRunner{failOn: "power off", failErr: errCLINoReply}
	_, err = iloCycle("x", r, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "state unknown") {
		t.Errorf("no reply: %v", err)
	}
}
