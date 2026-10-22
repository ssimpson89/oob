package mcpserver

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ssimpson89/oob/internal/bmc"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
)

// Secrets that must never reach the model.
const (
	secretPass = "s3cr3t-pw-do-not-leak"
	secretKey  = "/keys/id_test_do_not_leak"
)

func testConfig() *config.Config {
	return &config.Config{
		Defaults: config.Host{User: "admin", Pass: secretPass},
		Hosts: map[string]config.Host{
			"node01": {IP: "192.0.2.11", Type: "ipmi", Tags: []string{"rack1", "gpu"}, Notes: "first node", Key: secretKey},
			"node02": {IP: "192.0.2.12", Type: "ilo", Tags: []string{"rack2"}, Pass: secretPass + "-2"},
			"login1": {IP: "192.0.2.13", Type: "idrac", Notes: "login node"},
		},
	}
}

// fakeDriver is a bmc.Driver whose output goes where a real driver's does.
// Methods it doesn't define panic, so a tool that reaches for one fails loudly.
type fakeDriver struct {
	bmc.Driver
	h    *harness
	name string
}

func (d *fakeDriver) record(s string) {
	d.h.mu.Lock()
	d.h.calls = append(d.h.calls, d.name+":"+s)
	d.h.mu.Unlock()
}

func (d *fakeDriver) Power(act string) error {
	d.record("power " + act)
	if act == "status" {
		fmt.Fprintln(bmc.Stdout(), "Chassis Power is on")
		return nil
	}
	fmt.Fprintln(bmc.Stdout(), "Chassis Power Control: "+act)
	return nil
}

func (d *fakeDriver) SEL(act string) error {
	d.record("sel " + act)
	if act == "list" {
		if d.h.selLines > 0 {
			for i := 0; i < d.h.selLines; i++ {
				fmt.Fprintf(bmc.Stdout(), "%d | 10/21/2026 | Temperature #0x01 | Upper Critical going high | line padding padding padding\n", i)
			}
			return nil
		}
		fmt.Fprintln(bmc.Stdout(), "1 | 10/21/2026 | Power Supply #0x02 | \x1b[31mPresence detected\x1b[0m\x1b]0;evil\x07")
	}
	return nil
}

func (d *fakeDriver) Boot(dev string, legacy, persistent bool) error {
	d.record(fmt.Sprintf("boot %s legacy=%v persistent=%v", dev, legacy, persistent))
	return nil
}
func (d *fakeDriver) UID(arg string) error      { d.record("uid " + arg); return nil }
func (d *fakeDriver) NMI() error                { d.record("nmi"); return nil }
func (d *fakeDriver) MCReset(kind string) error { d.record("mcreset " + kind); return nil }

func (d *fakeDriver) OpenConsole(o bmc.OpenConsoleOptions) (*bmc.ConsoleTransport, error) {
	d.record(fmt.Sprintf("open console force=%v", o.Force))
	if d.h.openErr != nil {
		return nil, d.h.openErr
	}
	var fh *fakeHost
	if d.h.initial != nil {
		fh = newFakeHostShowing(d.h.t, d.name, *d.h.initial)
	} else {
		fh = newFakeHost(d.h.t, d.name)
	}
	if d.h.setup != nil {
		d.h.setup(fh)
	}
	d.h.mu.Lock()
	d.h.hosts[d.name] = fh
	d.h.mu.Unlock()
	return fh.transport(), nil
}

// fakeHost is a server's console behind a fake link: it echoes what is typed
// and answers a few commands.
type fakeHost struct {
	t    *testing.T
	name string
	pw   *io.PipeWriter
	pr   *io.PipeReader
	outq chan []byte
	done chan console.Result

	mu       sync.Mutex
	sent     []byte
	lines    []string // lines entered, in order
	buf      string
	breaks   int
	breakErr error
	closed   bool
	ended    bool
	prompt   string
	asking   string // a question waiting for y or n
	toggle   bool   // behaves like iLO/iDRAC: has a BMC prompt
	atBMC    bool
	respond  func(f *fakeHost, cmd string) bool // true if it handled the command

	disconnects int           // calls of the link's Disconnect, which types an escape at the console
	released    bool          // transport Release ran
	closedFirst bool          // ... and the transport had been closed by then
	stuck       bool          // Send blocks, like an SSH channel that stopped taking input, until Close
	unstick     chan struct{} // closed by Close
	unstickOnce sync.Once
	blocked     chan struct{} // receives once each time a Send blocks because the host is stuck

	writes, maxWrite int           // Send calls and the longest one, to see whether a line is typed in one burst
	sendDelay        time.Duration // each Send takes this long, like a slow serial link
	vsp              bool          // maps input like an iLO VSP
	serial           bool          // a serial console: the session paces what it types, by pacing
	pacing           console.Pacing
}

