package mcpserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ssimpson89/oob/internal/bmc"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/share"
)

const (
	commandWait    = 10 * time.Second // send_command: default wait per line for the prompt
	commandWaitMax = 60 * time.Second
	keysWait       = 2 * time.Second
	keysWaitMax    = 30 * time.Second
	readWaitMax    = 60 * time.Second
	historyShown   = 50
	commandLines   = 20   // most lines in one send_command
	commandBytes   = 4096 // most bytes in one send_command
	textBytes      = 1024 // most bytes of text in one send_keys
	keysMax        = 32
)

// Time limits; variables so tests can shorten them.
var (
	connectTimeout = 60 * time.Second // opening a console (SSH login, ipmitool session)
	connectWait    = 3 * time.Second  // connect: how long to wait for the console to say something
)

// keyBytes are the keys send_keys can press. ctrl+\ and ctrl+^ are left out:
// they are the escape characters of the iDRAC console and of IPMI SOL, and
// would end the session.
var keyBytes = map[string]string{
	"enter": "\r", "tab": "\t", "space": " ", "backspace": "\x7f", "delete": "\x1b[3~", "esc": "\x1b",
	"up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
	"home": "\x1b[H", "end": "\x1b[F", "pageup": "\x1b[5~", "pagedown": "\x1b[6~", "insert": "\x1b[2~",
	"f1": "\x1bOP", "f2": "\x1bOQ", "f3": "\x1bOR", "f4": "\x1bOS", "f5": "\x1b[15~", "f6": "\x1b[17~",
	"f7": "\x1b[18~", "f8": "\x1b[19~", "f9": "\x1b[20~", "f10": "\x1b[21~", "f11": "\x1b[23~", "f12": "\x1b[24~",
}

// aliasKeys are control keys that do what another listed key does, or submit
// the line like enter: ctrl+m and ctrl+j are a carriage return and a line feed,
// ctrl+i is tab, and Readline runs the line on ctrl+o as well. They would
// bypass the checks made on enter and tab, so they aren't offered.
var aliasKeys = map[string]bool{"ctrl+m": true, "ctrl+j": true, "ctrl+o": true, "ctrl+i": true}

func init() {
	for c := 'a'; c <= 'z'; c++ {
		if k := "ctrl+" + string(c); !aliasKeys[k] {
			keyBytes[k] = string(rune(c - 'a' + 1))
		}
	}
}

// firmwareKeys commonly boot, save or reset in a bootloader or firmware menu.
var firmwareKeys = []string{"ctrl+x", "f9", "f10"}

type connectIn struct {
	Host  string `json:"host" jsonschema:"the exact host name from list_hosts"`
	Force bool   `json:"force,omitempty" jsonschema:"IPMI SOL only: end a stale SOL session on the host first. Needs the user's approval"`
}

type sessionIn struct {
	Session string `json:"session" jsonschema:"the session id connect returned"`
}

type readIn struct {
	Session     string  `json:"session" jsonschema:"the session id connect returned"`
	WaitSeconds float64 `json:"wait_seconds,omitempty" jsonschema:"how long to wait for output, at most 60. Returns once the console goes quiet, or with until, as soon as that appears. Default 0: return at once"`
	Until       string  `json:"until,omitempty" jsonschema:"keep waiting until this text appears. Lowercase matches any case"`
}

type historyIn struct {
	Session string `json:"session" jsonschema:"the session id connect returned"`
	Lines   int    `json:"lines,omitempty" jsonschema:"how many lines to return, newest last. Default 50, at most 300"`
	Search  string `json:"search,omitempty" jsonschema:"only lines containing this text. Lowercase matches any case"`
}

type commandIn struct {
	Session     string  `json:"session" jsonschema:"the session id connect returned"`
	Line        string  `json:"line" jsonschema:"the command to run. Several lines run one after another, each once the prompt is back"`
	WaitSeconds float64 `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the prompt after each line, at most 60. Default 10"`
}

type keysIn struct {
	Session     string   `json:"session" jsonschema:"the session id connect returned"`
	Text        string   `json:"text,omitempty" jsonschema:"characters to type, without Enter: an answer such as y, or the start of a command"`
	Keys        []string `json:"keys,omitempty" jsonschema:"keys to press after the text, in order"`
	WaitSeconds float64  `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the console to answer, at most 30. Default 2"`
}

type bmcPromptIn struct {
	Session string `json:"session" jsonschema:"the session id connect returned"`
	To      string `json:"to" jsonschema:"bmc for the BMC's own command line, console for the server's console"`
}

