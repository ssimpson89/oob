# oob design review

Name `oob` lives in one constant (`app.Name` in `internal/app`, re-exported as `AppName` in
`cmd/oob/name.go`) plus `go.mod`.
Env var prefix, config dir and error prefixes derive from it.

## IPMI expert
Most used tasks, in order: SOL console, power status/on/off/cycle/reset, chassis bootdev
(pxe/disk/bios), SEL list/clear, sensors, mc info/reset, fru. Shell out to `ipmitool`
(`-I lanplus`); do not reimplement IPMI.
- Password goes through `-E` and the `IPMI_PASSWORD` env var, never argv (argv is visible in `ps`).
- SOL: escape is `~.` at line start; stale sessions need `sol deactivate` first, so
  `oob sol --force` deactivates (ignoring errors) then activates.
- Many BMCs only work with a specific lanplus cipher suite, so each host has an optional
  `cipher` (passed as `-C`; commonly 17 or 3).
- Destructive actions (power off/cycle/reset/soft, sel clear, mc reset, nmi, risky `ipmi` passthrough) ask for
  confirmation unless `--yes`, and `--yes` needs the exact host name, not a prefix. One table
  (`destructiveActions`, cmd/oob/cmd_bmc.go) drives both checks.
- Bulk power status runs concurrently with a bounded worker pool and a per-host timeout.

## Linux console expert
- SSH to BMCs with password auth. No sshpass dependency (not on macOS): use
  `golang.org/x/crypto/ssh` with a real PTY, `x/term` raw mode, initial window size,
  SIGWINCH forwarding, and terminal restore before exit.
- Auth: password, then keyboard-interactive (many BMCs only offer the latter).
- Host keys: own `known_hosts` in the config dir, accept-new semantics. A changed key is
  refused with a hint; `ssh --reset-hostkey` removes the entry (and the host's Redfish TLS pin),
  since BMCs get reimaged. Once a host has stored keys, only those key types are offered, so a
  BMC cannot downgrade the connection to a key type oob has never seen.
- Legacy algorithms (DH group1/14 SHA1, CBC ciphers, ssh-rsa/ssh-dss host keys) are appended
  after the modern defaults so modern servers still negotiate modern algorithms.
- Extra args after the name run a remote command without a PTY (e.g. racadm).

## Linux architect
- Config: `$XDG_CONFIG_HOME/oob/hosts.json`, else `~/.config/oob/hosts.json` (macOS and Linux alike).
  Override with `--config` or `OOB_CONFIG`.
- File is 0600, directory 0700; writes are atomic (temp file + rename). Loading warns on stderr
  if group/other bits are set.
- Schema: `defaults` block (user, pass, port, cipher) merged under each host; host values win.
  Host fields: ip, user, pass, port (SSH port), cipher, notes, tags.
- Host lookup: exact name, else unique prefix.
- Single static binary (`CGO_ENABLED=0`), darwin and linux, amd64 and arm64.

## Drivers
- `bmc.Driver` (internal/bmc/driver.go) is the per-vendor interface: Power, PowerStatus, Boot, UID, NMI, SEL,
  Console, ConsolePower. Commands validate arguments and confirm; the driver does the work.
  `bmc.DriverFor` picks one from the host `type`; empty and `ipmi` are generic IPMI (ipmitool).
- iLO is Redfish-first (power, one-time PXE boot) and falls back to the iLO CLI over SSH when
  Redfish is unreachable or rejects the login. iLO 7 has no SSH exec, only an interactive shell.
- TLS: BMC certificates are self-signed, so chain verification is skipped and the leaf key is
  pinned on first use (TOFU) in `redfish_pins`, next to `known_hosts`. Later connections must
  match the pin; a mismatch is refused. `ssh --reset-hostkey` clears the pin after a reimage.
- Vendor detection runs after authentication: the first SSH banner or prompt that shows iLO,
  iDRAC or Supermicro saves `type` (and `ilo_gen`) on the host. A banner alone is never saved,
  and nothing is saved before the login worked. `type: ipmi` turns it off.
- Shared SSH CLI plumbing (expect-style session, console link, the vendor `cliSpec`) is in
  internal/bmc/cli.go; ilo.go, idrac.go and supermicro.go there hold each vendor's driver and
  parsing. Notes and progress lines are in cmd/oob/ui.go; library packages reach them through the
  hooks in `internal/app` (`AddNotice`, `Progressf`, `Quiet`) that cmd/oob sets at startup.
- iDRAC (racadm over SSH) and Supermicro (ipmitool with cipher fallback) are untested on real
  hardware; oob prints a one-time note on stderr when they change state.

## Layout
- `cmd/oob`: cobra wiring, flags, notices and the confirmation prompts.
- `internal/app`: the name, exit-code error and the notice hooks. `internal/config`: hosts file.
- `internal/console`: bottom bar, key menu, session loop, log files, text sanitizing.
- `internal/detect`: vendor detection. `internal/sshx`: SSH connect, host keys, auth.
- `internal/bmc`: drivers (IPMI, Redfish, iLO, iDRAC, Supermicro) and the vendor SSH CLI console.
- Imports run one way: bmc -> sshx, detect -> console, config -> app; none import cmd/oob.

## Console scrollback
The bar uses a DECSTBM scroll region on the normal screen (not the alternate screen), so lines
that scroll off the top stay in the terminal's native scrollback. Checked in a plain terminal;
tmux and screen may behave differently.

## Known limits
Passwords are stored in plaintext JSON (0600), as requested; no keychain integration.
`--pass` on the command line leaks to shell history, so `add` prompts when it is omitted.
Config changes (`add`, `rm`, `import`, vendor detection, saved ciphers) go through `UpdateConfig`,
which takes a lock file next to the config and re-reads before writing, so concurrent runs do not
lose each other's updates. `edit` is the exception: the editor owns the file while it is open. `add` and `import` reject host names that equal a command name, because
`oob <name>` shorthand would run the command instead. Only the iLO driver has Redfish; other
drivers need IPMI over LAN or SSH. Shell completion lists names from the config at the time of
the request.
