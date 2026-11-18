package bmc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"golang.org/x/term"
)

// OpenBMC (bmcweb) BMCs, such as the NVIDIA GB200/GB300 compute tray BMC ("GB
// BMC"). These often ship with IPMI over LAN off, so everything here goes over
// HTTPS: the serial console is bmcweb's websocket at /console0 (what the web
// UI's SOL page uses), and power and boot are Redfish. The ComputerSystem is
// discovered from /redfish/v1/Systems (System_0 on these BMCs, not iLO's
// Systems/1). The rest (UID, NMI, SEL, mc reset) falls back to IPMI.

const openbmcConsolePath = "/console0"

type openbmcDriver struct {
	ipmiDriver
	rf *redfishClient

	sysOnce sync.Once
	sysErr  error
}

func newOpenBMCDriver(name string, h config.Host) *openbmcDriver {
	h.Name = name
	return &openbmcDriver{ipmiDriver: ipmiDriver{name, h}, rf: redfishFor(name, h)}
}

// system discovers the ComputerSystem path once: the first member of
// /redfish/v1/Systems.
func (d *openbmcDriver) system(ctx context.Context) error {
	d.sysOnce.Do(func() {
		data, err := d.rf.do(ctx, http.MethodGet, redfishRoot+"/Systems", nil)
		if err != nil {
			d.sysErr = err
			return
		}
		var s struct {
			Members []struct {
				ID string `json:"@odata.id"`
			}
		}
		if err := json.Unmarshal(data, &s); err != nil {
			d.sysErr = fmt.Errorf("Redfish /redfish/v1/Systems: %w", err)
			return
		}
		if len(s.Members) == 0 || !strings.HasPrefix(s.Members[0].ID, redfishRoot+"/Systems/") {
			d.sysErr = errors.New("Redfish /redfish/v1/Systems lists no system")
			return
		}
		d.rf.system = s.Members[0].ID
	})
	return d.sysErr
}

