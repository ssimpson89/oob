package bmc

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// fakeBMC records what is typed and answers with out(typed) as remote output.
type fakeBMC struct {
	l    *cliLink
	sent []string
	out  func(typed string) string
}

func (f *fakeBMC) Write(b []byte) (int, error) {
	f.sent = append(f.sent, string(b))
	if f.out != nil {
		if o := f.out(string(b)); o != "" {
			go f.l.Observe([]byte(o))
		}
	}
	return len(b), nil
}
func (f *fakeBMC) Close() error { return nil }

func newFakeCLI(sp cliSpec, atPrompt bool, prompt string) (*cliLink, *fakeBMC) {
	f := &fakeBMC{}
	l := newCLILink(&sshLink{stdin: f}, sp, atPrompt, false)
	l.closeSess = func() error { return nil }
	f.l = l
	f.out = func(typed string) string {
		if typed == string(sp.esc) {
			return "\r\n" + prompt
		}
		return ""
	}
	return l, f
}

func TestCLILinkToggle(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sp           cliSpec
		prompt       string
		esc          string
		toConsole    []string
		kind, pkind  string
		promptOnCmds bool
	}{
		{"ilo", iloSpec, "hpeiLO-> ", "\x1b(", []string{"vsp\r"}, "vsp", "ilo cli", false},
		{"idrac", idracSpec, "racadm>> ", "\x1c", []string{"console com2\r"}, "com2", "idrac cli", false},
		{"supermicro", smcSpec, "/system1-> ", "\x1b(", []string{"cd /system1/sol1\r", "start\r"}, "sol", "smash", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, f := newFakeCLI(tc.sp, false, tc.prompt)
			kind, at, err := l.Toggle()
			if err != nil || !at || kind != tc.pkind {
				t.Fatalf("to prompt: %q %v %v", kind, at, err)
			}
			if len(f.sent) != 1 || f.sent[0] != tc.esc {
				t.Fatalf("sent %q, want only the escape", f.sent)
			}
			f.sent = nil
			f.out = func(typed string) string { // supermicro prompts after cd
				if tc.promptOnCmds {
					return tc.prompt
				}
				return ""
			}
			kind, at, err = l.Toggle()
			if err != nil || at || kind != tc.kind {
				t.Fatalf("to console: %q %v %v", kind, at, err)
			}
			if strings.Join(f.sent, "|") != strings.Join(tc.toConsole, "|") {
				t.Fatalf("sent %q, want %q", f.sent, tc.toConsole)
			}
		})
	}
}

func TestCLILinkToggleNoPrompt(t *testing.T) {
	l, f := newFakeCLI(iloSpec, false, "")
	f.out = nil
	kind, at, err := l.Toggle()
	if err == nil || at || kind != "vsp" {
		t.Fatalf("got %q %v %v, want an error and still in the console", kind, at, err)
	}
}

func TestCLILinkResync(t *testing.T) {
	l, _ := newFakeCLI(iloSpec, false, "")
	if _, at, changed := l.Observe([]byte("login: stuff\r\nhpeiLO-> ")); !at || !changed {
		t.Error("iLO prompt should resync to the prompt state")
	}
	l, _ = newFakeCLI(smcSpec, false, "")
	if _, at, changed := l.Observe([]byte("root@host:~# ")); at || changed {
		t.Error("Supermicro must not resync on a shell-like prompt")
	}
	// `vsp` typed by hand at the iLO prompt: back to the console state.
	l, _ = newFakeCLI(iloSpec, true, "")
	if _, at, changed := l.Observe([]byte("vsp\r\n\r\nVirtual Serial Port Active: COM1\r\n")); at || !changed {
		t.Error("VSP starting should resync to the console state")
	}
}

func TestCLILinkDisconnect(t *testing.T) {
	l, f := newFakeCLI(iloSpec, true, "")
	_ = l.Disconnect()
	if strings.Join(f.sent, "|") != "exit\r" {
		t.Errorf("at prompt sent %q, want only exit", f.sent)
	}
	l, f = newFakeCLI(iloSpec, false, "hpeiLO-> ")
	_ = l.Disconnect()
	if strings.Join(f.sent, "|") != "\x1b(|exit\r" {
		t.Errorf("in console sent %q, want escape then exit", f.sent)
	}
}

