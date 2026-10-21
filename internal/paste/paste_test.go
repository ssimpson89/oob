// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// Copied from bauddy (internal/paste/paste_test.go), with the simdev import repointed.

package paste

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ssimpson89/oob/internal/simdev"
)

var config = []string{
	"interface Gi1/0/1", "description uplink", "no shutdown", "exit",
	"vlan 10", "name users", "exit",
	"write mem",
	"interface Gi1/0/2", "description server", "switchport access vlan 10", "end",
}

func pasteInto(t *testing.T, d *simdev.Device, cmds []string) ([]string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	d.Start(ctx)
	_ = d.Write([]byte("conf t\r")) // config mode before the paste; the simulator's Write can't fail
	var seen string
	for !strings.Contains(seen, "(config)#") {
		select {
		case b := <-d.Out():
			seen += string(b)
		case <-ctx.Done():
			t.Fatal("simulator never reached config mode")
		}
	}
	prompt := d.Prompt()
	p := &Pacer{Write: d.Write, Rx: d.Out(), Baud: d.Baud, Prompt: prompt}
	err := p.Run(ctx, Lines(strings.Join(cmds, "\n")+"\n"), func(int) {})
	time.Sleep(100 * time.Millisecond)
	return d.Applied()[1:], err // drop "conf t"
}

func TestPacing(t *testing.T) {
	cases := []struct {
		name string
		dev  *simdev.Device
	}{
		{"fast device", &simdev.Device{Baud: 9600, FIFO: 16}},
		{"slow vlan and write", &simdev.Device{Baud: 9600, FIFO: 16, Slow: map[string]time.Duration{"vlan": 300 * time.Millisecond, "write": 3 * time.Second}}},
		{"slow every command", &simdev.Device{Baud: 9600, FIFO: 16, Slow: map[string]time.Duration{"": 400 * time.Millisecond}}},
		{"syslog noise", &simdev.Device{Baud: 9600, FIFO: 16, Noise: 250 * time.Millisecond, Slow: map[string]time.Duration{"vlan": 300 * time.Millisecond}}},
		{"115200", &simdev.Device{Baud: 115200, FIFO: 16, Slow: map[string]time.Duration{"write": time.Second}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := c.dev
			got, err := pasteInto(t, d, config)
			if err != nil {
				t.Fatalf("paste stopped: %v", err)
			}
			if d.Dropped() != 0 || strings.Join(got, "|") != strings.Join(config, "|") {
				t.Errorf("dropped %d bytes; device ran:\n  %s\nwant:\n  %s", d.Dropped(), strings.Join(got, "|"), strings.Join(config, "|"))
			}
		})
	}
}

func TestStopsAtDeviceError(t *testing.T) {
	t.Parallel()
	d := &simdev.Device{Baud: 9600, FIFO: 16}
	got, err := pasteInto(t, d, []string{"interface Gi1/0/1", "bogus command", "no shutdown"})
	var se *StopError
	if !errors.As(err, &se) || se.Line != 2 || !strings.Contains(se.Reason, "Invalid input") {
		t.Fatalf("err = %v, want a stop at line 2 with the device's message", err)
	}
	if strings.Join(got, "|") != "interface Gi1/0/1" {
		t.Errorf("device ran %q; nothing after the error should be sent", got)
	}
}

func TestStopsAtQuestion(t *testing.T) {
	t.Parallel()
	d := &simdev.Device{Baud: 9600, FIFO: 16}
	_, err := pasteInto(t, d, []string{"end", "reload", "interface Gi1/0/1"})
	var se *StopError
	if !errors.As(err, &se) || se.Line != 2 || !strings.Contains(se.Reason, "[confirm]") {
		t.Fatalf("err = %v, want a stop at line 2 asking to confirm", err)
	}
}

