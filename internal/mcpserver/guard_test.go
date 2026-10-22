package mcpserver

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGuardCatchesRebootLikeCommands(t *testing.T) {
	for _, line := range []string{
		"reboot", "sudo reboot", "sudo -i reboot", "systemctl poweroff", "ls; reboot", "true && shutdown -h now",
		"mkfs.xfs /dev/sdb1", "dd if=/dev/zero of=/dev/sda", "wipefs -a /dev/sda", "rm -rf /var/lib/important",
		"echo b > /proc/sysrq-trigger", "dnf remove openssh-server", "ip link set eth0 down", "passwd root",
		"ipmitool chassis power cycle", "init 6",
	} {
		for _, mode := range []struct {
			answer string
			runs   bool
			says   string
		}{{"", false, "can't ask"}, {"decline", false, "the user declined"}, {"accept", true, ""}} {
			h := start(t, Options{}, mode.answer)
			h.connect()
			out, _ := h.send("s1", line)
			ran := len(h.host("node01").entered()) > 0
			if ran != mode.runs {
				t.Errorf("%q with answer %q: ran = %v; said %q", line, mode.answer, ran, out)
			}
			if mode.says != "" && !strings.Contains(out, mode.says) {
				t.Errorf("%q with answer %q said %q", line, mode.answer, out)
			}
			if mode.answer != "" && !strings.Contains(h.askedAll(), line) {
				t.Errorf("%q: the user wasn't shown the command: %q", line, h.askedAll())
			}
		}
	}
}

func TestGuardAllowsOrdinaryCommands(t *testing.T) {
	h := start(t, Options{}, "") // can't ask: anything that needs approval is refused
	h.connect()
	for _, line := range []string{"uname -r", "cat /etc/os-release", "dmesg | tail -20", "systemctl status sshd", "df -h"} {
		if out, isErr := h.send("s1", line); isErr || strings.Contains(out, "approval") {
			t.Errorf("%q: %q %v", line, out, isErr)
		}
	}
}

func TestGuardShowsTheWholeCommand(t *testing.T) {
	h := start(t, Options{}, "decline")
	h.connect()
	h.send("s1", "uname -r\nreboot\necho done")
	asked := h.askedAll()
	if !strings.Contains(asked, "uname -r\nreboot   <- this one\necho done") || !strings.Contains(asked, "node01") {
		t.Errorf("approval asked:\n%s", asked)
	}
	if len(h.host("node01").entered()) != 0 {
		t.Errorf("nothing should run before approval; ran %q", h.host("node01").entered())
	}
}

// Splitting a risky command across keystrokes or calls must not get around the check.
func TestNoWayAroundTheChecks(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *harness)
		tool  string
		args  map[string]any
	}{
		{"keys then enter", nil, "send_keys", map[string]any{"text": "reboot", "keys": []string{"enter"}}},
		{"typed in two calls", func(h *harness) { h.keys("s1", "reb") }, "send_keys", map[string]any{"text": "oot", "keys": []string{"enter"}}},
		{"typed then send_command", func(h *harness) { h.keys("s1", "reb") }, "send_command", map[string]any{"line": "oot"}},
		{"typed text ending like a prompt", func(h *harness) { h.keys("s1", "reboot #") }, "send_command", map[string]any{"line": "uname -r"}},
		{"enter after text typed earlier", func(h *harness) { h.keys("s1", "sudo reboot") }, "send_keys", map[string]any{"keys": []string{"enter"}}},
		{"text key by key", nil, "send_keys", map[string]any{"text": "reboo", "keys": []string{"t", "enter"}}},
		{"answering a question nobody approved", func(h *harness) { h.unaskedQuestion() }, "send_keys", map[string]any{"text": "y", "keys": []string{"enter"}}},
		{"answering with send_command", func(h *harness) { h.unaskedQuestion() }, "send_command", map[string]any{"line": "y"}},
		{"recalled line", func(h *harness) { h.keys("s1", "", "up") }, "send_keys", map[string]any{"keys": []string{"enter"}}},
		{"firmware key", nil, "send_keys", map[string]any{"keys": []string{"f10"}}},
		{"bootloader prompt", func(h *harness) { h.send("s1", "grub") }, "send_command", map[string]any{"line": "boot"}},
		{"bootloader typed then entered", func(h *harness) { h.send("s1", "grub"); h.keys("s1", "reboot") }, "send_keys", map[string]any{"keys": []string{"enter"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// "t" isn't a key name: spell the case with text instead.
			if keys, ok := c.args["keys"].([]string); ok && len(keys) == 2 && keys[0] == "t" {
				c.args["text"], c.args["keys"] = "reboot", []string{"enter"}
			}
			h := start(t, Options{}, "")
			h.connect()
			if c.setup != nil {
				c.setup(h)
			}
			before := h.host("node01").rawSent()
			args := map[string]any{"session": "s1", "wait_seconds": 0.3}
			for k, v := range c.args {
				args[k] = v
			}
			out, isErr := h.call(c.tool, args)
			if !isErr || !strings.Contains(out, "nothing was done") && !strings.Contains(out, "nothing was sent") {
				t.Errorf("not refused: %q %v", out, isErr)
			}
			if after := h.host("node01").rawSent(); after != before {
				t.Errorf("the host received %q", strings.TrimPrefix(after, before))
			}
		})
	}
}