// If the escape doesn't bring the BMC prompt back, "exit" must not be typed:
// it would land on the server's console (at a login prompt, as a username).
func TestCLILinkDisconnectWithoutPromptHangsUp(t *testing.T) {
	defer func(d time.Duration) { disconnectPromptWait = d }(disconnectPromptWait)
	disconnectPromptWait = 50 * time.Millisecond
	l, f := newFakeCLI(iloSpec, false, "") // escape produces no prompt
	closed := false
	l.closeSess = func() error { closed = true; return nil }
	_ = l.Disconnect()
	if strings.Join(f.sent, "|") != "\x1b(" {
		t.Errorf("sent %q, want only the escape", f.sent)
	}
	if !closed {
		t.Error("session not closed")
	}
}

func TestSkipEcho(t *testing.T) {
	read := func(t *testing.T, r io.Reader) string {
		t.Helper()
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	chunks := func(parts ...string) io.Reader {
		pr, pw := io.Pipe()
		go func() {
			for _, p := range parts {
				pw.Write([]byte(p))
			}
			pw.Close()
		}()
		return pr
	}
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"echo and CRLF in one chunk", []string{"vsp\r\nVirtual Serial Port Active\r\n"}, "Virtual Serial Port Active\r\n"},
		{"echo split byte by byte", []string{"v", "s", "p", "\r", "\n", "Hello"}, "Hello"},
		{"echo only", []string{"vsp\r\n"}, ""},
		{"no echo", []string{"Virtual Serial Port Active\r\n"}, "Virtual Serial Port Active\r\n"},
		{"partial prefix then different", []string{"vs", "x data"}, "vsx data"},
		{"stream ends mid prefix", []string{"vs"}, "vs"},
		{"later vsp is kept", []string{"vsp\r\nabc vsp\r\n"}, "abc vsp\r\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := read(t, newSkipEcho(chunks(tc.in...), "vsp")); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("small reads", func(t *testing.T) {
		r := iotest.OneByteReader(newSkipEcho(strings.NewReader("vsp\r\nabcdef"), "vsp"))
		if got := read(t, r); got != "abcdef" {
			t.Errorf("got %q", got)
		}
	})
}

func TestAwaitConsole(t *testing.T) {
	sess := func(out string) *cliSession {
		r, w := io.Pipe()
		go func() { _, _ = io.WriteString(w, out) }()
		return newCLISession(r, io.Discard, iloSpec)
	}
	// Another session holds VSP: the iLO says so and shows its prompt again.
	s := sess("Starting virtual serial port\r\nPress 'ESC (' to return to the CLI Session\r\n" +
		"xyzVirtual Serial Port is already in use: from vsp.\r\r\n\r\n</>hpeiLO-> ")
	if err := s.awaitConsole(iloSpec.entered, time.Second); err == nil || !strings.Contains(err.Error(), "didn't start: Virtual Serial Port is already in use") {
		t.Errorf("busy console: err = %v, want 'already in use'", err)
	}
	// The console started: no error, and its output is still there to show.
	s = sess("Starting virtual serial port\r\nVirtual Serial Port Active: COM1\r\nnode01 login: ")
	if err := s.awaitConsole(iloSpec.entered, time.Second); err != nil {
		t.Fatalf("started console: %v", err)
	}
	buf := make([]byte, 256)
	n, _ := s.Read(buf)
	if !strings.Contains(string(buf[:n]), "Virtual Serial Port Active") {
		t.Errorf("console output lost: %q", buf[:n])
	}
	// Neither within the wait: assume the console is up.
	s = sess("Starting virtual serial port\r\n")
	if err := s.awaitConsole(iloSpec.entered, 100*time.Millisecond); err != nil {
		t.Errorf("silent console: %v", err)
	}
}
