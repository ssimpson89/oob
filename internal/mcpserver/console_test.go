package mcpserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/console"
)

func (h *harness) send(session, line string) (string, bool) {
	h.t.Helper()
	return h.call("send_command", map[string]any{"session": session, "line": line, "wait_seconds": 0.5})
}

func (h *harness) keys(session, text string, keys ...string) (string, bool) {
	h.t.Helper()
	args := map[string]any{"session": session, "wait_seconds": 0.3}
	if text != "" {
		args["text"] = text
	}
	if len(keys) > 0 {
		args["keys"] = keys
	}
	return h.call("send_keys", args)
}

func TestConsoleSessionLifecycle(t *testing.T) {
	h := start(t, Options{}, "")
	out, isErr := h.call("connect", map[string]any{"host": "node01"})
	if isErr || !strings.Contains(out, "Connected to node01 as session s1 (sol console)") || !strings.Contains(out, "[root@node01 ~]# ") {
		t.Fatalf("connect: %q %v", out, isErr)
	}
	// Connecting again returns the session.
	if again, _ := h.call("connect", map[string]any{"host": "node01"}); !strings.Contains(again, "Already connected to node01 as session s1") {
		t.Errorf("second connect: %q", again)
	}
	if n := strings.Count(h.called(), "open console"); n != 1 {
		t.Errorf("console opened %d times", n)
	}

	out, isErr = h.send("s1", "uname -r")
	if isErr || !strings.Contains(out, "5.14.0-test") || !strings.HasSuffix(strings.TrimSpace(out), "#") {
		t.Fatalf("send_command: %q %v", out, isErr)
	}
	if got := h.host("node01").entered(); len(got) != 1 || got[0] != "uname -r" {
		t.Errorf("host got %q", got)
	}
	// Output is returned once.
	if out, _ := h.call("read_output", map[string]any{"session": "s1"}); !strings.Contains(out, "no new") {
		t.Errorf("read_output repeated output: %q", out)
	}
	// Something the host prints on its own is picked up, and waiting for it works.
	h.host("node01").emit("kernel: heartbeat\r\n")
	out, _ = h.call("read_output", map[string]any{"session": "s1", "wait_seconds": 2, "until": "heartbeat"})
	if !strings.Contains(out, "kernel: heartbeat") {
		t.Errorf("read_output until: %q", out)
	}
	if out, _ := h.call("read_output", map[string]any{"session": "s1", "wait_seconds": 0.2, "until": "never appears"}); !strings.Contains(out, "didn't appear") {
		t.Errorf("until that never comes: %q", out)
	}
	out, _ = h.call("read_history", map[string]any{"session": "s1", "lines": 5})
	if !strings.Contains(out, "5.14.0-test") || len(strings.Split(out, "\n")) > 5 {
		t.Errorf("read_history: %q", out)
	}
	out, _ = h.call("read_history", map[string]any{"session": "s1", "search": "HEARTBEAT"}) // exact case: no match
	if out != "(nothing found)" {
		t.Errorf("history is smart-case: %q", out)
	}
	if out, _ := h.call("read_history", map[string]any{"session": "s1", "search": "heartbeat"}); !strings.Contains(out, "kernel: heartbeat") {
		t.Errorf("history search: %q", out)
	}
	if out, isErr := h.call("read_history", map[string]any{"session": "s1", "lines": 5000}); !isErr {
		t.Errorf("too many lines accepted: %q", out)
	}

	// send_keys: text, then keys.
	out, isErr = h.keys("s1", "uname", "space")
	if isErr || !strings.Contains(out, "uname ") {
		t.Errorf("typing without enter: %q %v", out, isErr)
	}
	out, isErr = h.keys("s1", "-r", "enter")
	if isErr || !strings.Contains(out, "5.14.0-test") {
		t.Errorf("send_keys: %q %v", out, isErr)
	}

	out, isErr = h.call("disconnect", map[string]any{"session": "s1"})
	if isErr || !strings.Contains(out, "Disconnected from node01") {
		t.Errorf("disconnect: %q %v", out, isErr)
	}
	if !h.host("node01").isClosed() {
		t.Error("the transport wasn't closed")
	}
	if out, isErr := h.call("read_output", map[string]any{"session": "s1"}); !isErr || !strings.Contains(out, "closed") {
		t.Errorf("a closed session: %q %v", out, isErr)
	}
	if out, _ := h.call("disconnect", map[string]any{"session": "s1"}); !strings.Contains(out, "already closed") {
		t.Errorf("second disconnect: %q", out)
	}
	if out, isErr := h.call("read_output", map[string]any{"session": "s99"}); !isErr || !strings.Contains(out, "connect first") {
		t.Errorf("unknown session: %q %v", out, isErr)
	}
	// And a new connection works.
	if id := h.connect(); id != "s2" {
		t.Errorf("new session id %q", id)
	}
}

