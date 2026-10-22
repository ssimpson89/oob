package console

import "testing"

func TestSecretPrompt(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"node01 login: ", true},
		{"login:", true},
		{"Username: ", true},
		{"Password: ", true},
		{"[sudo] password for admin: ", true},
		{"Password (again): ", true},
		{"Verify password: ", true},
		{"Retype new password:", true},
		{"Enter passphrase for /home/u/.ssh/id_ed25519: ", true},
		{"Enter passphrase for key '/home/u/.ssh/id_rsa': ", true},
		{"Enter PIN: ", true},
		{"Enter secret:", true},
		{"Last login: Tue Oct 22 10:00:00 2026 from 192.0.2.5", false},
		{"[root@node01 ~]# ", false},
		{"pinging 192.0.2.1: ", false},
		{"Password: hunter2", false}, // already answered
		{"", false},
	} {
		if got := SecretPrompt.MatchString(tc.line); got != tc.want {
			t.Errorf("SecretPrompt(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestPager(t *testing.T) {
	for line, want := range map[string]bool{
		"--More--": true, "-- more --": true, "(END)": true, "lines 1-23/80": true,
		"[root@node01 ~]# ": false,
	} {
		if got := Pager.MatchString(line); got != want {
			t.Errorf("Pager(%q) = %v, want %v", line, got, want)
		}
	}
}