func TestBanner(t *testing.T) {
	t.Parallel()
	d := &simdev.Device{Baud: 9600, FIFO: 16}
	cmds := []string{"banner motd #", "Authorised access only.", "Disconnect now if not.", "#", "interface Gi1/0/1"}
	got, err := pasteInto(t, d, cmds)
	if err != nil {
		t.Fatalf("paste stopped: %v", err)
	}
	if strings.Join(got, "|") != strings.Join(cmds, "|") {
		t.Errorf("device ran %q", got)
	}
}

func TestLines(t *testing.T) {
	got := Lines("a\r\nb\rc\n\tindented\x01\nd")
	want := []string{"a\r", "b\r", "c\r", " indented\r", "d"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Lines = %q, want %q", got, want)
	}
}

func TestRisky(t *testing.T) {
	if Risky([]string{"interface Gi1/0/1\r", "no shutdown\r"}) != "" {
		t.Error("small clean paste flagged")
	}
	if r := Risky([]string{"end\r", "reload\r"}); !strings.Contains(r, "line 2") {
		t.Errorf("reload not flagged: %q", r)
	}
	if r := Risky([]string{"end\r", "copy run start\r"}); !strings.Contains(r, "line 2") {
		t.Errorf("copy to startup not flagged: %q", r)
	}
	if r := Risky([]string{"show ip route ?\r", "x\r"}); !strings.Contains(r, "?") {
		t.Errorf("? not flagged: %q", r)
	}
	if Risky(make([]string, 10)) == "" {
		t.Error("10 lines not flagged")
	}
}

func TestDangerous(t *testing.T) {
	for _, l := range []string{
		"reload", "reload in 5", "rel", "relo\r", "  RELOAD", "do reload", "do rel",
		"write erase", "wr er", "wri era", "erase startup-config", "era nvram:",
		"del flash:vlan.dat", "delete /force flash:config.text", "format flash:",
		"copy run start", "cop runn st", "copy tftp: startup-config", "conf rep flash:old",
		"copy running-configuration startup-configuration", "clear configure all",
		"boot system flash:old.bin", "request system reboot", "req sys reb", "run request system reboot",
		"factory-reset", "execute factoryreset2", "exe reb", "configure factory-default",
		"request restart system", "install add file bootflash:x.bin", "/system reboot", "system shutdown",
		"sudo reboot", "sudo -i reboot", "/sbin/reboot", "bash sudo reboot", "shutdown now", "shutdown -h +5",
		"systemctl reboot", "ls; reboot", "true && reboot", "echo `reboot`", "x $(reboot)",
		"mkfs.ext4 /dev/sda1", "dd if=/dev/zero of=/dev/sda", "rm -rf /", "ipmitool chassis power cycle",
		"!!", "!reb", "!-1", "sudo !!", "sudo -u root reboot", "env A=1 reboot", "bash -c 'reboot'", "su -c reboot",
		"eval reboot", "xargs reboot", `\reboot`, "'reboot'", "busybox reboot", "systemctl --force reboot",
		"MODE=recovery reboot", "A=1 B=2 sudo reboot",
	} {
		if !Dangerous(l) {
			t.Errorf("%q not flagged", l)
		}
	}
	for _, l := range []string{
		"show version", "sh run", "sh reload", "re", "write memory", "wr", "copy run tftp:", "conf t",
		"for i in 1 2 3; do echo $i; done", "description reload test", "interface Gi1/0/1",
		"do show ip int brief", "no shutdown", "shutdown", "show reload", "reload cancel",
		"show run | include reload", "show log | match reboot", "!", "! uplink to core", "ls /system/reboot",
		"!Command: show running-config", "!Time: Sun Oct 11 14:00:00 2026", "!!! Authorized use only !!!",
		"exec-timeout 5 0", "time-range work", "echo hello!", "sh -c reboot is show",
		"sudo -u", "sudo -g", "env A=1", // wrappers with nothing after them
	} {
		if Dangerous(l) {
			t.Errorf("%q flagged", l)
		}
	}
}