func (s *server) addConsoleTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "connect",
		Description: "Open the server's serial console (its login prompt, GRUB, kernel messages; not the BMC) through the " +
			"BMC: IPMI SOL, iLO VSP or iDRAC console com2. Returns a session id for the other console tools, and what the " +
			"console has printed so far. Connecting to a host that already has a session returns that session. Exact host name only.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, OpenWorldHint: &yes},
	}, s.connect)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "read_output",
		Description: "Return what the console printed since output was last returned, ending with the line the cursor is " +
			"on (usually the prompt). Can wait for output, or until some text appears, such as a login prompt after a reboot.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &yes},
	}, s.readOutput)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "read_history",
		Description: fmt.Sprintf("Return earlier console output: the last lines, or the last lines containing some text. "+
			"Keeps the last %d lines.", console.HistoryLines),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &no},
	}, s.readHistory)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "send_command",
		Description: "Run a command at the console, or several on separate lines, and return the output. Waits for the " +
			"prompt after each line, and stops when the console asks a question such as [y/N] or --More--; answer with " +
			"send_keys. Commands that reboot, power off, wipe, format or remove software need the user's approval.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes, OpenWorldHint: &yes},
	}, s.sendCommand)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "send_keys",
		Description: "Type text and/or press keys, then return what the console prints. For answering a question (text y, " +
			"keys enter), paging (space, or text q), stopping something (ctrl+c), arrow keys and function keys in GRUB or " +
			"BIOS menus. Use send_command to run commands. The same approval rules apply as for send_command.",
		InputSchema: schemaFor[keysIn](map[string][]string{"keys[]": slices.Sorted(maps.Keys(keyBytes))}),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes, OpenWorldHint: &yes},
	}, s.sendKeys)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "send_break",
		Description: "Send a serial BREAK (IPMI SOL consoles only; iLO and iDRAC have none). On Linux the next key becomes a " +
			"SysRq command. Needs the user's approval, and is refused while the prompt has text typed after it (the BREAK starts with a carriage return).",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes, OpenWorldHint: &yes},
	}, s.sendBreak)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "bmc_prompt",
		Description: "iLO and iDRAC only: switch the session between the server's console and the BMC's own command line. " +
			"Everything typed at the BMC command line needs the user's approval.",
		InputSchema: schemaFor[bmcPromptIn](map[string][]string{"to": {"bmc", "console"}}),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, OpenWorldHint: &yes},
	}, s.bmcPrompt)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "disconnect",
		Description: "Close a console session and save its log, if logging is on.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true, OpenWorldHint: &no},
	}, s.disconnectTool)
}

// session finds an open session and marks it in use.
func (s *server) session(id string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.sessions[id]; ok {
		c.touch()
		return c, nil
	}
	if why, ok := s.gone[id]; ok {
		return nil, fmt.Errorf("session %s is closed (%s); call connect again", id, why)
	}
	if len(s.sessions) == 0 {
		return nil, fmt.Errorf("no session %q: call connect first", id)
	}
	var open []string
	for _, c := range s.sessions {
		open = append(open, c.id+" ("+c.host+")")
	}
	slices.Sort(open)
	return nil, fmt.Errorf("no session %q; open: %s", id, strings.Join(open, ", "))
}

// forget removes a session from the table, remembering why.
func (s *server) forget(c *session, why string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[c.id] == c {
		delete(s.sessions, c.id)
	}
	if len(s.gone) >= 32 {
		clear(s.gone)
	}
	s.gone[c.id] = why
}

// reap closes sessions that no tool has touched for IdleTimeout.
func (s *server) reap() {
	defer s.reaperWG.Done()
	iv := min(max(s.opt.IdleTimeout/4, 10*time.Millisecond), 30*time.Second)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.closeIdle()
		}
	}
}

func (s *server) closeIdle() {
	s.mu.Lock()
	var idle []*session
	for _, c := range s.sessions {
		// A session in the middle of a send isn't idle.
		if c.idleFor() > s.opt.IdleTimeout && c.sendMu.TryLock() {
			idle = append(idle, c)
		}
	}
	s.mu.Unlock()
	for _, c := range idle {
		s.forget(c, "closed after being idle for "+s.opt.IdleTimeout.String())
		go func() {
			c.close()
			// A tool that found the session before it was forgotten is waiting
			// for the send lock. Let it go on: it sees a closed session.
			c.sendMu.Unlock()
		}()
	}
}

// stuck forgets a session whose transport was closed because a write didn't
// complete in time.
func (s *server) stuck(c *session, err error) {
	if errors.Is(err, errStuck) {
		s.forget(c, "closed because the console stopped accepting input")
		go c.close()
	}
}