func newFakeHost(t *testing.T, name string) *fakeHost {
	return newFakeHostShowing(t, name, "[root@"+name+" ~]# ")
}

// newFakeHostShowing is a host whose console starts by printing initial: a
// banner with no prompt after it, as a console opened on a quiet serial port
// looks.
func newFakeHostShowing(t *testing.T, name, initial string) *fakeHost {
	f := &fakeHost{t: t, name: name, outq: make(chan []byte, 4096), unstick: make(chan struct{}), blocked: make(chan struct{}, 16), done: make(chan console.Result, 1), prompt: "[root@" + name + " ~]# "}
	f.pr, f.pw = io.Pipe()
	go func() {
		for b := range f.outq {
			if _, err := f.pw.Write(b); err != nil {
				return
			}
		}
	}()
	f.emit(initial)
	return f
}

func (f *fakeHost) emit(s string) {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if !closed {
		select {
		case f.outq <- []byte(s):
		default:
		}
	}
}

func (f *fakeHost) Send(b []byte) error {
	f.mu.Lock()
	if f.stuck {
		f.mu.Unlock()
		select {
		case f.blocked <- struct{}{}:
		default:
		}
		<-f.unstick
		return io.ErrClosedPipe
	}
	if f.closed || f.ended {
		f.mu.Unlock()
		return io.ErrClosedPipe
	}
	delay := f.sendDelay
	f.writes++
	f.maxWrite = max(f.maxWrite, len(b))
	f.sent = append(f.sent, b...)
	f.mu.Unlock()
	time.Sleep(delay)
	for _, c := range b {
		switch c {
		case '\r':
			f.mu.Lock()
			line := f.buf
			f.buf = ""
			f.lines = append(f.lines, line)
			f.mu.Unlock()
			f.emit("\r\n")
			f.run(line)
		case 0x7f:
			f.mu.Lock()
			if f.buf != "" {
				f.buf = f.buf[:len(f.buf)-1]
			}
			f.mu.Unlock()
			f.emit("\b \b")
		case 0x15:
			f.mu.Lock()
			f.buf = ""
			f.mu.Unlock()
			f.emit("\r\x1b[K" + f.prompt) // a shell redraws the empty line
		case 0x03:
			f.mu.Lock()
			f.buf = ""
			f.asking = ""
			f.mu.Unlock()
			f.emit("^C\r\n" + f.prompt)
		default:
			if c >= 0x20 && c < 0x7f {
				f.mu.Lock()
				f.buf += string(rune(c))
				f.mu.Unlock()
				f.emit(string(rune(c)))
			}
		}
	}
	return nil
}

func (f *fakeHost) run(line string) {
	f.mu.Lock()
	asking, respond := f.asking, f.respond
	f.asking = ""
	f.mu.Unlock()
	if asking != "" {
		f.emit("Complete!\r\n" + f.prompt)
		return
	}
	if respond != nil && respond(f, line) {
		return
	}
	switch strings.TrimSpace(line) {
	case "uname -r":
		f.emit("5.14.0-test\r\n" + f.prompt)
	case "reboot", "sudo reboot", "systemctl reboot":
		f.emit("Rebooting.\r\n")
	case "dnf install foo":
		f.mu.Lock()
		f.asking = "y"
		f.mu.Unlock()
		f.emit("Is this ok [y/N]: ")
	case "flood":
		for i := 0; i < 3000; i++ {
			f.emit(fmt.Sprintf("log line %d padding padding padding padding\r\n", i))
		}
		f.emit(f.prompt)
	case "evil":
		f.emit("\x1b]0;evil title\x07\x1b[2J\x1b[31mred\x1b[0m ‮txet‬ ok\x9b31m\r\n" + f.prompt)
	case "grub":
		f.mu.Lock()
		f.prompt = "grub> "
		f.mu.Unlock()
		f.emit("grub> ")
	case "vsp":
		f.mu.Lock()
		f.atBMC = false
		f.mu.Unlock()
		f.emit("Virtual Serial Port Active: COM2\r\n" + f.prompt)
	default:
		f.mu.Lock()
		bmcMode := f.atBMC
		f.mu.Unlock()
		if bmcMode {
			f.emit("\r\n</>hpiLO-> ")
			return
		}
		f.emit("sh: " + line + ": ok\r\n" + f.prompt)
	}
}