func TestLongLinesScrolled(t *testing.T) {
	t.Parallel()
	d := &simdev.Device{Baud: 9600, FIFO: 16, Width: 80}
	long := "description " + strings.Repeat("uplink-to-core-distribution-", 4) + "end"
	cmds := []string{"interface Gi1/0/1", long, "no shutdown"}
	got, err := pasteInto(t, d, cmds)
	if err != nil || strings.Join(got, "|") != strings.Join(cmds, "|") {
		t.Fatalf("err %v; device ran %q", err, got)
	}
}

func TestSyslogMidLine(t *testing.T) {
	for _, sync := range []bool{false, true} {
		t.Run(fmt.Sprintf("logging synchronous %v", sync), func(t *testing.T) {
			t.Parallel()
			d := &simdev.Device{Baud: 9600, FIFO: 16, Noise: 70 * time.Millisecond, SyncLog: sync}
			got, err := pasteInto(t, d, config)
			if err != nil {
				t.Fatalf("paste stopped: %v", err)
			}
			if d.Dropped() != 0 || strings.Join(got, "|") != strings.Join(config, "|") {
				t.Errorf("dropped %d; device ran %q", d.Dropped(), got)
			}
		})
	}
}

func TestPasswordPrompt(t *testing.T) {
	t.Parallel()
	d := &simdev.Device{Baud: 9600, FIFO: 16}
	got, err := pasteInto(t, d, []string{"end", "enable", "s3cret", "conf t", "interface Gi1/0/1"})
	want := "end|enable|<password>|conf t|interface Gi1/0/1"
	if err != nil || strings.Join(got, "|") != want {
		t.Fatalf("err %v; device ran %q, want %q", err, got, want)
	}
}

func TestStopsAtCopyQuestion(t *testing.T) {
	t.Parallel()
	d := &simdev.Device{Baud: 9600, FIFO: 16}
	got, err := pasteInto(t, d, []string{"end", "copy run start", "conf t"})
	var se *StopError
	if !errors.As(err, &se) || se.Line != 2 || !strings.Contains(se.Reason, "Destination filename") {
		t.Fatalf("err = %v, want a stop at the copy question", err)
	}
	if len(got) != 1 {
		t.Errorf("device ran %q; the question must not be answered", got)
	}
}

func TestEchoedErrorWordsDontStop(t *testing.T) {
	t.Parallel()
	d := &simdev.Device{Baud: 9600, FIFO: 16}
	cmds := []string{"interface Gi1/0/1", "description unknown command port", "no shutdown"}
	got, err := pasteInto(t, d, cmds)
	if err != nil || strings.Join(got, "|") != strings.Join(cmds, "|") {
		t.Fatalf("err %v; device ran %q", err, got)
	}
}

func TestStopsWhenNothingEchoes(t *testing.T) {
	t.Parallel()
	d := &simdev.Device{Baud: 9600, FIFO: 16, NoEcho: true}
	got, err := pasteInto(t, d, []string{"interface Gi1/0/1", "no shutdown"})
	var se *StopError
	if !errors.As(err, &se) || se.Line != 1 || !strings.Contains(se.Reason, "isn't echoing") {
		t.Fatalf("err = %v, want a stop before pressing Enter", err)
	}
	if len(got) != 0 {
		t.Errorf("device ran %q; Enter must not be pressed blind", got)
	}
}

