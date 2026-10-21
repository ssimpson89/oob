package main

// AppName is the binary name and config directory name. Change it here and in go.mod
// (module line) to rename the tool.
const AppName = "oob"

// Set at release time by GoReleaser via -ldflags "-X main.version=..." etc.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func versionString() string {
	return version + " (commit " + commit + ", built " + date + ")"
}