func (f *fakeHost) Break() error {
	f.mu.Lock()
	err := f.breakErr
	if err == nil {
		f.breaks++
	}
	f.mu.Unlock()
	if err == nil {
		f.emit("\r\nSysRq : BREAK\r\n")
	}
	return err
}

func (f *fakeHost) Disconnect() error {
	f.mu.Lock()
	f.disconnects++
	f.mu.Unlock()
	f.emit("[SOL Session closed]\r\n")
	f.finish()
	return nil
}

func (f *fakeHost) Resize(rows, cols int) error  { return nil }
func (f *fakeHost) FitSize(rows, cols int) error { return nil }
func (f *fakeHost) MapInput(b []byte) ([]byte, string) {
	if !f.vsp {
		return b, ""
	}
	return console.MapVSPInput(b)
}
func (f *fakeHost) SizeIsFree() bool { return false }
func (f *fakeHost) Serial() bool     { f.mu.Lock(); defer f.mu.Unlock(); return f.serial }

// finish ends the transport the way a hung-up console does.
func (f *fakeHost) finish() {
	f.mu.Lock()
	if f.ended {
		f.mu.Unlock()
		return
	}
	f.ended = true
	f.mu.Unlock()
	go func() {
		time.Sleep(20 * time.Millisecond) // let queued output through
		f.pw.Close()
		f.done <- console.Result{}
	}()
}

func (f *fakeHost) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.pw.Close()
	f.pr.Close()
	f.finish() // a real transport ends when it is closed
	f.unstickOnce.Do(func() { close(f.unstick) })
}

func (f *fakeHost) setStuck() {
	f.mu.Lock()
	f.stuck = true
	f.mu.Unlock()
}

func (f *fakeHost) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeHost) entered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

func (f *fakeHost) rawSent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.sent)
}

func (f *fakeHost) transport() *bmc.ConsoleTransport {
	tr := &bmc.ConsoleTransport{Kind: "sol", Link: f, Out: f.pr, Done: f.done, Close: f.Close, Tail: func() string { return "" }, Pacing: f.pacing}
	tr.Release = func() {
		f.mu.Lock()
		f.released, f.closedFirst = true, f.closed
		f.mu.Unlock()
	}
	if f.toggle {
		tr.Kind = "vsp"
		tr.Toggle = func() (string, bool, error) {
			f.mu.Lock()
			if f.stuck {
				f.mu.Unlock()
				<-f.unstick
				return "", false, io.ErrClosedPipe
			}
			f.atBMC = !f.atBMC
			at := f.atBMC
			f.mu.Unlock()
			if at {
				f.emit("\r\n</>hpiLO-> ")
				return "ilo cli", true, nil
			}
			f.emit("Virtual Serial Port Active: COM2\r\n" + f.prompt)
			return "vsp", false, nil
		}
		tr.Observe = func(b []byte) (string, bool, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if strings.Contains(string(b), "hpiLO->") {
				f.atBMC = true
			} else if strings.Contains(string(b), "Virtual Serial Port Active") {
				f.atBMC = false
			}
			if f.atBMC {
				return "ilo cli", true, false
			}
			return "vsp", false, false
		}
	}
	return tr
}