// seconds turns an optional wait in seconds into a duration.
func seconds(v float64, def, most time.Duration) (time.Duration, error) {
	switch {
	case v < 0:
		return 0, errors.New("wait_seconds can't be negative")
	case v == 0:
		return def, nil
	case v > most.Seconds():
		return 0, fmt.Errorf("wait_seconds can be at most %d", int(most.Seconds()))
	}
	return time.Duration(v * float64(time.Second)), nil
}

// pattern matches q literally, with smart case: all lowercase matches any
// case ("error" finds "ERROR"); any capital matches exactly.
func pattern(q string) *regexp.Regexp {
	expr := regexp.QuoteMeta(q)
	if q == strings.ToLower(q) {
		expr = "(?i)" + expr
	}
	return regexp.MustCompile(expr)
}

func (s *server) connect(ctx context.Context, req *mcp.CallToolRequest, in connectIn) (*mcp.CallToolResult, any, error) {
	name, h, err := s.hostFor(in.Host, true)
	if err != nil {
		return fail(err)
	}
	s.mu.Lock()
	for _, c := range s.sessions {
		if c.host == name {
			s.mu.Unlock()
			c.touch()
			return text(false, fmt.Sprintf("Already connected to %s as session %s.", name, c.id), c.output()), nil, nil
		}
	}
	s.mu.Unlock()

	if in.Force {
		if res, err := s.approve(req, target{name: name, addr: h.IP, typ: h.Type}, "end any other SOL session on "+name+" and take it over",
			[]string{whyForce}); res != nil || err != nil {
			if err != nil {
				return fail(err)
			}
			return res, nil, nil
		}
	}

	s.mu.Lock()
	switch {
	case s.pending[name]:
		s.mu.Unlock()
		return fail("a connection to " + name + " is already being opened")
	case len(s.sessions)+len(s.pending) >= s.opt.MaxSessions:
		s.mu.Unlock()
		return fail(fmt.Sprintf("%d sessions are open, the most allowed; disconnect one first", len(s.sessions)))
	}
	s.pending[name] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, name)
		s.mu.Unlock()
	}()

	type opened struct {
		cl  *share.Client
		err error
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	ch := make(chan opened, 1)
	who := clientName(req)
	go func() {
		// Join the console if the user already has it open in an oob window (an
		// iLO allows one VSP session, so a second connection couldn't work);
		// otherwise open it here, and a window opened later joins this one.
		addr, typ := share.Endpoint(h)
		cl, err := share.Open(ctx, share.Options{Host: name, Addr: addr, Type: typ, Agent: true, Name: who,
			Open: func() (*bmc.ConsoleTransport, error) {
				return s.be.Driver(name, h).OpenConsole(bmc.OpenConsoleOptions{Rows: ptyRows, Cols: ptyCols, Force: in.Force})
			}})
		ch <- opened{cl, err}
	}()
	var cl *share.Client
	select {
	case o := <-ch:
		if o.err != nil {
			return fail("can't open the console on "+name+": ", shareError(o.err))
		}
		cl = o.cl
	case <-ctx.Done():
		go func() { // it may still finish; don't leave it open
			if o := <-ch; o.cl != nil {
				o.cl.Close()
			}
		}()
		return fail(errTimeout("opening the console on "+name, connectTimeout, false))
	}
	tr := cl.Transport()

	tx := console.NewTranscript(name, tr.Kind)
	logNote := "Not logging."
	if s.opt.Log {
		if path, err := tx.StartLog(s.opt.LogDir); err != nil {
			logNote = "Not logging: " + err.Error()
		} else {
			logNote = "Logging to " + filepath.Base(path) + "."
		}
	}
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("s%d", s.nextID)
	c := newSession(id, name, tr, tx, cl)
	c.addr, c.typ = h.IP, h.Type // what was connected to, whatever the host list says later
	s.sessions[id] = c
	s.mu.Unlock()

	c.settle(ctx, c.changes(), connectWait, c.unreadWaiting())
	if err := ctx.Err(); err != nil {
		s.forget(c, "the connect call was cancelled")
		c.close()
		return fail(err)
	}
	// It ended while we waited: the login or the session failed. The output
	// hanging up counts even if the exit status is not in yet, so a console
	// that prints an error and exits is caught however the quiet timer ran.
	ended := false
	select {
	case <-c.doneC:
		ended = true
	case <-c.eofC:
		ended = true
		select { // the exit status is worth a moment, as in drain
		case <-c.doneC:
		case <-time.After(connectEndGrace):
		}
	default:
	}
	if ended {
		why := c.output()
		if tr.Tail != nil && !cl.Joined() {
			why = lastLines(tr.Tail(), 6)
		}
		s.forget(c, "the console ended right after connecting")
		c.close()
		return fail("the console on "+name+" ended right after connecting:", why)
	}
	out := c.output()
	var quiet string
	if strings.HasPrefix(out, "(no new output)") {
		quiet = "The console has printed nothing yet. send_keys with keys [\"enter\"] usually brings up the prompt."
	}
	head := fmt.Sprintf("Connected to %s as session %s (%s console).", name, id, tr.Kind)
	if cl.Joined() {
		head = fmt.Sprintf("Joined %s as session %s (%s console), which the user already has open in an oob window. "+
			"They see everything you type and can pause you. You see the line the cursor is on, not what came before.", name, id, tr.Kind)
	}
	return text(false, head+" "+logNote, out, quiet), nil, nil
}

