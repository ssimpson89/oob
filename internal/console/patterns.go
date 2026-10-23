package console

import "regexp"

var (
	// Pager matches a pager's question on the cursor line, such as --More--.
	Pager = regexp.MustCompile(`(?i)-\s*more|\(more|q quit|\(end\)|lines \d+-\d+`)

	// SecretPrompt matches a question for a name or a secret on the cursor
	// line: login, username, password (also "Password (again):", "Verify
	// password:", "[sudo] password for u:"), passphrase ("Enter passphrase for
	// /path/key:"), PIN and secret. What is typed in answer is not logged.
	SecretPrompt = regexp.MustCompile(`(?i)\b(login|user ?name|password|pass ?phrase|pin|secret)\b[^:\n]{0,80}: ?$`)

	// shellPrompt is a conservative shell prompt on an otherwise empty cursor
	// line: user@host text ending in "# " or "$ " (root@node01:~# ,
	// [user@host dir]$ ), or a bare bash-5.1$ . Login and password questions,
	// the BMC's own prompts and bootloaders do not end that way.
	shellPrompt = regexp.MustCompile(`^(?:\[?[\w.\-]+@[\w.\-]+(?::| )[^\n]{0,80}?[#$]|[a-z]*sh-[\d.]+[#$]) $`)
)

// ShellPrompt reports whether line, the cursor's line as the user sees it, is
// an empty shell prompt.
func ShellPrompt(line string) bool {
	return shellPrompt.MatchString(line) && !SecretPrompt.MatchString(line)
}
