package bmc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/sshx"
)

// A small Redfish client: HTTPS, basic auth, JSON. It only knows the things oob
// needs (power state, reset, one-time boot override) and is vendor neutral, but
// the system path is fixed to /redfish/v1/Systems/1, which is what an HPE iLO
// uses. Dell iDRAC uses /redfish/v1/Systems/System.Embedded.1, and a vendor
// whose member id is unknown must discover it from /redfish/v1/Systems before
// reusing this client; do not point it at another vendor as is.
//
// BMCs ship self-signed certificates, so the TLS key is pinned on first use
// (redfish_pins next to known_hosts) instead of verified against a CA; see
// verifyPin.

const redfishTimeout = 15 * time.Second

const redfishRoot = "/redfish/v1"

// redfishClient talks to one BMC.
type redfishClient struct {
	name   string // the host's name, for messages
	base   string // https://host:port
	system string // the ComputerSystem resource, fixed to Systems/1 (iLO), see above
	user   string
	pass   string
	hc     *http.Client

	pinPath string // redfish_pins file; empty means no TLS pinning (tests)
	pinKey  string // host:port
}

func newRedfishClient(base, user, pass string) *redfishClient {
	c := &redfishClient{
		base: strings.TrimRight(base, "/"), system: "/redfish/v1/Systems/1", user: user, pass: pass,
	}
	c.hc = &http.Client{
		Timeout: redfishTimeout,
		// A redirect would send the Basic credentials wherever the BMC points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			// BMCs ship self-signed certificates, so chain verification is
			// skipped on purpose. The leaf key is pinned on first use instead
			// (see verifyPin): the first contact is trusted, later ones must
			// match.
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec
				VerifyConnection:   c.verifyPin,
			},
			DisableKeepAlives: true,
		},
	}
	return c
}

// redfishFor builds a client for a host's management address, pinning its TLS
// key in redfish_pins next to known_hosts.
func redfishFor(name string, h config.Host) *redfishClient {
	c := newRedfishClient("https://"+net.JoinHostPort(h.IP, "443"), h.User, h.Pass)
	c.name = name
	c.pinKey = net.JoinHostPort(h.IP, "443")
	if kh, err := sshx.KnownHostsPath(); err == nil {
		c.pinPath = pinsPath(kh)
	}
	return c
}

func (c *redfishClient) label() string {
	if c.name != "" {
		return c.name
	}
	return strings.TrimPrefix(c.base, "https://")
}

// TLS pinning.

// pinMismatchError is a Redfish certificate key that differs from the pinned one.
type pinMismatchError struct{ name, key string }

func (e *pinMismatchError) Error() string {
	n := e.name
	if n == "" {
		n = "<name>"
	}
	return fmt.Sprintf("the Redfish TLS key of %s (%s) differs from the one pinned earlier. If the BMC was reimaged or its certificate was replaced, run `%s ssh %s --reset-hostkey` (it also clears this pin); otherwise someone may be impersonating the BMC",
		n, e.key, app.Name, n)
}

var pinMu sync.Mutex

func pinsPath(khPath string) string { return filepath.Join(filepath.Dir(khPath), "redfish_pins") }