func TestPatterns(t *testing.T) {
	questions := []string{"Proceed with reload? [confirm]", "Destination filename [startup-config]? ", "Delete flash:x? [yes/no]: ",
		"Do you want to continue? [yes,no] (no) ", " --More-- ", "-- MORE --, next page: Space", "---(more 42%)---", "-- [Q quit|D dump|down]",
		":", "(END)"} // less, after its first page and at the end
	for _, q := range questions {
		if !question.MatchString(q) {
			t.Errorf("question not recognised: %q", q)
		}
	}
	errs := []string{"% Invalid input detected at '^' marker.", "Invalid input: shutdownn", "syntax error.", "Command fail. Return code -61",
		"bad command name foo (line 1 column 1)", "bash: foo: command not found", "% Incomplete command."}
	for _, e := range errs {
		if !devErr.MatchString(e) {
			t.Errorf("error not recognised: %q", e)
		}
	}
	for _, p := range []string{"Switch(config-if)#", "user@sw1# ", "[admin@MikroTik] > ", "rommon 1 >", "Password: "} {
		r := &run{}
		if !r.isPrompt(p) {
			t.Errorf("prompt not recognised: %q", p)
		}
	}
	r := &run{}
	r.learn("Switch(config)#")
	if r.isPrompt("45%") || r.isPrompt("#####") {
		t.Error("progress output taken for a prompt once the host is known")
	}
}

func TestAtLogin(t *testing.T) {
	for line, want := range map[string]bool{
		"Password: ":                  true,
		"[sudo] password for alice: ": true,
		"login as: ":                  true,
		"Enter passcode: ":            true,
		"Enter passphrase for key 'id_ed25519': ": true,
		"Unauthorized login prohibited. Contact:": false,
		"Switch#": false,
	} {
		if got := AtLogin(line); got != want {
			t.Errorf("AtLogin(%q) = %v, want %v", line, got, want)
		}
	}
}

