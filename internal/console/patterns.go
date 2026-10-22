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
)