// connectEndGrace is how long connect waits for the exit status once the
// console's output has hung up. A variable so tests can shorten it.
var connectEndGrace = 2 * time.Second

// lastLines is the last n non-empty lines of s.
func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(console.CleanRemote(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (s *server) readOutput(ctx context.Context, _ *mcp.CallToolRequest, in readIn) (*mcp.CallToolResult, any, error) {
	c, err := s.session(in.Session)
	if err != nil {
		return fail(err)
	}
	defer c.touch()
	wait, err := seconds(in.WaitSeconds, 0, readWaitMax)
	if err != nil {
		return fail(err)
	}
	var note string
	if in.Until != "" {
		if !c.waitFor(ctx, pattern(in.Until), wait) {
			note = fmt.Sprintf("%q didn't appear within %s.", in.Until, wait)
		}
	} else if wait > 0 {
		c.settle(ctx, c.changes(), wait, c.unreadWaiting())
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return text(false, c.output(), note), nil, nil
}

func (s *server) readHistory(_ context.Context, _ *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, any, error) {
	c, err := s.session(in.Session)
	if err != nil {
		return fail(err)
	}
	defer c.touch()
	n := cmp.Or(in.Lines, historyShown)
	if n < 1 || n > shownLinesMax {
		return fail(fmt.Sprintf("lines must be between 1 and %d", shownLinesMax))
	}
	var p *regexp.Regexp
	if in.Search != "" {
		p = pattern(in.Search)
	}
	return text(false, c.history(p, n)), nil, nil
}

// splitLines turns a command into the lines to type: tabs become spaces (Tab
// is "complete this word"), and anything else that is a control character is
// refused because it would act, not type.
func splitLines(command string) ([]string, error) {
	command = strings.TrimRight(strings.NewReplacer("\r\n", "\n", "\r", "\n", "\t", " ").Replace(command), "\n")
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("no command given; to press Enter, use send_keys with keys [\"enter\"]")
	}
	if len(command) > commandBytes {
		return nil, fmt.Errorf("the command is %d bytes, more than the %d allowed in one call", len(command), commandBytes)
	}
	lines := strings.Split(command, "\n")
	if len(lines) > commandLines {
		return nil, fmt.Errorf("%d lines is more than the %d allowed in one call", len(lines), commandLines)
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			return nil, fmt.Errorf("line %d is empty; to press Enter, use send_keys", i+1)
		}
		if strings.ContainsFunc(l, func(r rune) bool { return unicode.IsControl(r) }) {
			return nil, fmt.Errorf("line %d has a control character; press keys with send_keys", i+1)
		}
	}
	return lines, nil
}

// marked follows a line that needs approval, when the approval shows several.
const marked = "   <- this one"

// approvalText is what the user is asked to approve: the whole command, or for
// a long one, the lines that need it.
func approvalText(lines []string) string {
	switch {
	case len(lines) == 1:
		return strings.TrimSuffix(lines[0], marked)
	case len(lines) <= 40:
		return strings.Join(lines, "\n")
	}
	risky := []string{fmt.Sprintf("%d lines, of which these need approval:", len(lines))}
	for i, l := range lines {
		if strings.HasSuffix(l, marked) {
			risky = append(risky, fmt.Sprintf("line %d: %s", i+1, strings.TrimSuffix(l, marked)))
		}
	}
	return strings.Join(risky, "\n")
}

// safeAnswerKeys reports whether typing text and then keys at a yes-or-no
// question can only decline or stop it.
func safeAnswerKeys(text string, keys []string) bool {
	if strings.EqualFold(text, "n") || strings.EqualFold(text, "no") {
		return !slices.ContainsFunc(keys, func(k string) bool { return k != "enter" })
	}
	if text != "" || len(keys) == 0 {
		return false
	}
	return !slices.ContainsFunc(keys, func(k string) bool { return !safeAnswer("", k) })
}

// staged is what is typed at the prompt and not yet run, or "" if the line is
// empty: the text after the prompt, text sent that the echo hasn't shown yet,
// or a note that the line was edited by keys the server can't follow.
func (c *session) staged() string {
	at := c.line()
	c.mu.Lock()
	defer c.mu.Unlock()
	typed := at.typed()
	if pending, unknown := c.typing.pending(at.prompts); len(pending) > len(typed) {
		typed = pending
	} else if unknown && typed == "" {
		typed = "a line edited or recalled with cursor, editing or history keys"
	}
	return typed
}

// gate is what an approval depends on: where the console is (a bootloader, the
// BMC's prompt, just after a BREAK), what is already typed on the line, and
// whether it is a question waiting for an answer. A call records it before it
// asks the user and checks it again once it holds the keyboard.
type gate struct {
	ctx     string // contextWhy
	typed   string
	asking  bool
	prompts int // which question, when asking
}

// gateAt is the gate for the cursor line at.
func (c *session) gateAt(at line) gate {
	g := gate{ctx: c.contextWhy(at), asking: at.confirming()}
	if g.asking {
		g.prompts = at.prompts
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g.typed = at.typed()
	if pending, unknown := c.typing.pending(at.prompts); len(pending) > len(g.typed) {
		g.typed = pending
	} else if unknown && g.typed == "" {
		g.typed = "a line edited or recalled with cursor, editing or history keys"
	}
	return g
}

// changedWhileWaiting is nil if the console is as the call found it. Taking
// the keyboard on a shared console can wait for the user, and an approval
// waits for them too, so the console may have moved on: what was approved was
// for somewhere else. Then nothing is sent, and the approval is forgotten so
// the next call asks again.
func (c *session) changedWhileWaiting(before gate) *mcp.CallToolResult {
	now := c.gateAt(c.line())
	if now == before {
		return nil
	}
	c.mu.Lock()
	c.answersAt = -1
	c.mu.Unlock()
	res, _, _ := fail("nothing was sent: the console changed while this call waited for the user's approval or for the keyboard " +
		"(it was " + describeGate(before) + ", and is now " + describeGate(now) + "). Look at it with read_output and call again, " +
		"which asks the user afresh.")
	return res
}

func describeGate(g gate) string {
	switch {
	case g.asking:
		return "at a question"
	case g.ctx != "":
		return "a place where it " + g.ctx
	case g.typed != "":
		return "at a prompt with " + strconv.Quote(g.typed) + " typed"
	}
	return "at an empty prompt"
}

// sinceBreak reports whether a BREAK was sent recently enough that the next
// key may be a SysRq command.
func (c *session) sinceBreak() bool {
	c.mu.Lock()
	at := c.breakAt
	c.mu.Unlock()
	if c.sh != nil { // a BREAK sent from a window counts the same
		if t := c.sh.BreakAt(); t.After(at) {
			at = t
		}
	}
	return !at.IsZero() && time.Since(at) < afterBreak
}

func (c *session) onBMC() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.atBMC
}

func (s *server) sendCommand(ctx context.Context, req *mcp.CallToolRequest, in commandIn) (*mcp.CallToolResult, any, error) {
	c, err := s.session(in.Session)
	if err != nil {
		return fail(err)
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	defer c.touch()
	if err := c.canSend(); err != nil {
		return fail(err)
	}
	lines, err := splitLines(in.Line)
	if err != nil {
		return fail(err)
	}
	wait, err := seconds(in.WaitSeconds, commandWait, commandWaitMax)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(len(lines))*wait+toolSlack+c.typingTime(len(in.Line), len(lines)))
	defer cancel()

	at := c.line()
	c.mu.Lock()
	answering := at.confirming() && c.answersAt != at.prompts
	c.mu.Unlock()
	before := c.gateAt(at)
	typed := before.typed
	if typed != "" && !answering {
		return fail(fmt.Sprintf("the prompt already has %q typed after it, so nothing was sent. Clear it with send_keys "+
			"keys [\"ctrl+c\"], or finish it with send_keys", typed))
	}

	var shownLines, whys []string
	ctx0 := c.contextWhy(at)
	for i, l := range lines {
		why := risk(l)
		switch {
		case why != "":
		case ctx0 != "":
			why = ctx0
		case i == 0 && answering && !safeAnswer(l, "enter"):
			why = whyAnswer
		}
		if why != "" {
			whys = append(whys, why)
			l += marked
		}
		shownLines = append(shownLines, l)
	}
	if len(whys) > 0 && ctx0 != "" && !slices.Contains(whys, ctx0) {
		whys = append(whys, ctx0) // where it is typed is part of what is approved
	}
	if len(whys) > 0 {
		if res, err := s.approve(req, c.target(), "type at the console of "+c.host+":\n"+approvalText(shownLines), whys); res != nil || err != nil {
			if err != nil {
				return fail(err)
			}
			return res, nil, nil
		}
		c.mu.Lock()
		c.answersAt = at.prompts
		c.mu.Unlock()
	}

	ctx, why, release, err := c.keyboard(ctx)
	if err != nil {
		return fail(err)
	}
	defer release()
	if res := c.changedWhileWaiting(before); res != nil {
		return res, nil, nil
	}
	var lastErr error // a failed write says more than that the link dropped
	stopped := func() (*mcp.CallToolResult, any, error, bool) {
		if w := why(); w != "" && !(w == "disconnected" && lastErr != nil) {
			return text(true, c.output(), stoppedText(w)), nil, nil, true
		}
		return nil, nil, nil, false
	}
	for i, l := range lines {
		// The approval covered the context the batch started in. Each line
		// after the first is typed where the earlier ones left the console: if
		// that is a bootloader, the BMC or just after a BREAK, and the batch
		// wasn't approved for it, stop here and let the next call ask.
		if i > 0 {
			if now := c.contextWhy(c.line()); now != "" && now != ctx0 {
				return text(false, c.output(), "Stopped before line "+strconv.Itoa(i+1)+": the console is now somewhere that needs the "+
					"user's approval for what is typed (it "+now+"). Lines sent: "+strconv.Itoa(i)+". Lines not sent, which "+
					"nothing will replay:\n"+strings.Join(lines[i:], "\n")+"\nSend them with a new send_command, which will ask the user."), nil, nil
			}
		}
		c.tx.Mark("agent typed: " + l)
		err := c.runLine(ctx, l, wait)
		lastErr = err
		if !errors.Is(err, errStuck) {
			if res, out, e, ok := stopped(); ok {
				return res, out, e
			}
		}
		if err != nil {
			s.stuck(c, err)
			return fail(c.output(), "Sending stopped: "+err.Error())
		}
		if err := ctx.Err(); err != nil {
			return fail(err) // what arrived stays unread for the next call
		}
		if left := len(lines) - i - 1; left > 0 {
			if advice := c.stopAdvice(true); advice != "" {
				return text(false, c.output(), advice, fmt.Sprintf("The %d line(s) after it weren't sent.", left)), nil, nil
			}
		}
	}
	return text(false, c.output(), c.stopAdvice(false)), nil, nil
}

// runLine types a line and waits for the console to answer: until it is quiet
// at a prompt or a question, or wait passes.
func (c *session) runLine(ctx context.Context, l string, wait time.Duration) error {
	changed := c.changes()
	if err := c.write(ctx, []byte(l+"\r")); err != nil {
		return err
	}
	deadline := time.Now().Add(wait) // typing a line paced takes a while; wait_seconds is for the answer
	for {
		c.settle(ctx, changed, time.Until(deadline), false)
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-c.doneC:
			return nil
		default:
		}
		cur := c.line().cursor
		if looksLikePrompt(cur) || confirming.MatchString(cur) || paging.MatchString(cur) || loginPrompt.MatchString(cur) {
			return nil
		}
		changed = c.changes() // still running: wait for more
	}
}