// harness is a server with fake hosts, and an MCP client.
type harness struct {
	t       *testing.T
	srv     *server
	cs      *mcp.ClientSession
	cfg     *config.Config
	asked   []string // the approval questions the user saw
	setup   func(*fakeHost)
	openErr error
	initial *string // what a console prints when it opens, if not a prompt
	onAsk   func()  // runs while the user is being asked, before the answer

	mu       sync.Mutex
	calls    []string
	hosts    map[string]*fakeHost
	ipmi     []string
	selLines int
	statuses map[string]bmc.StatusResult
}

func (h *harness) called() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.calls, "|")
}

func (h *harness) host(name string) *fakeHost {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hosts[name]
}

// start connects a client to a server. With answer set, the client can ask the
// user to approve things, and the user answers: "accept" ticks "Allow it" and
// accepts, "unticked" accepts without ticking, anything else is the action,
// such as "decline".
// shareState points the share sockets at a fresh short folder: a unix socket
// path is limited to about 100 bytes, which t.TempDir often exceeds.
func shareState(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "oob")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", d)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func start(t *testing.T, opt Options, answer string) *harness {
	return startVersion(t, opt, answer, "")
}

func startVersion(t *testing.T, opt Options, answer, version string) *harness {
	t.Helper()
	shareState(t)
	oldQuiet := quietFor
	quietFor = 25 * time.Millisecond
	t.Cleanup(func() { quietFor = oldQuiet })
	h := &harness{t: t, cfg: testConfig(), hosts: map[string]*fakeHost{}, statuses: map[string]bmc.StatusResult{}}
	h.setup = nil
	opt.Backend = Backend{
		Config: func() (*config.Config, error) { return h.cfg, nil },
		Driver: func(name string, host config.Host) bmc.Driver {
			h.mu.Lock()
			h.calls = append(h.calls, "driver:"+name+" pass="+fmt.Sprint(host.Pass != ""))
			h.mu.Unlock()
			return &fakeDriver{h: h, name: name}
		},
		PowerStatus: func(name string, host config.Host, timeout time.Duration) bmc.StatusResult {
			h.mu.Lock()
			defer h.mu.Unlock()
			if r, ok := h.statuses[name]; ok {
				return r
			}
			return bmc.StatusResult{Text: "Chassis Power is on"}
		},
		IPMI: func(host config.Host, args ...string) (int, error) {
			h.mu.Lock()
			h.ipmi = append(h.ipmi, host.IP+" "+strings.Join(args, " "))
			h.mu.Unlock()
			fmt.Fprintln(bmc.Stdout(), "CPU Temp | 41.000 | degrees C | ok")
			return 0, nil
		},
	}
	s := newServer(opt)
	h.srv = s
	t.Cleanup(s.closeAll)

	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.mcp().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	copt := &mcp.ClientOptions{}
	if answer != "" {
		copt.ElicitationHandler = func(_ context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			h.mu.Lock()
			h.asked = append(h.asked, r.Params.Message)
			onAsk := h.onAsk
			h.mu.Unlock()
			if onAsk != nil {
				onAsk()
			}
			switch answer {
			case "accept":
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"allow": true}}, nil
			case "unticked":
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"allow": false}}, nil
			}
			return &mcp.ElicitResult{Action: answer}, nil
		}
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, copt).Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	h.cs = cs
	return h
}

func (h *harness) askedAll() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.asked, "\n--\n")
}

// result runs a tool and returns the whole result.
func (h *harness) result(name string, args map[string]any) *mcp.CallToolResult {
	h.t.Helper()
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	return res
}

// call runs a tool and returns its text and whether it was an error.
func (h *harness) call(name string, args map[string]any) (string, bool) {
	h.t.Helper()
	res := h.result(name, args)
	var b strings.Builder
	for _, c := range res.Content {
		b.WriteString(c.(*mcp.TextContent).Text)
	}
	return b.String(), res.IsError
}

// connect opens a console on node01 and returns its session id.
func (h *harness) connect() string {
	h.t.Helper()
	out, isErr := h.call("connect", map[string]any{"host": "node01"})
	if isErr || !strings.Contains(out, "session s") {
		h.t.Fatalf("connect: %s", out)
	}
	i := strings.Index(out, "session s")
	id := out[i+len("session "):]
	return strings.Fields(id)[0]
}