func TestConnectRefusals(t *testing.T) {
	h := start(t, Options{}, "")
	if out, isErr := h.call("connect", map[string]any{"host": "node0"}); !isErr || !strings.Contains(out, "no such host") {
		t.Errorf("prefix: %q %v", out, isErr)
	}
	if out, isErr := h.call("connect", map[string]any{"host": "nope"}); !isErr {
		t.Errorf("unknown host: %q", out)
	}
	h.openErr = errors.New("no way to log in over SSH \x1b[31m(test)")
	out, isErr := h.call("connect", map[string]any{"host": "node01"})
	if !isErr || !strings.Contains(out, "can't open the console on node01") || strings.Contains(out, "\x1b") {
		t.Errorf("open failure: %q %v", out, isErr)
	}
	h.openErr = nil

	small := start(t, Options{MaxSessions: 1}, "")
	small.connect()
	if out, isErr := small.call("connect", map[string]any{"host": "login1"}); !isErr || !strings.Contains(out, "most allowed") {
		t.Errorf("session limit: %q %v", out, isErr)
	}
}

func TestConnectForceNeedsApproval(t *testing.T) {
	for _, c := range []struct {
		answer string
		opens  bool
	}{{"", false}, {"decline", false}, {"accept", true}} {
		h := start(t, Options{}, c.answer)
		out, _ := h.call("connect", map[string]any{"host": "node01", "force": true})
		if opened := strings.Contains(h.called(), "open console force=true"); opened != c.opens {
			t.Errorf("answer %q: opened = %v, said %q", c.answer, opened, out)
		}
	}
}

func TestConnectNoticesAConsoleThatEndsAtOnce(t *testing.T) {
	h := start(t, Options{}, "")
	// The quiet timer must not decide this: with it out of the picture, only
	// the console ending can end connect's wait, however slowly the host runs.
	quietFor = time.Minute
	h.setup = func(f *fakeHost) { f.emit("Error: SOL payload already active\r\n"); f.finish() }
	out, isErr := h.call("connect", map[string]any{"host": "node01"})
	if !isErr || !strings.Contains(out, "ended right after connecting") {
		t.Errorf("connect: %q %v", out, isErr)
	}
	if n := len(h.srv.sessions); n != 0 {
		t.Errorf("%d sessions left open", n)
	}
}

// The output hanging up is enough to know the console ended, even if its exit
// status never arrives; connect must not return success after the full wait.
func TestConnectNoticesAnOutputThatHangsUpWithoutAnExitStatus(t *testing.T) {
	h := start(t, Options{}, "")
	quietFor = time.Minute
	oldGrace := connectEndGrace
	connectEndGrace = 50 * time.Millisecond
	t.Cleanup(func() { connectEndGrace = oldGrace })
	h.setup = func(f *fakeHost) {
		f.emit("Error: SOL payload already active\r\n")
		go func() { time.Sleep(10 * time.Millisecond); f.pw.Close() }() // no f.done
	}
	begin := time.Now()
	out, isErr := h.call("connect", map[string]any{"host": "node01"})
	if !isErr || !strings.Contains(out, "ended right after connecting") {
		t.Errorf("connect: %q %v", out, isErr)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("connect took %v, it should notice the hang-up, not wait out the full settle", d)
	}
	if n := len(h.srv.sessions); n != 0 {
		t.Errorf("%d sessions left open", n)
	}
}

func TestIdleSessionsAreClosed(t *testing.T) {
	h := start(t, Options{IdleTimeout: 150 * time.Millisecond}, "")
	h.connect()
	f := h.host("node01")
	deadline := time.Now().Add(5 * time.Second)
	for !f.isClosed() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !f.isClosed() {
		t.Fatal("the idle session was never closed")
	}
	out, isErr := h.call("read_output", map[string]any{"session": "s1"})
	if !isErr || !strings.Contains(out, "idle") {
		t.Errorf("after idle cleanup: %q %v", out, isErr)
	}
}

func TestSessionsStayOpenWhileInUse(t *testing.T) {
	h := start(t, Options{IdleTimeout: 300 * time.Millisecond}, "")
	h.connect()
	for range 8 {
		time.Sleep(100 * time.Millisecond)
		if out, isErr := h.call("read_output", map[string]any{"session": "s1"}); isErr {
			t.Fatalf("a session in use was closed: %q", out)
		}
	}
}

func TestSessionsCloseWhenTheServerExits(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	h.call("connect", map[string]any{"host": "login1"})
	a, b := h.host("node01"), h.host("login1")
	h.srv.closeAll()
	if !a.isClosed() || !b.isClosed() {
		t.Errorf("sessions left open: node01 closed=%v login1 closed=%v", a.isClosed(), b.isClosed())
	}
}