func TestAnsweringAQuestion(t *testing.T) {
	// A question no command of ours raised (the user's own, say): answering yes needs the user.
	h := start(t, Options{}, "accept")
	h.connect()
	h.unaskedQuestion()
	h.keys("s1", "y", "enter")
	if !strings.Contains(h.askedAll(), "answers the host's question") {
		t.Errorf("the answer wasn't questioned: %q", h.askedAll())
	}
	if got := strings.Join(h.host("node01").entered(), "|"); got != "y" {
		t.Errorf("host got %q", got)
	}

	// A question raised by a command that needed no approval: answering it needs none either.
	h = start(t, Options{}, "")
	h.connect()
	h.send("s1", "dnf install foo")
	if out, isErr := h.keys("s1", "y", "enter"); isErr || len(h.asked) != 0 {
		t.Errorf("answering a harmless command's question asked or failed: %q %v %v", out, isErr, h.asked)
	}
	// ... and the same through send_command.
	h = start(t, Options{}, "")
	h.connect()
	h.send("s1", "dnf install foo")
	if out, isErr := h.send("s1", "y"); isErr || len(h.asked) != 0 {
		t.Errorf("answering with send_command asked or failed: %q %v %v", out, isErr, h.asked)
	}

	// A question raised by a command the user approved: answering it needs nothing more.
	h = start(t, Options{}, "accept")
	h.connect()
	h.host("node01").respond = func(f *fakeHost, cmd string) bool {
		if cmd == "dnf remove foo" {
			f.mu.Lock()
			f.asking = "y"
			f.mu.Unlock()
			f.emit("Is this ok [y/N]: ")
			return true
		}
		return false
	}
	h.send("s1", "dnf remove foo")
	n := len(h.asked)
	if out, isErr := h.keys("s1", "y", "enter"); isErr || len(h.asked) != n {
		t.Errorf("answering the approved command's question asked again or failed: %q %v", out, isErr)
	}

	// Declining or stopping never needs the user.
	for _, c := range []struct {
		text string
		keys []string
	}{{"n", []string{"enter"}}, {"no", []string{"enter"}}, {"", []string{"ctrl+c"}}} {
		h := start(t, Options{}, "")
		h.connect()
		h.unaskedQuestion()
		if out, isErr := h.keys("s1", c.text, c.keys...); isErr {
			t.Errorf("%q %v was refused: %q", c.text, c.keys, out)
		}
	}
}