func (d *openbmcDriver) ctx(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

func (d *openbmcDriver) Power(act string) error {
	if act == "status" {
		r := d.PowerStatus(redfishTimeout)
		if r.Failed {
			return errors.New(strings.TrimPrefix(r.Text, "ERROR: "))
		}
		fmt.Fprintln(Stdout(), r.Text)
		return nil
	}
	msg, err := d.reset(act)
	if err != nil {
		return err
	}
	fmt.Fprintln(Stdout(), msg)
	return nil
}

// reset sends the Redfish reset for a power action and returns the line to show.
func (d *openbmcDriver) reset(act string) (string, error) {
	rt, ok := redfishResetType(act)
	if !ok {
		return "", UnknownPowerError(act)
	}
	if err := checkCreds(d.name, d.h); err != nil {
		return "", err
	}
	ctx, cancel := d.ctx(2 * redfishTimeout)
	defer cancel()
	if err := d.system(ctx); err != nil {
		return "", err
	}
	if act == "cycle" {
		if allowed, err := d.rf.ResetTypes(ctx); err == nil && slices.Contains(allowed, "PowerCycle") {
			rt = "PowerCycle"
		}
	}
	if err := d.rf.Reset(ctx, rt); err != nil {
		return "", err
	}
	return fmt.Sprintf("Chassis Power Control: %s (Redfish %s)", act, rt), nil
}

func (d *openbmcDriver) PowerStatus(timeout time.Duration) StatusResult {
	if err := checkCreds(d.name, d.h); err != nil {
		return statusFail(err.Error())
	}
	ctx, cancel := d.ctx(timeout)
	defer cancel()
	if err := d.system(ctx); err != nil {
		return statusFail(err.Error())
	}
	st, err := d.rf.PowerState(ctx)
	if err != nil {
		return statusFail(err.Error())
	}
	return StatusResult{Text: "Chassis Power is " + strings.ToLower(st)}
}

func (d *openbmcDriver) Boot(dev string, legacy, persistent bool) error {
	target, ok := redfishBootTarget(dev)
	if !ok {
		return fmt.Errorf("unknown boot device %q", dev)
	}
	if persistent {
		return errors.New("--persistent is not supported over Redfish here; the override is one-time")
	}
	if err := checkCreds(d.name, d.h); err != nil {
		return err
	}
	ctx, cancel := d.ctx(2 * redfishTimeout)
	defer cancel()
	if err := d.system(ctx); err != nil {
		return err
	}
	if err := d.rf.BootOnce(ctx, target, legacy); err != nil {
		return err
	}
	fmt.Fprintf(Stdout(), "Next boot: %s (once, Redfish)\n", dev)
	return nil
}

// ConsolePower is the console's p key: a Redfish power cycle.
func (d *openbmcDriver) ConsolePower() func() (string, error) {
	return func() (string, error) { return d.reset("cycle") }
}

// wsLink is the console link over the websocket.
type wsLink struct {
	ws *wsConn
}

func (l *wsLink) Send(b []byte) error { _, err := l.ws.Write(b); return err }

// Break: bmcweb's console websocket carries bytes only; it has no serial BREAK.
func (l *wsLink) Break() error {
	return errors.New("the OpenBMC web console cannot send a serial BREAK")
}

func (l *wsLink) Disconnect() error {
	err := l.ws.CloseGracefully()
	time.AfterFunc(2*time.Second, func() { _ = l.ws.Close() })
	return err
}

func (l *wsLink) Resize(rows, cols int) error { return nil }

func (l *wsLink) FitSize(rows, cols int) error {
	line, _ := l.SizeLine(rows, cols)
	return l.Send([]byte(line + "\r"))
}

func (l *wsLink) SizeLine(rows, cols int) (string, bool) { return sttyLine(rows, cols), true }

// Serial: the websocket ends at the BMC's serial line, so pastes are paced.
func (l *wsLink) Serial() bool { return true }

func (l *wsLink) SizeIsFree() bool { return false }

// openWS opens the console websocket with Basic auth, on the same pinned TLS
// key as Redfish.
func (d *openbmcDriver) openWS() (*ConsoleTransport, error) {
	if err := checkCreds(d.name, d.h); err != nil {
		return nil, err
	}
	tr, ok := d.rf.hc.Transport.(*http.Transport)
	if !ok {
		return nil, errors.New("internal: no TLS transport")
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(d.h.User+":"+d.h.Pass)))
	ctx, cancel := d.ctx(redfishTimeout)
	defer cancel()
	ws, err := wsDial(ctx, net.JoinHostPort(d.h.IP, "443"), openbmcConsolePath, tr.TLSClientConfig.Clone(), hdr)
	if err != nil {
		return nil, fmt.Errorf("%s console: %w", d.name, err)
	}
	done := make(chan console.Result, 1)
	pr, pw := io.Pipe()
	go func() {
		_, err := io.Copy(pw, ws)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			pw.CloseWithError(err)
			done <- console.Result{Code: 1, Err: err}
			return
		}
		pw.Close()
		done <- console.Result{}
	}()
	return &ConsoleTransport{
		Kind:  "console",
		Link:  &wsLink{ws: ws},
		Out:   pr,
		Done:  done,
		Close: func() { _ = ws.Close(); _ = pr.Close() },
	}, nil
}

func (d *openbmcDriver) OpenConsole(o OpenConsoleOptions) (*ConsoleTransport, error) {
	return d.openWS()
}

func (d *openbmcDriver) Console(o SOLOptions) error {
	if o.SSH {
		return errNoSSHConsole
	}
	if err := rejectSOLFlags(o, "the OpenBMC console is a websocket and has no SOL session to clear"); err != nil {
		return err
	}
	if !console.UseBar() {
		return errors.New("the OpenBMC console needs a terminal")
	}
	if _, _, err := term.GetSize(int(os.Stdout.Fd())); err != nil {
		return err
	}
	tr, err := d.openWS()
	if err != nil {
		return err
	}
	defer tr.Close()
	return app.Code(console.Run(console.Setup{
		Host: d.name, Kind: tr.Kind,
		Link:       tr.Link,
		Out:        tr.Out,
		Done:       tr.Done,
		Power:      d.ConsolePower(),
		Connecting: "connecting to " + d.name + " via the OpenBMC web console...",
	}))
}