func TestMultipleSessionsAreIndependent(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	out, _ := h.call("connect", map[string]any{"host": "login1"})
	if !strings.Contains(out, "session s2") {
		t.Fatalf("second connect: %q", out)
	}
	done := make(chan string, 2)
	go func() { o, _ := h.send("s1", "uname -r"); done <- o }()
	go func() { o, _ := h.send("s2", "echo hi"); done <- o }()
	a, b := <-done, <-done
	both := a + b
	if !strings.Contains(both, "5.14.0-test") || !strings.Contains(both, "sh: echo hi: ok") {
		t.Errorf("results %q / %q", a, b)
	}
	if got := h.host("node01").entered(); len(got) != 1 || got[0] != "uname -r" {
		t.Errorf("node01 got %q", got)
	}
	if got := h.host("login1").entered(); len(got) != 1 || got[0] != "echo hi" {
		t.Errorf("login1 got %q", got)
	}
}

func TestMultiLineCommandsRunOneAfterAnother(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	out, isErr := h.send("s1", "uname -r\necho two")
	if isErr || !strings.Contains(out, "5.14.0-test") || !strings.Contains(out, "sh: echo two: ok") {
		t.Errorf("got %q %v", out, isErr)
	}
	if got := strings.Join(h.host("node01").entered(), "|"); got != "uname -r|echo two" {
		t.Errorf("host got %q", got)
	}
	for _, bad := range []string{"", "  ", "a\n\nb", "a\x01b", strings.Repeat("x", commandBytes+1), strings.Repeat("a\n", commandLines+1)} {
		if out, isErr := h.send("s1", bad); !isErr {
			t.Errorf("%q was accepted: %q", bad, out)
		}
	}
	if out, isErr := h.call("send_command", map[string]any{"session": "s1", "line": "uname -r", "wait_seconds": 500}); !isErr {
		t.Errorf("a huge wait was accepted: %q", out)
	}
}

func TestStopsAtAQuestion(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	out, isErr := h.send("s1", "dnf install foo\necho after")
	if isErr || !strings.Contains(out, "asking a question") || !strings.Contains(out, "weren't sent") {
		t.Errorf("got %q %v", out, isErr)
	}
	if got := strings.Join(h.host("node01").entered(), "|"); got != "dnf install foo" {
		t.Errorf("host got %q", got)
	}
}

func TestConsoleOutputIsSanitized(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"send_command", map[string]any{"session": "s1", "line": "evil", "wait_seconds": 0.5}},
		{"read_history", map[string]any{"session": "s1"}},
	} {
		out, _ := h.call(c.tool, c.args)
		if !strings.Contains(out, "red") || !strings.Contains(out, "ok") {
			t.Errorf("%s lost the text: %q", c.tool, out)
		}
		for _, bad := range []string{"\x1b", "\x07", "\x9b", "\u202e", "\u202c", "evil title"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s output has %q: %q", c.tool, bad, out)
			}
		}
	}
}

func TestConsoleOutputIsBounded(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	out, isErr := h.call("send_command", map[string]any{"session": "s1", "line": "flood", "wait_seconds": 5})
	if isErr {
		t.Fatalf("flood: %q", out)
	}
	if len(out) > shownBytesMax+2048 || strings.Count(out, "\n") > shownLinesMax+10 {
		t.Errorf("result is %d bytes, %d lines", len(out), strings.Count(out, "\n"))
	}
	if !strings.Contains(out, "earlier lines not shown") || !strings.Contains(out, "log line 2999") {
		t.Errorf("should say lines were cut and keep the newest: %q", out[:min(len(out), 200)])
	}
	hist, _ := h.call("read_history", map[string]any{"session": "s1", "lines": 300})
	if len(hist) > shownBytesMax+2048 {
		t.Errorf("history is %d bytes", len(hist))
	}
}

func TestSessionLog(t *testing.T) {
	dir := t.TempDir()
	h := start(t, Options{Log: true, LogDir: dir}, "")
	out, _ := h.call("connect", map[string]any{"host": "node01"})
	if !strings.Contains(out, "Logging to node01-") || strings.Contains(out, dir) {
		t.Errorf("connect should name the log by file name only: %q", out)
	}
	h.send("s1", "uname -r")
	out, _ = h.call("disconnect", map[string]any{"session": "s1"})
	if !strings.Contains(out, "Log saved: node01-") || strings.Contains(out, dir) {
		t.Errorf("disconnect: %q", out)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "node01-*.log"))
	if len(files) != 1 {
		t.Fatalf("log files %v", files)
	}
	b, _ := os.ReadFile(files[0])
	for _, want := range []string{"5.14.0-test", "uname -r", "agent typed", "session closed"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("log lacks %q:\n%s", want, b)
		}
	}
	if st, _ := os.Stat(files[0]); st.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v", st.Mode().Perm())
	}
	// Off by default.
	h2 := start(t, Options{LogDir: t.TempDir()}, "")
	if out, _ := h2.call("connect", map[string]any{"host": "node01"}); !strings.Contains(out, "Not logging") {
		t.Errorf("logging should be off by default: %q", out)
	}
}