func TestSendKeysPressesKeys(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	for _, c := range []struct {
		keys []string
		want string
	}{
		{[]string{"up"}, "\x1b[A"}, {[]string{"down", "left", "right"}, "\x1b[B\x1b[D\x1b[C"},
		{[]string{"tab"}, "\t"}, {[]string{"ctrl+c"}, "\x03"}, {[]string{"ctrl+d"}, "\x04"}, {[]string{"esc"}, "\x1b"},
		{[]string{"f2"}, "\x1bOQ"}, {[]string{"f12"}, "\x1b[24~"}, {[]string{"pageup", "pagedown"}, "\x1b[5~\x1b[6~"},
		{[]string{"home", "end", "delete"}, "\x1b[H\x1b[F\x1b[3~"}, {[]string{"ctrl+u"}, "\x15"}, {[]string{"space"}, " "},
	} {
		before := h.host("node01").rawSent()
		if out, isErr := h.keys("s1", "", c.keys...); isErr {
			t.Errorf("%v: %q", c.keys, out)
		}
		if got := strings.TrimPrefix(h.host("node01").rawSent(), before); got != c.want {
			t.Errorf("%v sent %q, want %q", c.keys, got, c.want)
		}
	}
	// The escape characters of SOL and the iDRAC console can't be sent.
	for _, k := range []string{"ctrl+^", "ctrl+\\", "ctrl+]", "bogus"} {
		if out, isErr := h.keys("s1", "", k); !isErr {
			t.Errorf("key %q was accepted: %q", k, out)
		}
	}
	for _, text := range []string{"a\rb", "a\x1b[A", "a\x1eb", "a\tb"} {
		if out, isErr := h.keys("s1", text); !isErr {
			t.Errorf("text %q was accepted: %q", text, out)
		}
	}
	if out, isErr := h.call("send_keys", map[string]any{"session": "s1"}); !isErr {
		t.Errorf("empty send_keys accepted: %q", out)
	}
}

func TestSendBreak(t *testing.T) {
	for _, c := range []struct {
		answer string
		sends  bool
		says   string
	}{{"", false, "can't ask"}, {"decline", false, "the user declined"}, {"accept", true, "BREAK"}} {
		h := start(t, Options{}, c.answer)
		h.connect()
		out, isErr := h.call("send_break", map[string]any{"session": "s1"})
		f := h.host("node01")
		if (f.breaks == 1) != c.sends || !strings.Contains(out, c.says) || isErr == c.sends {
			t.Errorf("answer %q: breaks=%d isErr=%v said %q", c.answer, f.breaks, isErr, out)
		}
	}
	// Right after a BREAK the next key may be a SysRq command, so it needs the user too.
	h := start(t, Options{}, "accept")
	h.connect()
	h.call("send_break", map[string]any{"session": "s1"})
	h.keys("s1", "b")
	if !strings.Contains(h.askedAll(), "right after a BREAK") {
		t.Errorf("a key after BREAK wasn't questioned: %q", h.askedAll())
	}
	// A failing BREAK says so.
	h = start(t, Options{}, "accept")
	h.connect()
	h.host("node01").breakErr = errors.New("link refused it")
	if out, isErr := h.call("send_break", map[string]any{"session": "s1"}); !isErr || !strings.Contains(out, "link refused it") {
		t.Errorf("failing break: %q %v", out, isErr)
	}
}

func TestBreakOnILOAndIDRACIsAClearError(t *testing.T) {
	h := start(t, Options{}, "accept")
	h.setup = func(f *fakeHost) { f.toggle = true }
	h.connect()
	out, isErr := h.call("send_break", map[string]any{"session": "s1"})
	if !isErr || !strings.Contains(out, "no serial BREAK") || h.askedAll() != "" {
		t.Errorf("got %q %v, asked %q", out, isErr, h.askedAll())
	}
	if h.host("node01").breaks != 0 {
		t.Error("a BREAK was sent")
	}
}

func TestBMCPrompt(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	if out, isErr := h.call("bmc_prompt", map[string]any{"session": "s1", "to": "bmc"}); !isErr || !strings.Contains(out, "no BMC command line") {
		t.Errorf("SOL has no BMC prompt: %q %v", out, isErr)
	}

	h = start(t, Options{}, "")
	h.setup = func(f *fakeHost) { f.toggle = true }
	h.connect()
	out, isErr := h.call("bmc_prompt", map[string]any{"session": "s1", "to": "bmc"})
	if isErr || !strings.Contains(out, "Now at ilo cli") || !strings.Contains(out, "BMC's own command line") {
		t.Fatalf("to bmc: %q %v", out, isErr)
	}
	// Anything typed at the BMC's command line needs the user, even something harmless.
	if out, isErr := h.send("s1", "show /system1"); !isErr || !strings.Contains(out, "can't ask") {
		t.Errorf("a command at the BMC prompt: %q %v", out, isErr)
	}
	if out, isErr := h.keys("s1", "power off", "enter"); !isErr || !strings.Contains(out, "can't ask") {
		t.Errorf("keys at the BMC prompt: %q %v", out, isErr)
	}
	if got := h.host("node01").entered(); len(got) != 0 {
		t.Errorf("the BMC received %q", got)
	}
	if out, _ := h.call("bmc_prompt", map[string]any{"session": "s1", "to": "bmc"}); !strings.Contains(out, "already") {
		t.Errorf("repeat: %q", out)
	}
	out, isErr = h.call("bmc_prompt", map[string]any{"session": "s1", "to": "console"})
	if isErr || !strings.Contains(out, "Now at vsp") {
		t.Fatalf("to console: %q %v", out, isErr)
	}
	if out, isErr := h.send("s1", "uname -r"); isErr || !strings.Contains(out, "5.14.0-test") {
		t.Errorf("back at the console: %q %v", out, isErr)
	}
}