// verifyPin is the TLS VerifyConnection hook: the SHA-256 of the leaf's
// SubjectPublicKeyInfo must match the pin for this host:port, which is
// recorded when there is none.
func (c *redfishClient) verifyPin(cs tls.ConnectionState) error {
	if c.pinPath == "" {
		return nil
	}
	if len(cs.PeerCertificates) == 0 {
		return errors.New("the BMC sent no TLS certificate")
	}
	sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
	got := hex.EncodeToString(sum[:])

	pinMu.Lock()
	defer pinMu.Unlock()
	pins, err := readPins(c.pinPath)
	if err != nil {
		return err
	}
	if want, ok := pins[c.pinKey]; ok {
		if want != got {
			return &pinMismatchError{c.name, c.pinKey}
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.pinPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(c.pinPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%s %s\n", c.pinKey, got); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !app.Quiet() {
		fmt.Fprintf(os.Stderr, "Pinned Redfish TLS key sha256:%s for %s in %s\n", got[:16], c.pinKey, c.pinPath)
	}
	return nil
}

func readPins(path string) (map[string]string, error) {
	pins := map[string]string{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return pins, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fs := strings.Fields(sc.Text()); len(fs) == 2 {
			pins[fs[0]] = fs[1]
		}
	}
	return pins, sc.Err()
}

// ResetHostKey removes known_hosts lines for addr (host:port) and the Redfish
// TLS pins of the same host (they live in redfish_pins next to khPath), so one
// --reset-hostkey covers both after a BMC reimage.
func ResetHostKey(khPath, addr string) error {
	if err := sshx.ResetKnownHosts(khPath, addr); err != nil {
		return err
	}
	return resetHostPins(khPath, addr)
}

// resetHostPins forgets the Redfish pins (any port) of the host in addr
// ("host" or "host:port"). The pins file sits next to khPath.
func resetHostPins(khPath, addr string) error {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	path := pinsPath(khPath)
	pinMu.Lock()
	defer pinMu.Unlock()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		fs := strings.Fields(l)
		if len(fs) == 2 {
			h := fs[0]
			if hh, _, err := net.SplitHostPort(h); err == nil {
				h = hh
			}
			if h == host {
				continue
			}
		}
		if strings.TrimSpace(l) != "" {
			keep = append(keep, l)
		}
	}
	data := strings.Join(keep, "\n")
	if len(keep) > 0 {
		data += "\n"
	}
	return config.WriteFileAtomic(path, []byte(data))
}

// redfishError is an HTTP error answer from the BMC.
type redfishError struct {
	Method, Path string
	Status       int
	Msg          string
	MessageIDs   []string
	NoService    bool // a 404 where /redfish/v1 itself is 404 too: no Redfish service
}

func (e *redfishError) Error() string {
	s := fmt.Sprintf("Redfish %s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

// hasMessageID reports whether the answer carried a message whose id ends in suffix.
func (e *redfishError) hasMessageID(suffix string) bool {
	for _, id := range e.MessageIDs {
		if id == suffix || strings.HasSuffix(id, "."+suffix) {
			return true
		}
	}
	return false
}

// redfishFallback reports whether err means "Redfish is not usable here" (nothing
// listening, no Redfish service, or the login is refused), as opposed to a real
// failure of the request. Callers fall back to another transport only for
// these. A TLS failure (including a pin mismatch) never falls back, and neither
// does a 404 on the system path while the service itself exists.
func redfishFallback(err error) bool {
	var pm *pinMismatchError
	if errors.As(err, &pm) {
		return false
	}
	var he *redfishError
	if errors.As(err, &he) {
		switch he.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return true
		case http.StatusNotFound:
			return he.NoService || he.Path == redfishRoot || he.Path == redfishRoot+"/"
		}
		return false
	}
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}

func (c *redfishClient) do(ctx context.Context, method, path string, in any) ([]byte, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.netErr(err, start)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, c.netErr(err, start)
	}
	if resp.StatusCode >= 300 {
		msg, ids := redfishMessage(data)
		re := &redfishError{Method: method, Path: path, Status: resp.StatusCode, Msg: msg, MessageIDs: ids}
		if resp.StatusCode == http.StatusNotFound && path != redfishRoot {
			re.NoService = c.noService(ctx)
		}
		return nil, re
	}
	return data, nil
}

// noService probes the service root once: a 404 there means the BMC has no
// Redfish at all (old firmware), not just no such system.
func (c *redfishClient) noService(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+redfishRoot, nil)
	if err != nil {
		return false
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusNotFound
}

// netErr turns a transport error into a readable one. A timeout names the host
// and how long it waited; the cause stays reachable for errors.As.
func (c *redfishClient) netErr(err error, start time.Time) error {
	if console.IsTimeout(err) {
		return &console.TimeoutError{Host: c.label(), Via: "Redfish", After: time.Since(start), Cause: err}
	}
	return err
}

// redfishMessage pulls a readable message and the message ids out of a Redfish
// error body: every ExtendedInfo entry (its Message, or its MessageId when
// there is no text), else the error's own message. The text is sanitized.
func redfishMessage(data []byte) (string, []string) {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Info    []struct {
				Message   string `json:"Message"`
				MessageID string `json:"MessageId"`
			} `json:"@Message.ExtendedInfo"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && (e.Error.Message != "" || len(e.Error.Info) > 0) {
		var parts, ids []string
		for _, i := range e.Error.Info {
			if i.MessageID != "" {
				ids = append(ids, i.MessageID)
			}
			switch {
			case i.Message != "":
				parts = append(parts, i.Message)
			case i.MessageID != "":
				parts = append(parts, i.MessageID)
			}
		}
		if len(parts) == 0 && e.Error.Message != "" {
			parts = append(parts, e.Error.Message)
		}
		return console.CleanRemoteLine(strings.Join(parts, "; ")), ids
	}
	return console.CleanRemoteLine(string(data)), nil
}

// PowerState returns the system's PowerState as the BMC reports it ("On", "Off").
func (c *redfishClient) PowerState(ctx context.Context) (string, error) {
	data, err := c.do(ctx, http.MethodGet, c.system, nil)
	if err != nil {
		return "", err
	}
	var s struct{ PowerState string }
	if err := json.Unmarshal(data, &s); err != nil {
		return "", fmt.Errorf("Redfish %s: %w", c.system, err)
	}
	if s.PowerState == "" {
		return "", fmt.Errorf("Redfish %s: no PowerState in the answer", c.system)
	}
	return console.CleanRemoteLine(s.PowerState), nil
}

// ResetTypes returns the ResetType values the system allows
// (ResetType@Redfish.AllowableValues), from one GET.
func (c *redfishClient) ResetTypes(ctx context.Context) ([]string, error) {
	data, err := c.do(ctx, http.MethodGet, c.system, nil)
	if err != nil {
		return nil, err
	}
	var s struct {
		Actions map[string]struct {
			Allowed []string `json:"ResetType@Redfish.AllowableValues"`
		}
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("Redfish %s: %w", c.system, err)
	}
	return s.Actions["#ComputerSystem.Reset"].Allowed, nil
}

// Reset posts a ComputerSystem.Reset with the given ResetType. It is sent once:
// a timeout leaves the outcome unknown and the error says so, and nothing here
// retries.
func (c *redfishClient) Reset(ctx context.Context, resetType string) error {
	_, err := c.do(ctx, http.MethodPost, c.system+"/Actions/ComputerSystem.Reset", map[string]string{"ResetType": resetType})
	return c.unknownOnTimeout(err)
}

// unknownOnTimeout marks a timeout of a state-changing request as "result
// unknown". A failure to even connect means nothing was sent.
func (c *redfishClient) unknownOnTimeout(err error) error {
	var te *console.TimeoutError
	if errors.As(err, &te) {
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "dial" {
			return err
		}
		te2 := *te
		te2.Unknown = true
		return &te2
	}
	return err
}

// BootOnce sets a one-time boot override to target ("Pxe", "Hdd", "BiosSetup").
// BootSourceOverrideMode is sent only for legacy; otherwise the BMC keeps its
// own mode.
func (c *redfishClient) BootOnce(ctx context.Context, target string, legacy bool) error {
	boot := map[string]string{
		"BootSourceOverrideTarget":  target,
		"BootSourceOverrideEnabled": "Once",
	}
	if legacy {
		boot["BootSourceOverrideMode"] = "Legacy"
	}
	_, err := c.do(ctx, http.MethodPatch, c.system, map[string]map[string]string{"Boot": boot})
	return c.unknownOnTimeout(err)
}

// redfishBootTarget maps a boot device to a BootSourceOverrideTarget.
func redfishBootTarget(dev string) (string, bool) {
	switch dev {
	case "pxe":
		return "Pxe", true
	case "disk":
		return "Hdd", true
	case "bios":
		return "BiosSetup", true
	}
	return "", false
}

// redfishResetType maps a power action to a ResetType. "status" has none. cycle
// is ForceRestart here; the caller upgrades it to PowerCycle when the BMC lists it.
func redfishResetType(act string) (string, bool) {
	switch act {
	case "on":
		return "On", true
	case "off":
		return "ForceOff", true
	case "cycle", "reset":
		return "ForceRestart", true
	case "soft":
		return "GracefulShutdown", true
	}
	return "", false
}