// On a serial console everything typed goes through the paced queue: a byte at
// a time with a gap, so the BMC's small buffer can't overrun. On anything else
// a line is one write.
func TestSendCommandOnASerialConsoleIsTypedByteByByte(t *testing.T) {
	ms := time.Millisecond
	h := start(t, Options{}, "")
	h.setup = func(f *fakeHost) { f.serial, f.pacing = true, console.Pacing{Char: &ms, Line: &ms} }
	h.connect()
	line := "echo " + strings.Repeat("abcdefghij", 10)
	out, isErr := h.send("s1", line+"\necho two")
	if isErr || !strings.Contains(out, "sh: echo two: ok") {
		t.Fatalf("got %q %v", out, isErr)
	}
	f := h.host("node01")
	if got := strings.Join(f.entered(), "|"); got != line+"|echo two" {
		t.Errorf("host got %q", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maxWrite != 1 {
		t.Errorf("longest write was %d bytes; a serial console is typed a byte at a time", f.maxWrite)
	}
}

func TestSendCommandOnAnSSHConsoleIsOneWritePerLine(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	line := "echo " + strings.Repeat("abcdefghij", 10)
	if out, isErr := h.send("s1", line); isErr || !strings.Contains(out, "sh: "+line+": ok") {
		t.Fatalf("got %q %v", out, isErr)
	}
	f := h.host("node01")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writes != 1 || f.maxWrite != len(line)+1 {
		t.Errorf("%d writes, longest %d; want the line and Enter in one write", f.writes, f.maxWrite)
	}
}

// A write that fails stops the call and says so; nothing later is typed.
func TestSendCommandReportsAWriteError(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	f := h.host("node01")
	f.mu.Lock()
	f.ended = true // the console's input is closed, as when the connection drops
	f.mu.Unlock()
	out, isErr := h.send("s1", "uname -r\necho two")
	if !isErr || !strings.Contains(out, "Sending stopped") && !strings.Contains(out, "ended") {
		t.Errorf("got %q %v, want an error", out, isErr)
	}
	if got := f.entered(); len(got) != 0 {
		t.Errorf("host ran %q after the write failed", got)
	}
}

// wait_seconds is how long to wait for the console's answer, not for the line
// to be typed: a slow paced line must not use the wait up before Enter.
func TestSendCommandWaitStartsAfterTyping(t *testing.T) {
	h := start(t, Options{}, "")
	h.setup = func(f *fakeHost) {
		ms := time.Millisecond
		f.sendDelay, f.serial, f.pacing = 15*time.Millisecond, true, console.Pacing{Char: &ms, Line: &ms}
		f.respond = func(f *fakeHost, cmd string) bool {
			go func() {
				time.Sleep(300 * time.Millisecond)
				f.emit("late-result\r\n" + f.prompt)
			}()
			return true
		}
	}
	h.connect()
	line := "echo " + strings.Repeat("abcdefgh", 5) // typed a byte at a time: 0.7s or more
	start := time.Now()
	out, isErr := h.call("send_command", map[string]any{"session": "s1", "line": line, "wait_seconds": 0.6})
	if isErr || !strings.Contains(out, "late-result") {
		t.Errorf("answer arrived %.1fs after typing began but was missed: %q %v", time.Since(start).Seconds(), out, isErr)
	}
}

// A console whose output hangs up with no exit status must end runLine at
// once, not spin until wait_seconds passes.
func TestRunLineStopsWhenOutputClosed(t *testing.T) {
	h := start(t, Options{}, "")
	h.connect()
	f := h.host("node01")
	c, err := h.srv.session("s1")
	if err != nil {
		t.Fatal(err)
	}
	f.emit("compiling...") // cursor not at a prompt, so only EOF can end the wait
	time.Sleep(100 * time.Millisecond)
	f.pw.Close() // output EOF; Done is never sent
	select {
	case <-c.eofC:
	case <-time.After(10 * time.Second): // a shared console waits endWait for the exit status first
		t.Fatal("output EOF not seen")
	}
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.runLine(context.Background(), "uptime", 5*time.Second) }()
	select {
	case <-done:
		if d := time.Since(start); d > time.Second {
			t.Errorf("runLine took %v after output closed", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runLine kept waiting after the output closed")
	}
}
