# Contributing

```
make build && ./bin/oob
make test        # go test -race ./...
make lint        # go vet ./...
make snapshot    # a local release build, see RELEASING.md
```

It needs Go 1.26 or later. The build is pure Go (no cgo); macOS and Linux only.

## Layout

```
cmd/oob/            entry point, cobra commands, flags, notices, version
internal/app/       app name, exit-code error, hooks for notes and progress
internal/config/    the hosts file: load, merge, locked atomic updates
internal/console/   bottom bar, key menu, session loop, logs, output sanitizing
internal/detect/    vendor detection from SSH banners and prompts
internal/sshx/      SSH connect, host keys, auth
internal/bmc/       drivers: IPMI, Redfish, iLO, iDRAC, Supermicro, vendor SSH CLI console
```

Imports run one way and nothing imports `cmd/oob`; library packages report
notes and progress through the hooks in `internal/app`. See DESIGN.md.

Tests use `node01` and `192.0.2.x` addresses, never real hosts or captured
device output.
