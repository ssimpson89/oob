package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func pinnedClient(t *testing.T, srv *httptest.Server) (*redfishClient, string) {
	t.Helper()
	c := newRedfishClient(srv.URL, "admin", "secret")
	c.name = "bmc1"
	c.pinKey = strings.TrimPrefix(srv.URL, "https://")
	kh := filepath.Join(t.TempDir(), "known_hosts")
	c.pinPath = pinsPath(kh)
	return c, kh
}

func TestRedfishPinTOFUAndMismatch(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"PowerState":"On"}`)
	}))
	defer srv.Close()
	c, kh := pinnedClient(t, srv)

	// First use pins the leaf key.
	if _, err := c.PowerState(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(c.pinPath)
	fs := strings.Fields(string(b))
	if len(fs) != 2 || fs[0] != c.pinKey || len(fs[1]) != 64 {
		t.Fatalf("pin file %q", b)
	}
	if _, err := c.PowerState(context.Background()); err != nil {
		t.Fatalf("second use with the same key: %v", err)
	}

	// A different key under the same host:port is a hard error, never a fallback.
	os.WriteFile(c.pinPath, []byte(c.pinKey+" "+strings.Repeat("0", 64)+"\n"), 0o600)
	_, err := c.PowerState(context.Background())
	var pm *pinMismatchError
	if err == nil || !errors.As(err, &pm) {
		t.Fatalf("want pin mismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "`oob ssh bmc1 --reset-hostkey`") {
		t.Errorf("no reset advice: %v", err)
	}
	if redfishFallback(err) {
		t.Error("a pin mismatch must not fall back to another transport")
	}
	if err := c.Reset(context.Background(), "On"); !errors.As(err, &pm) {
		t.Errorf("reset over a mismatched pin: %v", err)
	}

	// --reset-hostkey clears the pin (resetHostKey covers known_hosts and pins).
	host, _, _ := net.SplitHostPort(c.pinKey)
	if err := resetHostKey(kh, net.JoinHostPort(host, "22")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PowerState(context.Background()); err != nil {
		t.Fatalf("after reset: %v", err)
	}
	if b, _ := os.ReadFile(c.pinPath); !strings.HasPrefix(string(b), c.pinKey+" ") || strings.Count(string(b), "\n") != 1 {
		t.Errorf("not re-pinned cleanly: %q", b)
	}
}

func TestRedfishRedirectBlocked(t *testing.T) {
	var hits int
	var mu sync.Mutex
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := newRedfishClient(srv.URL, "admin", "secret")
	_, err := c.PowerState(context.Background())
	var re *redfishError
	if !errors.As(err, &re) || re.Status != http.StatusTemporaryRedirect {
		t.Fatalf("want the 307 as an error, got %v", err)
	}
	if err := c.Reset(context.Background(), "On"); !errors.As(err, &re) {
		t.Errorf("reset: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Errorf("redirect target was contacted %d times (credentials would have followed)", hits)
	}
	if redfishFallback(err) {
		t.Error("a redirect must not fall back")
	}
}

func TestRedfishTLSFailureIsHard(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer plain.Close()
	c := newRedfishClient("https://"+strings.TrimPrefix(plain.URL, "http://"), "u", "p")
	_, err := c.PowerState(context.Background())
	if err == nil {
		t.Fatal("expected a TLS failure talking https to an http server")
	}
	if redfishFallback(err) {
		t.Errorf("TLS handshake failure fell back: %v", err)
	}
}

func TestRedfishMessage(t *testing.T) {
	msg, ids := redfishMessage([]byte(`{"error":{"code":"x","message":"outer","@Message.ExtendedInfo":[
		{"MessageId":"iLO.2.1.One","Message":"first \u001b]0;t\u0007problem"},
		{"MessageId":"Base.1.0.Second"},
		{"Message":"third"}]}}`))
	if msg != "first problem; Base.1.0.Second; third" {
		t.Errorf("msg %q", msg)
	}
	if len(ids) != 2 || ids[1] != "Base.1.0.Second" {
		t.Errorf("ids %v", ids)
	}
	if msg, _ := redfishMessage([]byte(`{"error":{"message":"only outer"}}`)); msg != "only outer" {
		t.Errorf("%q", msg)
	}
	if msg, _ := redfishMessage([]byte("<html>\x1b[2Jbad gateway</html>")); msg != "<html>bad gateway</html>" {
		t.Errorf("%q", msg)
	}
}

// rfSrv is a Redfish fake. handler sees every request; calls records them.
type rfSrv struct {
	mu    sync.Mutex
	calls []string
}

func (s *rfSrv) log(r *http.Request, body string) {
	s.mu.Lock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path+" "+body)
	s.mu.Unlock()
}

func (s *rfSrv) got() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.calls, "|")
}

// newTestILO returns an iLO driver whose Redfish talks to a fake with handler,
// and whose SSH CLI points at a closed port, so reaching the CLI shows up as a
// connection error.
func newTestILO(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body string)) (*iloDriver, *rfSrv) {
	t.Helper()
	rec := &rfSrv{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.log(r, string(b))
		handler(w, r, string(b))
	}))
	t.Cleanup(srv.Close)
	swap(t, &cfgFlag, filepath.Join(t.TempDir(), "hosts.json"))
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	h := Host{Name: "x", IP: "127.0.0.1", Port: closed, User: "u", Pass: "p", Type: typeILO, ILOGen: 7}
	d := &iloDriver{ipmiDriver: ipmiDriver{"x", h}, rf: newRedfishClient(srv.URL, "u", "p")}
	d.rf.name = "x"
	return d, rec
}

const sysPath = "/redfish/v1/Systems/1"

func TestILODriverPowerBoot(t *testing.T) {
	jsonErr := func(w http.ResponseWriter, status int, id, msg string) {
		w.WriteHeader(status)
		io.WriteString(w, `{"error":{"@Message.ExtendedInfo":[{"MessageId":"`+id+`","Message":"`+msg+`"}]}}`)
	}
	okHandler := func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"PowerState":"On","Actions":{"#ComputerSystem.Reset":{"ResetType@Redfish.AllowableValues":["On","ForceOff","PowerCycle"]}}}`)
			return
		}
		io.WriteString(w, `{}`)
	}
	noCycle := func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"PowerState":"On","Actions":{"#ComputerSystem.Reset":{"ResetType@Redfish.AllowableValues":["On","ForceOff","ForceRestart"]}}}`)
			return
		}
		io.WriteString(w, `{}`)
	}
	status := func(code int) func(http.ResponseWriter, *http.Request, string) {
		return func(w http.ResponseWriter, r *http.Request, _ string) { jsonErr(w, code, "Base.1.0.Err", "nope") }
	}
	notFoundSystem := func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Path == "/redfish/v1" {
			io.WriteString(w, `{}`) // the service exists
			return
		}
		jsonErr(w, 404, "Base.1.0.ResourceMissing", "no such system")
	}
	noService := func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(404) }
	alreadyOn := func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"PowerState":"On"}`)
			return
		}
		jsonErr(w, 400, "iLO.2.14.InvalidOperationForSystemState", "invalid state")
	}
	otherRefusal := func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"PowerState":"On"}`)
			return
		}
		jsonErr(w, 400, "Base.1.0.PropertyValueNotInList", "bad value")
	}

	const (
		viaRedfish = iota // success, no fallback
		fellBack          // the SSH CLI was tried (closed port)
		hardErr           // Redfish error is the answer; CLI not tried
	)
	tests := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request, string)
		do      func(d *iloDriver) error
		want    int
		calls   string // exact requests, when checked
		errHas  string
	}{
		{"power on ok", okHandler, func(d *iloDriver) error { return d.Power("on") }, viaRedfish,
			"POST " + sysPath + "/Actions/ComputerSystem.Reset {\"ResetType\":\"On\"}", ""},
		{"cycle uses PowerCycle when listed", okHandler, func(d *iloDriver) error { return d.Power("cycle") }, viaRedfish,
			"GET " + sysPath + " |POST " + sysPath + "/Actions/ComputerSystem.Reset {\"ResetType\":\"PowerCycle\"}", ""},
		{"cycle falls to ForceRestart", noCycle, func(d *iloDriver) error { return d.Power("cycle") }, viaRedfish,
			"GET " + sysPath + " |POST " + sysPath + "/Actions/ComputerSystem.Reset {\"ResetType\":\"ForceRestart\"}", ""},
		{"reset is ForceRestart without a GET", okHandler, func(d *iloDriver) error { return d.Power("reset") }, viaRedfish,
			"POST " + sysPath + "/Actions/ComputerSystem.Reset {\"ResetType\":\"ForceRestart\"}", ""},
		{"401 falls back", status(401), func(d *iloDriver) error { return d.Power("off") }, fellBack, "", ""},
		{"403 falls back", status(403), func(d *iloDriver) error { return d.Power("off") }, fellBack, "", ""},
		{"500 does not fall back", status(500), func(d *iloDriver) error { return d.Power("off") }, hardErr, "", "HTTP 500"},
		{"404 on the system path does not fall back", notFoundSystem, func(d *iloDriver) error { return d.Power("off") }, hardErr, "", "HTTP 404"},
		{"404 without a service falls back", noService, func(d *iloDriver) error { return d.Power("off") }, fellBack, "", ""},
		{"already on is success by MessageId", alreadyOn, func(d *iloDriver) error { return d.Power("on") }, viaRedfish, "", ""},
		{"already on does not excuse other refusals", otherRefusal, func(d *iloDriver) error { return d.Power("on") }, hardErr, "", "HTTP 400"},
		{"already on never applies to cycle", alreadyOn, func(d *iloDriver) error { return d.Power("reset") }, hardErr, "", "HTTP 400"},
		{"boot pxe omits the mode", okHandler, func(d *iloDriver) error { return d.Boot("pxe", false, false) }, viaRedfish,
			"PATCH " + sysPath + " {\"Boot\":{\"BootSourceOverrideEnabled\":\"Once\",\"BootSourceOverrideTarget\":\"Pxe\"}}", ""},
		{"boot pxe legacy sends it", okHandler, func(d *iloDriver) error { return d.Boot("pxe", true, false) }, viaRedfish,
			"PATCH " + sysPath + " {\"Boot\":{\"BootSourceOverrideEnabled\":\"Once\",\"BootSourceOverrideMode\":\"Legacy\",\"BootSourceOverrideTarget\":\"Pxe\"}}", ""},
		{"boot disk is Hdd", okHandler, func(d *iloDriver) error { return d.Boot("disk", false, false) }, viaRedfish,
			"PATCH " + sysPath + " {\"Boot\":{\"BootSourceOverrideEnabled\":\"Once\",\"BootSourceOverrideTarget\":\"Hdd\"}}", ""},
		{"boot bios is BiosSetup", okHandler, func(d *iloDriver) error { return d.Boot("bios", false, false) }, viaRedfish,
			"PATCH " + sysPath + " {\"Boot\":{\"BootSourceOverrideEnabled\":\"Once\",\"BootSourceOverrideTarget\":\"BiosSetup\"}}", ""},
		{"boot 500 is the answer", status(500), func(d *iloDriver) error { return d.Boot("pxe", false, false) }, hardErr, "", "HTTP 500"},
		{"boot disk 401 explains the CLI limit", status(401), func(d *iloDriver) error { return d.Boot("disk", false, false) }, hardErr, "", "only pxe"},
		{"boot persistent is refused", okHandler, func(d *iloDriver) error { return d.Boot("pxe", false, true) }, hardErr, "", "--persistent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, rec := newTestILO(t, tc.handler)
			err := tc.do(d)
			switch tc.want {
			case viaRedfish:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			case fellBack:
				if err == nil || !(strings.Contains(err.Error(), "refused") || strings.Contains(err.Error(), "connect")) {
					t.Fatalf("expected the CLI connection to fail, got %v", err)
				}
			case hardErr:
				if err == nil || !strings.Contains(err.Error(), tc.errHas) || strings.Contains(err.Error(), "refused") {
					t.Fatalf("want an error with %q and no CLI attempt, got %v", tc.errHas, err)
				}
			}
			if tc.calls != "" && rec.got() != tc.calls {
				t.Errorf("requests\n got %s\nwant %s", rec.got(), tc.calls)
			}
		})
	}
}

// A state-changing request that times out says the result is unknown, is sent
// once, and never falls back to the CLI.
func TestILODriverResetTimeoutIsUnknown(t *testing.T) {
	release := make(chan struct{})
	var posts int
	var mu sync.Mutex
	d, _ := newTestILO(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.Method == http.MethodPost {
			mu.Lock()
			posts++
			mu.Unlock()
			<-release
		}
	})
	t.Cleanup(func() { close(release) })
	d.rf.hc.Timeout = 150 * time.Millisecond
	err := d.Power("off")
	if err == nil || !strings.Contains(err.Error(), "result unknown; check `oob power x` before retrying") || !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "refused") {
		t.Error("fell back to the CLI after a timeout")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Errorf("reset POST sent %d times", posts)
	}
}

// A status read gets one deadline for Redfish and the CLI together, and a
// timeout is worded for a person.
func TestILOStatusDeadline(t *testing.T) {
	release := make(chan struct{})
	d, _ := newTestILO(t, func(w http.ResponseWriter, r *http.Request, _ string) { <-release })
	t.Cleanup(func() { close(release) })
	start := time.Now()
	r := d.PowerStatus(300 * time.Millisecond)
	if !r.failed || !strings.HasPrefix(r.text, "ERROR: timed out after") || !strings.Contains(r.text, "(Redfish)") {
		t.Errorf("%+v", r)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v for a 300ms deadline", time.Since(start))
	}
}
