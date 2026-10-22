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
  Console, OpenConsole, ConsolePower. Commands validate arguments and confirm; the driver does the work.
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
- `internal/typer`: the paced input queue every typed or pasted byte goes through (one goroutine owns the queue).
- `internal/detect`: vendor detection. `internal/sshx`: SSH connect, host keys, auth.
- `internal/bmc`: drivers (IPMI, Redfish, iLO, iDRAC, Supermicro) and the vendor SSH CLI console.
- `internal/mcpserver`: the MCP server (`oob mcp`): host tools, console sessions, approvals.
- Imports run one way: bmc -> sshx, detect -> console, config -> app; none import cmd/oob.

## MCP server
`oob mcp` (internal/mcpserver, built on the same go-sdk version bauddy pins) serves the drivers and consoles
over stdio. Three rules shape it.
- Credentials never cross the tool boundary. Tools take host names; the host is looked up per call
  (`config.LookupExact` for anything that changes state, so no prefix can pick another host) and handed to a
  driver. `hostInfo` has no field for a secret.
- Everything that changes state goes through `server.approve`, MCP elicitation adapted from bauddy: the first
  call returns an input request, the client asks the user and repeats the call, and the answer is honored once,
  for that exact call and host. A client that can't ask is refused; there is no bypass flag. Console input is
  checked by `risk.go` (bauddy's command list plus server additions), and the cursor line, typed-but-not-entered
  text and yes/no questions are tracked so a command split across keystrokes or calls is still caught.
- Remote text is data. `text()` runs every result through `console.CleanRemoteN`, and results are bounded.
Drivers print through `bmc.Stdout()` rather than `os.Stdout`; `bmc.Capture` collects one call's output in a
bounded buffer, gives ipmitool no stdin, and kills it when the tool's deadline passes. Captures run one at a time
because the seam is package-level, so a slow BMC call delays the next one (the bulk `status` does not use it).
`Run` also points `os.Stdout` at stderr so nothing but the protocol can reach the stream.
The console runs headless through `Driver.OpenConsole`, which returns a `bmc.ConsoleTransport` (link, output,
exit, optional BMC-prompt toggle). `oob sol` builds the same transports (`openSOL`, `cliOpen`) and adds the bar
on top. `console.Transcript` keeps the 10000-line history, the read position and the optional log.

## Shared consoles
An iLO allows one VSP session, so the user's window and the agent can't hold separate connections. `internal/share`
(adapted from bauddy's `internal/share`) makes one process own the console: a flock on
`$XDG_STATE_HOME/oob/share/<host>.lock` elects it, and it runs a hub on `<host>.sock`. Every client, the owner's own
included, talks to the hub through `share.Client` (the owner over an in-memory pipe). The hub owns the
`console.Transport` and one paced `typer`, so input from all clients is serialized; output is fanned out. The AI
client takes a lease per tool call and the hub refuses it while the user paused it or is typing, as in bauddy.
Differences from bauddy: no takeover when the owner exits (reopening means a BMC login, a race with the BMC's own
timeout for the old session), so the others are told the console closed; an MCP owner keeps the hub until the last
window leaves; the AI's writes are requests answered once typed, because the typer is slow and the tools wait on it.
`oob sol` and `oob mcp` both go through `share.Open`; a console no one else uses behaves as before.

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