// On a FreeBSD shell, Enter echoes as \r\r\n. Read as a redraw, that blanked
// the line, so a command that printed nothing waited out promptWait.
func TestNoOutputAfterDoubleCR(t *testing.T) {
	t.Parallel()
	rx := make(chan []byte, 64)
	echo := func(b []byte) error { // a shell that echoes, and after Enter prints only its prompt
		for _, ch := range b {
			if ch == '\r' {
				rx <- []byte("\r\r\nroot@fw:~ # ")
			} else {
				rx <- []byte{ch}
			}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := &Pacer{Write: echo, Rx: rx, Baud: 115200, Prompt: "root@fw:~ # "}
	if err := p.Run(ctx, Lines("true\ngrep zzz /etc/hosts\n"), func(int) {}); err != nil {
		t.Fatalf("err = %v, want both lines run with the prompt seen after each", err)
	}
}

// less on a serial console shows the file's name where its prompt would be,
// so it's known by the command: once it goes quiet, stop, rather than wait a
// minute for a prompt that won't come.
func TestFullScreenProgramStopsQuickly(t *testing.T) {
	t.Parallel()
	rx := make(chan []byte, 64)
	pager := func(b []byte) error { // echoes, and after Enter shows a page of the file
		for _, ch := range b {
			if ch == '\r' {
				rx <- []byte("\r\r\n#!/bin/sh\r\n# rc.conf defaults\r\n/etc/defaults/rc.conf")
			} else {
				rx <- []byte{ch}
			}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := &Pacer{Write: pager, Rx: rx, Baud: 115200, Prompt: "root@fw:~ # "}
	t0 := time.Now()
	err := p.Run(ctx, Lines("less /etc/defaults/rc.conf\n"), func(int) {})
	var se *StopError
	if !errors.As(err, &se) || !strings.Contains(se.Reason, "full-screen") || time.Since(t0) > 5*time.Second {
		t.Fatalf("err = %v after %v, want a quick stop for a full-screen program", err, time.Since(t0))
	}
	for cmd, want := range map[string]bool{
		"less /var/log/messages": true, "dmesg | less": true, "man ls": true, "top": true, "vi /etc/hosts": true,
		"top -b -n 1": true, // matched, but it prints and returns to the prompt first
		"  less file": true, "sudo less /var/log/auth.log": true, "/usr/bin/less file": true,
		"topology": false, "show manager": false, "cat file": false, "echo more": false,
	} {
		if got := fullScreen(cmd); got != want {
			t.Errorf("fullScreen(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// At a prompt oob doesn't recognise, a quiet screen usually means ready;
// a pager's isn't, or the next line would be typed into it.
func TestNoTypingIntoAPagerAtAnUnknownPrompt(t *testing.T) {
	t.Parallel()
	rx := make(chan []byte, 64)
	var wrote []byte
	pager := func(b []byte) error {
		wrote = append(wrote, b...)
		for _, ch := range b {
			if ch == '\r' {
				rx <- []byte("\r\n# rc.conf defaults\r\n/etc/defaults/rc.conf")
			} else {
				rx <- []byte{ch}
			}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := &Pacer{Write: pager, Rx: rx, Baud: 115200, Prompt: ""}
	err := p.Run(ctx, Lines("less /etc/defaults/rc.conf\necho next\n"), func(int) {})
	var se *StopError
	if !errors.As(err, &se) || !strings.Contains(se.Reason, "full-screen") || strings.Contains(string(wrote), "echo next") {
		t.Fatalf("err = %v, typed %q: want a stop at the pager, with the next line not typed", err, wrote)
	}
}

// Type is what the console uses for one line (the stty size line) and the MCP
// server for each command: echo-checked, Enter only after the whole line shows.
func TestTypeDeliversLineIntact(t *testing.T) {
	t.Parallel()
	d := &simdev.Device{Baud: 9600, FIFO: 16, Slow: map[string]time.Duration{"": 300 * time.Millisecond}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d.Start(ctx)
	p := &Pacer{Write: d.Write, Rx: d.Out(), Baud: d.Baud, Prompt: d.Prompt()}
	line := "stty rows 50 cols 200"
	if err := p.Type(ctx, line+"\r"); err != nil {
		t.Fatalf("Type: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if got := d.Applied(); len(got) != 1 || got[0] != line || d.Dropped() != 0 {
		t.Errorf("device ran %q, dropped %d; want exactly %q", got, d.Dropped(), line)
	}
}

func TestTypeStopsBeforeEnterWhenNothingEchoes(t *testing.T) {
	t.Parallel()
	d := &simdev.Device{Baud: 9600, FIFO: 16, NoEcho: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d.Start(ctx)
	p := &Pacer{Write: d.Write, Rx: d.Out(), Baud: d.Baud, Prompt: d.Prompt()}
	err := p.Type(ctx, "stty rows 50 cols 200\r")
	var se *StopError
	if !errors.As(err, &se) || !se.Unentered || se.Line != 1 {
		t.Fatalf("err = %v, want a StopError with Enter not pressed", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := d.Applied(); len(got) != 0 {
		t.Errorf("device ran %q; Enter must not be pressed blind", got)
	}
}

// Type leaves the device's output to its caller: an error message in the
// output while typing is not a stop.
func TestTypeIgnoresErrorsInOutput(t *testing.T) {
	t.Parallel()
	rx := make(chan []byte, 8)
	var typed strings.Builder
	write := func(b []byte) error {
		typed.Write(b)
		rx <- b // echo
		return nil
	}
	rx <- []byte("error: link down\r\n")
	p := &Pacer{Write: write, Rx: rx, Baud: 115200, Prompt: "root@h:~ # "}
	if err := p.Type(context.Background(), "ls\r"); err != nil {
		t.Fatalf("Type: %v", err)
	}
	if typed.String() != "ls\r" {
		t.Errorf("typed %q", typed.String())
	}
}

// A byte lost from the start of the line leaves a tail of it on screen ("tty"
// for "stty"). That must read as a mismatch, not as an echo in progress.
func TestFirstByteLostIsAMismatch(t *testing.T) {
	t.Parallel()
	rx := make(chan []byte, 64)
	var entered bool
	first := true
	write := func(b []byte) error {
		if first { // lose the first byte, echo the rest
			first = false
			b = b[1:]
		}
		for _, c := range b {
			if c == '\r' {
				entered = true
			}
		}
		rx <- b
		return nil
	}
	p := &Pacer{Write: write, Rx: rx, Baud: 115200, Prompt: "root@h:~ # "}
	err := p.Type(context.Background(), "stty rows 50 cols 200\r")
	var se *StopError
	if !errors.As(err, &se) || !se.Unentered || !strings.Contains(se.Reason, "something other than what was typed") {
		t.Fatalf("err = %v, want a mismatch stop before Enter", err)
	}
	if entered {
		t.Error("Enter was pressed on a line that lost its first byte")
	}
}

// The no-echo matcher takes the qualifiers the login matcher does.
func TestPasswordMatcherQualifiers(t *testing.T) {
	for prompt, want := range map[string]bool{
		"Password: ":                              true,
		"[sudo] password for alice: ":             true,
		"Enter passphrase for key 'id_ed25519': ": true,
		"Enter passcode: ":                        true,
		"Username: ":                              false,
		"Switch#":                                 false,
	} {
		if got := password.MatchString(prompt); got != want {
			t.Errorf("password.MatchString(%q) = %v, want %v", prompt, got, want)
		}
	}
}

// echoRig is a Pacer whose writes echo (after dropping the first n bytes of
// the first write) into a buffered channel, and which records what it sent.
type echoRig struct {
	rx      chan []byte
	writes  []string
	times   []time.Time
	entered bool
}

func (e *echoRig) write(b []byte) error {
	e.writes = append(e.writes, string(b))
	e.times = append(e.times, time.Now())
	if strings.Contains(string(b), "\r") {
		e.entered = true
	}
	return nil
}

func newEchoRig() *echoRig { return &echoRig{rx: make(chan []byte, 256)} }

func stopsUnentered(t *testing.T, err error, e *echoRig) {
	t.Helper()
	var se *StopError
	if !errors.As(err, &se) || !se.Unentered {
		t.Fatalf("err = %v, want a StopError with Enter not pressed", err)
	}
	if e.entered {
		t.Error("Enter was pressed on a line that lost its first byte")
	}
}

// With nothing on the cursor line to begin with, a tail of the line ("tty")
// must not pass for the whole echo.
func TestFirstByteLostWithEmptyCursor(t *testing.T) {
	t.Parallel()
	e := newEchoRig()
	first := true
	p := &Pacer{Baud: 115200, Prompt: "", Rx: e.rx, Write: func(b []byte) error {
		_ = e.write(b)
		if first {
			first, b = false, b[1:]
		}
		e.rx <- b
		return nil
	}}
	stopsUnentered(t, p.Type(context.Background(), "stty rows 50 cols 200\r"), e)
}

// A syslog line landing before the first echo leaves the cursor line empty
// again, so a line that lost its first byte must still be refused.
func TestFirstByteLostAfterSyslogNewline(t *testing.T) {
	t.Parallel()
	e := newEchoRig()
	first := true
	p := &Pacer{Baud: 115200, Prompt: "root@h:~ # ", Rx: e.rx, Write: func(b []byte) error {
		_ = e.write(b)
		if first {
			first, b = false, b[1:]
			e.rx <- []byte("\r\n<189>link state changed\r\n")
		}
		e.rx <- b
		return nil
	}}
	stopsUnentered(t, p.Type(context.Background(), "stty rows 50 cols 200\r"), e)
}

// Once the start of the line has been verified, an interruption (syslog
// scrolling the prompt away) may leave only the tail on screen.
func TestSyslogAfterVerifiedPrefixStillCompletes(t *testing.T) {
	t.Parallel()
	e := newEchoRig()
	n := 0
	p := &Pacer{Baud: 115200, Prompt: "root@h:~ # ", Rx: e.rx, Write: func(b []byte) error {
		_ = e.write(b)
		n++
		if n == 2 { // the first 8 bytes are on screen; a message arrives, then the rest echo with no prompt
			e.rx <- []byte("\r\n<189>link state changed\r\n")
		}
		e.rx <- b
		return nil
	}}
	if err := p.Type(context.Background(), "stty rows 50 cols 200\r"); err != nil {
		t.Fatalf("Type: %v", err)
	}
	if !e.entered {
		t.Error("Enter not pressed")
	}
}

// A password is not echoed, but it still goes out in small writes with a
// pause between them, then Enter.
func TestPasswordIsPacedInSmallWrites(t *testing.T) {
	t.Parallel()
	e := newEchoRig()
	secret := "correct-horse-battery-staple-1234"
	p := &Pacer{Baud: 115200, Prompt: "[sudo] password for alice: ", Rx: e.rx, Write: e.write}
	if err := p.Type(context.Background(), secret+"\r"); err != nil {
		t.Fatalf("Type: %v", err)
	}
	if got := strings.Join(e.writes, ""); got != secret+"\r" {
		t.Fatalf("typed %q, want %q", got, secret+"\r")
	}
	for i, w := range e.writes {
		if len(strings.TrimSuffix(w, "\r")) > blindChunk {
			t.Errorf("write %d is %d bytes, want at most %d", i, len(w), blindChunk)
		}
		if i > 0 {
			if gap := e.times[i].Sub(e.times[i-1]); gap < blindGap-10*time.Millisecond {
				t.Errorf("write %d came %v after the one before, want at least %v", i, gap, blindGap)
			}
		}
	}
	if e.writes[len(e.writes)-1] != "\r" {
		t.Errorf("last write %q, want Enter on its own", e.writes[len(e.writes)-1])
	}
}

// Lower-casing the line to match the screen changed its length for some
// characters (KELVIN SIGN lowercases to k, one byte for three), putting the
// echo count out of step with the bytes sent. The line is matched exactly.
func TestUnicodeLineMatchedExactly(t *testing.T) {
	t.Parallel()
	line := "echo KKK İstanbul K"
	e := newEchoRig()
	var echoed int
	p := &Pacer{Baud: 115200, Prompt: "root@h:~ # ", Rx: e.rx, Write: func(b []byte) error {
		_ = e.write(b)
		sent := 0
		for _, w := range e.writes {
			sent += len(strings.TrimSuffix(w, "\r"))
		}
		if ahead := sent - echoed; ahead > window {
			t.Errorf("%d bytes typed ahead of the echo, want at most %d", ahead, window)
		}
		echoed += len(strings.TrimSuffix(string(b), "\r"))
		e.rx <- b
		return nil
	}}
	if err := p.Type(context.Background(), line+"\r"); err != nil {
		t.Fatalf("Type: %v", err)
	}
	if got := strings.TrimSuffix(strings.Join(e.writes, ""), "\r"); got != line {
		t.Errorf("typed %q, want %q", got, line)
	}
}

// An echo in other letters than were typed is not the line.
func TestEchoInOtherCaseIsAMismatch(t *testing.T) {
	t.Parallel()
	e := newEchoRig()
	p := &Pacer{Baud: 115200, Prompt: "root@h:~ # ", Rx: e.rx, Write: func(b []byte) error {
		_ = e.write(b)
		e.rx <- []byte(strings.ToUpper(string(b)))
		return nil
	}}
	err := p.Type(context.Background(), "ls -l\r")
	var se *StopError
	if !errors.As(err, &se) || !se.Unentered || e.entered {
		t.Fatalf("err = %v entered %v, want a mismatch stop before Enter", err, e.entered)
	}
}

// An empty cursor line after the prompt came back means the prompt isn't
// there any more: the pacer must keep waiting rather than call it ready.
func TestEmptyCursorClearsAtPrompt(t *testing.T) {
	t.Parallel()
	r := &run{Pacer: &Pacer{}}
	r.learn("root@h:~ # ")
	ls := &lineSend{entered: true, newLine: true, atPrompt: true}
	r.screen.Feed([]byte("\r\n"))
	if err := r.recv(nil, ls); err != nil {
		t.Fatal(err)
	}
	if ls.atPrompt {
		t.Error("atPrompt still set with an empty cursor line")
	}
}