func TestEscapeToTheBMCNeedsApproval(t *testing.T) {
	h := start(t, Options{}, "")
	h.setup = func(f *fakeHost) { f.toggle = true }
	h.connect()
	// ESC then ( is iLO's escape from VSP to the CLI; split across calls too.
	if out, isErr := h.keys("s1", "(", "enter"); isErr {
		t.Errorf("a ( alone is just text: %q", out)
	}
	if out, isErr := h.keys("s1", "", "ctrl+u"); isErr {
		t.Fatalf("%q", out)
	}
	h.keys("s1", "", "esc")
	before := h.host("node01").rawSent()
	if out, isErr := h.keys("s1", "("); !isErr || !strings.Contains(out, "can't ask") {
		t.Errorf("ESC then ( across calls: %q %v", out, isErr)
	}
	if h.host("node01").rawSent() != before {
		t.Error("the escape was sent")
	}
	h.keys("s1", "", "ctrl+u") // the ESC is no longer the last byte sent
	if out, isErr := h.call("send_keys", map[string]any{"session": "s1", "text": "(", "keys": []string{"esc"}}); isErr {
		t.Errorf("( then ESC isn't the escape: %q", out)
	}
}

// 4340888964: ctrl+d on a nonempty Readline line deletes a character; the
// line after it can't be read, so the next Enter needs approval.
func TestCtrlDOnANonemptyLineNeedsApprovalAtEnter(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	if out, isErr := h.keys("s1", "xreboot"); isErr {
		t.Fatalf("%q", out)
	}
	if out, isErr := h.keys("s1", "", "ctrl+a", "ctrl+d"); isErr {
		t.Fatalf("%q", out)
	}
	before := h.host("node01").rawSent()
	if out, isErr := h.keys("s1", "", "enter"); !isErr || !strings.Contains(out, "can't ask") {
		t.Errorf("Enter after ctrl+a, ctrl+d should need approval: %q %v", out, isErr)
	}
	if h.host("node01").rawSent() != before {
		t.Error("the Enter was sent")
	}
}

// The user is shown the target and the command with anything invisible or
// reordering written out, so a bidi override or escape can't disguise them.
func TestApprovalShowsHiddenCharactersEscaped(t *testing.T) {
	h := start(t, Options{}, "decline")
	h.connect()
	h.keys("s1", "reboot\u202e", "enter")
	asked := h.askedAll()
	if asked == "" {
		t.Fatal("nothing was asked")
	}
	if strings.ContainsRune(asked, '\u202e') || !strings.Contains(asked, `reboot\u202e`) {
		t.Errorf("send_keys text not shown escaped: %q", asked)
	}
	if len(h.host("node01").entered()) != 0 {
		t.Error("ran without approval")
	}
}