// contextWhy says why anything typed at the console as it is now needs the
// user's approval, from where the cursor is: a bootloader, the BMC's own
// command line, or right after a BREAK. "" if nowhere special.
func (c *session) contextWhy(at line) string {
	switch {
	case at.atBootloader():
		return whyBoot
	case c.onBMC():
		return whyBMC
	case c.sinceBreak():
		return whyAfter
	}
	return ""
}

// stopAdvice explains where the console stopped, if it is waiting for the
// model: a question, a pager, a login, or a command still running. Lines of a
// batch still to send (mid) must never go in at a login or password prompt;
// the final result carries the login note already (see notes).
func (c *session) stopAdvice(mid bool) string {
	cur := c.line().cursor
	switch {
	case confirming.MatchString(cur):
		return "The console is asking a question. Answer with send_keys: text y and keys [\"enter\"] to go ahead, text n to cancel."
	case paging.MatchString(cur):
		return "The console is paging: send_keys keys [\"space\"] shows the next page, text q stops. Paging off, or head and grep, avoids this."
	case loginPrompt.MatchString(cur):
		if mid {
			return "The console is at a login or password prompt, so the lines after it would be typed in as a name or a password. " +
				"Ask the user to log in or to give you credentials; don't guess."
		}
		return "" // notes() says to ask the user
	case !looksLikePrompt(cur):
		return "No prompt yet: the command may still be running. read_output can wait for it; send_keys ctrl+c stops it."
	}
	return ""
}

