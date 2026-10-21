package main

import (
	"github.com/ssimpson89/oob/internal/app"
)

// AppName is the binary name and config directory name. Change it in internal/app and in go.mod
// (module line) to rename the tool. It is defined in internal/app so the library
// packages can use it too.
const AppName = app.Name

// Set at release time by GoReleaser via -ldflags "-X main.version=..." etc.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func versionString() string {
	return version + " (commit " + commit + ", built " + date + ")"
}