func TestVisibleEscapesWhatCanDisguiseText(t *testing.T) {
	in := "node01\u202e (192.0.2.1)\x1b[2J\x00\u200b\u00a0\xff\nreboot\ttab \u00e9 \ufffd"
	got := visible(in)
	for _, r := range got {
		if r != '\n' && !strconv.IsPrint(r) {
			t.Errorf("unprintable %U left in %q", r, got)
		}
	}
	for _, want := range []string{`\u202e`, `\x1b`, `\x00`, `\u200b`, `\u00a0`, `\xff`, `\t`, "\nreboot", "\u00e9", "\ufffd"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	if visible("plain text (192.0.2.1): uname -r") != "plain text (192.0.2.1): uname -r" {
		t.Error("ordinary text changed")
	}
	// The approval target is escaped too.
	if l := (target{name: "node01\u202e", addr: "192.0.2.1"}).label(); strings.ContainsRune(visible(l), '\u202e') {
		t.Error("label not escaped")
	}
}

// A BMC prompt switch that never completes can't hold the session or outlast the call.
func TestStuckBMCPromptEndsAndForgetsTheSession(t *testing.T) {
	old := bmcPromptTimeout
	bmcPromptTimeout = 150 * time.Millisecond
	t.Cleanup(func() { bmcPromptTimeout = old })
	h := start(t, Options{}, "")
	h.setup = func(f *fakeHost) { f.toggle = true }
	h.connect()
	f := h.host("node01")
	f.setStuck()
	began := time.Now()
	out, isErr := h.call("bmc_prompt", map[string]any{"session": "s1", "to": "bmc"})
	if time.Since(began) > 8*time.Second {
		t.Errorf("bmc_prompt took %s", time.Since(began))
	}
	if !isErr || !strings.Contains(out, "stopped accepting input") {
		t.Errorf("bmc_prompt: %q %v", out, isErr)
	}
	eventually(t, "the transport is closed", f.isClosed)
	out, isErr = h.call("read_output", map[string]any{"session": "s1"})
	if !isErr || !strings.Contains(out, "closed") {
		t.Errorf("the session wasn't forgotten: %q %v", out, isErr)
	}
}

// Where the console is comes from the shared console's state. A window that
// switched back to the host console without printing anything leaves the
// session's cached answer stale, and bmc_prompt must not go by it.
func TestBMCPromptGoesByTheSharedConsolesState(t *testing.T) {
	h := start(t, Options{}, "")
	h.setup = func(f *fakeHost) { f.toggle = true }
	h.connect()
	h.srv.mu.Lock()
	c := h.srv.sessions["s1"]
	h.srv.mu.Unlock()
	c.mu.Lock()
	c.atBMC = true // what the output last showed; the console has since moved back
	c.mu.Unlock()
	if out, isErr := h.call("bmc_prompt", map[string]any{"session": "s1", "to": "console"}); isErr || !strings.Contains(out, "already at the console") {
		t.Errorf("to console: %q %v", out, isErr)
	}
	if out, isErr := h.call("bmc_prompt", map[string]any{"session": "s1", "to": "bmc"}); isErr || !strings.Contains(out, "Now at ilo cli") {
		t.Errorf("to bmc: %q %v", out, isErr)
	}
}

// unaskedQuestion has the host print a yes/no question that no command sent
// through the session raised.
func (h *harness) unaskedQuestion() {
	f := h.host("node01")
	f.mu.Lock()
	f.asking = "y"
	f.mu.Unlock()
	f.emit("Is this ok [y/N]: ")
	eventually(h.t, "the question is shown", func() bool {
		c, _ := h.srv.session("s1")
		return c.line().confirming()
	})
}

// In a batch, each harmless line refreshes the answer exemption: the question
// raised by the second line is answerable without approval even though the
// first line produced a new prompt in between.
func TestBatchLineQuestionNeedsNoApproval(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	if out, isErr := h.send("s1", "uname -r\ndnf install foo"); isErr {
		t.Fatalf("batch: %q", out)
	}
	if out, isErr := h.keys("s1", "y", "enter"); isErr || len(h.asked) != 0 {
		t.Errorf("answering the batch's question asked or failed: %q %v %v", out, isErr, h.asked)
	}
}

// A call that never got the keyboard sent nothing, so it must not leave an
// answer exemption behind.
func TestFailedKeyboardLeavesNoAnswerExemption(t *testing.T) {
	for _, tool := range []string{"send_command", "send_keys"} {
		t.Run(tool, func(t *testing.T) {
			h := start(t, Options{}, "")
			h.connect()
			old := beforeKeyboard
			t.Cleanup(func() { beforeKeyboard = old })
			beforeKeyboard = func(c *session) { beforeKeyboard = old; c.sh.Close() }
			var out string
			var isErr bool
			if tool == "send_command" {
				out, isErr = h.send("s1", "uname -r")
			} else {
				out, isErr = h.keys("s1", "uname -r", "enter")
			}
			if !isErr {
				t.Fatalf("expected the keyboard to fail: %q", out)
			}
			c, _ := h.srv.session("s1")
			c.mu.Lock()
			at, prompts := c.answersAt, c.prompts
			c.mu.Unlock()
			if at == prompts {
				t.Errorf("answersAt = %d: an exemption was recorded for a command that was never sent", at)
			}
		})
	}
}