func (s *server) sendKeys(ctx context.Context, req *mcp.CallToolRequest, in keysIn) (*mcp.CallToolResult, any, error) {
	c, err := s.session(in.Session)
	if err != nil {
		return fail(err)
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	defer c.touch()
	if err := c.canSend(); err != nil {
		return fail(err)
	}
	switch {
	case strings.ContainsFunc(in.Text, unicode.IsControl):
		return fail("text can't contain Enter, Tab or other control characters; press those with keys, or run whole commands with send_command")
	case len(in.Text) > textBytes:
		return fail(fmt.Sprintf("text is %d bytes, more than the %d allowed in one call", len(in.Text), textBytes))
	case in.Text == "" && len(in.Keys) == 0:
		return fail("give text to type, keys to press, or both")
	case len(in.Keys) > keysMax:
		return fail(fmt.Sprintf("%d keys is more than the %d allowed in one call", len(in.Keys), keysMax))
	}
	for _, k := range in.Keys {
		if _, ok := keyBytes[k]; !ok {
			return fail(fmt.Sprintf("unknown key %q", k))
		}
	}
	wait, err := seconds(in.WaitSeconds, keysWait, keysWaitMax)
	if err != nil {
		return fail(err)
	}

	// Steps are the text, then each key. Work out what the line will be as the
	// keys land, so a line split across calls or keys is still checked.
	type step struct{ text, key string }
	steps := []step{{text: in.Text}}
	for _, k := range in.Keys {
		steps = append(steps, step{key: k})
	}
	at := c.line()
	before := c.gateAt(at)
	c.mu.Lock()
	sim := c.typing
	answersAt, lastEsc := c.answersAt, c.lastEsc
	c.mu.Unlock()
	var whys []string
	add := func(w string) {
		if w != "" {
			whys = append(whys, w)
		}
	}
	if at.confirming() && answersAt != at.prompts && !safeAnswerKeys(in.Text, in.Keys) {
		add(whyAnswer)
	}
	cur := at.cursor
	var wire strings.Builder
	var units [][]byte // the text and each key on their own, for the console to map one by one
	for i, st := range steps {
		if st.text == "" && st.key == "" {
			continue
		}
		cur += st.text
		sim.add(st.text, "", at.prompts)
		if slices.Contains(firmwareKeys, st.key) {
			add(whyFirmKey)
		}
		if st.key == "enter" {
			pending, unknown := sim.pending(at.prompts)
			add(cmp.Or(lineRisk(cur), risk(pending)))
			switch {
			case at.atBootloader() && (line{cursor: cur, prompt: at.prompt}.typed() != "" || pending != ""):
				add(whyBoot)
			case unknown:
				add(whyRecall)
			}
		}
		if st.key != "" {
			sim.add("", st.key, at.prompts)
			if st.key == "enter" {
				cur = ""
			}
		}
		// ESC then ( leaves an iLO console for the BMC's command line.
		if c.tr.Toggle != nil && strings.HasPrefix(st.text, "(") && (lastEsc || (i > 0 && steps[i-1].key == "esc")) {
			add(whyEscape)
		}
		lastEsc = st.key == "esc"
		wire.WriteString(st.text + keyBytes[st.key])
		units = append(units, []byte(st.text), []byte(keyBytes[st.key]))
	}
	if c.sh == nil {
		if _, err := c.mapUnits(units); err != nil {
			return fail(err) // before asking the user to approve what can't be typed
		}
	}
	if c.onBMC() && (in.Text != "" || slices.Contains(in.Keys, "enter")) {
		add(whyBMC)
	}
	if c.sinceBreak() {
		add(whyAfter)
	}
	if len(whys) > 0 {
		what := "type at the console of " + c.host + ": " + strings.TrimSpace(at.cursor+in.Text)
		if len(in.Keys) > 0 {
			what += " (then " + strings.Join(in.Keys, ", ") + ")"
		}
		if res, err := s.approve(req, c.target(), what, whys); res != nil || err != nil {
			if err != nil {
				return fail(err)
			}
			return res, nil, nil
		}
		c.mu.Lock()
		c.answersAt = at.prompts
		c.mu.Unlock()
	}

	ctx, why, release, err := c.keyboard(ctx)
	if err != nil {
		return fail(err)
	}
	defer release()
	if res := c.changedWhileWaiting(before); res != nil {
		return res, nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, wait+toolSlack+c.typingBudget([]byte(wire.String())))
	defer cancel()
	if len(in.Keys) > 0 {
		c.tx.Mark("agent pressed " + strings.Join(in.Keys, ", "))
	}
	changed := c.changes()
	if err := c.writeUnits(ctx, units); err != nil {
		if w := why(); w != "" && w != "disconnected" && !errors.Is(err, errStuck) {
			return text(true, c.output(), stoppedText(w)), nil, nil
		}
		s.stuck(c, err)
		return fail(err)
	}
	c.mu.Lock()
	c.typing = sim
	c.mu.Unlock()
	c.settle(ctx, changed, wait, false)
	if w := why(); w != "" {
		return text(true, c.output(), stoppedText(w)), nil, nil
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return text(false, c.output()), nil, nil
}

func (s *server) sendBreak(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, any, error) {
	c, err := s.session(in.Session)
	if err != nil {
		return fail(err)
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	defer c.touch()
	if err := c.canSend(); err != nil {
		return fail(err)
	}
	if c.tr.Toggle != nil { // an SSH console through the BMC: iLO VSP, iDRAC
		return fail("nothing was sent: this console (" + c.tr.Kind + ", through the BMC's SSH command line) has no serial BREAK. " +
			"Only IPMI SOL consoles do")
	}
	if typed := c.staged(); typed != "" {
		return fail(fmt.Sprintf("nothing was sent: the prompt has %q typed after it, and the BREAK is sent after a carriage return "+
			"that would run it. Clear the line with send_keys keys [\"ctrl+c\"], then call send_break again", typed))
	}
	if res, err := s.approve(req, c.target(), "send a serial BREAK to the console of "+c.host, []string{whyBreak}); res != nil || err != nil {
		if err != nil {
			return fail(err)
		}
		return res, nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, keysWait+toolSlack)
	defer cancel()
	changed := c.changes()
	if err := c.guarded(ctx, c.tr.Link.Break); err != nil {
		s.stuck(c, err)
		return fail("the BREAK wasn't sent: ", shareError(err))
	}
	c.mu.Lock()
	c.breakAt = time.Now()
	c.mu.Unlock()
	c.tx.Mark("BREAK sent")
	c.settle(ctx, changed, keysWait, false)
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return text(false, c.output()), nil, nil
}

// bmcPromptTimeout bounds one switch between the console and the BMC prompt.
var bmcPromptTimeout = 30 * time.Second

func (s *server) bmcPrompt(ctx context.Context, _ *mcp.CallToolRequest, in bmcPromptIn) (*mcp.CallToolResult, any, error) {
	c, err := s.session(in.Session)
	if err != nil {
		return fail(err)
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	defer c.touch()
	if err := c.canSend(); err != nil {
		return fail(err)
	}
	if c.tr.Toggle == nil {
		return fail("this console (" + c.tr.Kind + ") has no BMC command line to switch to; only iLO and iDRAC sessions do")
	}
	want := in.To == "bmc"
	if in.To != "bmc" && in.To != "console" {
		return fail("to must be bmc or console")
	}
	if c.onBMC() == want {
		return text(false, "The session is already at the "+in.To+"."), nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, bmcPromptTimeout)
	defer cancel()
	changed := c.changes()
	// Toggle writes to the console, so like every other write it can't be
	// allowed to block forever while holding the send lock.
	var kind string
	var at bool
	err = c.guarded(ctx, func() error {
		var e error
		kind, at, e = c.tr.Toggle()
		return e
	})
	if err != nil {
		if errors.Is(err, errStuck) {
			s.stuck(c, err) // kind and at may still be written by the stuck call: don't read them
			return fail("couldn't switch: ", err)
		}
	}
	c.mu.Lock()
	c.kind, c.atBMC = kind, at
	c.mu.Unlock()
	if err != nil {
		return fail("couldn't switch: ", shareError(err))
	}
	c.tx.Mark("switched to " + kind)
	c.settle(ctx, changed, keysWait, false)
	return text(false, "Now at "+kind+".", c.output()), nil, nil
}

func (s *server) disconnectTool(_ context.Context, _ *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	c, ok := s.sessions[in.Session]
	why, gone := s.gone[in.Session]
	s.mu.Unlock()
	if !ok {
		if gone {
			return text(false, "Session "+in.Session+" was already closed ("+why+")."), nil, nil
		}
		return text(false, "No such session."), nil, nil
	}
	s.forget(c, "disconnected")
	c.close()
	msg := fmt.Sprintf("Disconnected from %s.", c.host)
	c.mu.Lock()
	if c.logPath != "" {
		msg += " Log saved: " + filepath.Base(c.logPath)
	}
	c.mu.Unlock()
	return text(false, msg), nil, nil
}
