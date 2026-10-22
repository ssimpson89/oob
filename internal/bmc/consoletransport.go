package bmc

import "github.com/ssimpson89/oob/internal/console"

// ConsoleTransport is a running serial console connection with no terminal
// attached. It is console.Transport; the alias keeps the drivers' name for it.
type ConsoleTransport = console.Transport

// OpenConsoleOptions are the inputs of Driver.OpenConsole.
type OpenConsoleOptions struct {
	// Rows and Cols are the window size the remote is told.
	Rows, Cols int
	// Force clears a stale IPMI SOL session first (the same as sol --force).
	Force bool
}
