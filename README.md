<h1 align="center"><img src="docs/logo.svg" alt="oob" width="420"></h1>

A BMC (baseboard management controller) is the small always-on computer inside a server that
handles power, console and sensors even when the OS is down. `oob` gives your servers' BMCs
(iDRAC, iLO, IPMI) short names. Save each one's IP and login once,
then type `oob node01` to SSH in, or `oob power node01` and `oob sol node01` for power and console.

## Install

With [Homebrew](https://brew.sh) on a Mac or Linux:

    brew install ssimpson89/tap/oob

`brew upgrade` keeps it up to date. For betas too, install `ssimpson89/tap/oob@beta` instead
(`brew uninstall oob` first; only one can be installed).

Or download the archive for your OS and CPU (macOS or Linux; amd64, arm64 or armv7 on Linux)
from the project's [releases page](https://github.com/ssimpson89/oob/releases), unpack it, and
put `oob` somewhere on your PATH. On Linux there are also `.rpm` and `.deb` packages:
`sudo dnf install ./oob-*.rpm` or `sudo apt install ./oob_*.deb`. A browser download on macOS is
quarantined; allow it in System Settings, Privacy & Security, or run
`xattr -d com.apple.quarantine oob` (Homebrew does this for you).

Or install it with Go:

    go install github.com/ssimpson89/oob/cmd/oob@latest

Or build it from a clone of this repo (needs Go):

    go build -o oob ./cmd/oob

`go install` puts the binary in `$(go env GOPATH)/bin` (usually `~/go/bin`), so make sure that
directory is on your PATH. `oob version` prints the version, commit and build date.

### Optional: ipmitool (only for IPMI hosts)

oob calls the separate `ipmitool` program only for plain IPMI and Supermicro hosts, and for the
`sensors`, `sdr`, `fru` and `ipmi` (raw) commands. You do not need it for iLO consoles (VSP over
SSH), iLO power and boot (Redfish), `oob ssh`, Dell racadm, or the host list commands (`ls`, `add`,
`rm`, `edit`, `import`).

    brew install ipmitool    # macOS
    dnf install ipmitool     # Rocky, RHEL, Fedora
    apt install ipmitool     # Debian, Ubuntu

## Glossary

| Term | Meaning |
| --- | --- |
| BMC | Baseboard management controller, the out-of-band management chip (iLO, iDRAC, Supermicro IPMI) |
| SOL | Serial over LAN, the BMC's remote serial console |
| SEL | System event log kept by the BMC (hardware faults, power events) |
| SDR | Sensor data repository, the BMC's list of sensors and their readings |
| FRU | Field replaceable unit inventory (serial numbers, part numbers) |
| NMI | Non-maskable interrupt, which can make the OS dump core or panic |
| Cipher suite | The IPMI-over-LAN authentication and encryption set (`-C`); some BMCs only accept one, such as 17 or 3 |

## Quick start

    oob add node01 192.0.2.11 --user admin     # asks for the password
    oob ls                                     # see what is saved
    oob node01                                 # SSH to the BMC
    oob power node01                           # is it on?
    oob sol node01                             # serial console, ctrl+] then . to leave

## Hosts file

Hosts live in `~/.config/oob/hosts.json` (or `$XDG_CONFIG_HOME/oob/hosts.json` when that is set).
Use `oob edit` to open it (`$VISUAL`, then `$EDITOR`, then vi), or point at another file with
`--config` or `OOB_CONFIG`. A file given with `--config` must exist, except for `add` and `import`,
which create it. Settings in `defaults` apply
to every host unless the host sets its own. Passwords are stored in plain text, so keep the file
private (oob creates it readable only by you and warns if that changes).

    {
      "defaults": { "user": "admin", "pass": "CHANGEME", "cipher": "17" },
      "hosts": {
        "node01": { "ip": "192.0.2.11", "tags": ["rack1", "compute"] },
        "node02": { "ip": "192.0.2.12", "user": "root", "cipher": "3", "notes": "old Dell" },
        "stor1":  { "ip": "192.0.2.21", "port": 2222, "tags": ["storage"] }
      }
    }

You can type just the start of a name if it is unique (`oob node01` can be `oob node0` if nothing
else matches). Prefix matching applies to commands that act on a host; `oob rm` needs the exact
name, and `--yes` on a destructive command also needs the exact name. `port` is the SSH port. `cipher` is the IPMI cipher suite some BMCs insist on, and `type` selects the vendor driver (see Vendor support). `key` is an SSH private key file (see Using an SSH key).

## Using an SSH key

SSH logins (`oob ssh`, the iLO, iDRAC and Supermicro CLI consoles, the iLO CLI fallback, racadm
over SSH) can use a key instead of a password. oob picks the method like this:

1. If the host (or `defaults`) has a `key` file, oob offers only that key, then the password if
   one is set. The ssh-agent is not used. Set it with `oob add <name> --key ~/.ssh/id_ed25519`
   (`~` is expanded; `--key ""` clears it).
2. Otherwise, if `SSH_AUTH_SOCK` points at a reachable ssh-agent, oob offers the agent's keys,
   then the password if one is set. Set `OOB_NO_AGENT=1` to skip the agent.
3. Otherwise it uses the password (password and keyboard-interactive).

oob refuses a key file that group or others can read (`chmod 600` it), like OpenSSH. If the key
has a passphrase, oob asks for it once on the terminal (no echo); without a terminal, load the
key into ssh-agent instead and drop the `key` setting. Key material is never logged.

BMCs often limit failed logins per session and may lock the account after repeated failures
(the exact limits vary by vendor and firmware and haven't been verified).
Every key the agent offers counts as an attempt, so an agent holding many keys can use them up
or lock the account before the password is tried. If your agent holds more than a few keys,
set `key` for the host so only that key is offered, or use `OOB_NO_AGENT=1`.

The BMC user needs the matching public key registered. How depends on the vendor, and none of
these steps have been verified by oob: iLO takes an SSH key on the user in its web interface
[unverified]; iDRAC has `racadm sshpkauth` [unverified]; Supermicro varies by model and firmware
[unverified]. Check the vendor's documentation for the exact steps.

IPMI and Redfish do not use SSH keys. `oob power`, `sel`, and anything that goes over IPMI or
Redfish still need the BMC password, even when you use an SSH key. On iLO, a host with a key and
no password uses the SSH CLI instead of Redfish.

## Commands

| Command | What it does |
| --- | --- |
| `oob <name>` or `oob ssh <name> [cmd...]` | SSH to the BMC, optionally run one command |
| `oob sol <name>` | Serial console with a bottom bar (`--force` clears a stuck session, `--raw` skips the bar) |
| `oob power <name> [status\|on\|off\|cycle\|reset\|soft]` | Power control, default is status |
| `oob boot <name> <pxe\|disk\|bios>` | Set next boot device (UEFI by default; `--legacy`, `--persistent`) |
| `oob sel <name> [list\|clear]` | System event log |
| `oob sensors <name>`, `sdr`, `fru`, `info` | Sensor readings and hardware info |
| `oob uid <name> [on\|off]` | Identify (UID) LED. Reading the state needs an iLO or iDRAC host |
| `oob nmi <name>` | Send an NMI (asks first; iLO `nmi server`, iDRAC `serveraction nmi`, else `chassis power diag`) |
| `oob mcreset <name> [warm\|cold]` | Reset the BMC |
| `oob web <name>` | Open the BMC web page |
| `oob status [selector]` | Power status of many hosts at once (glob, text, or `tag:rack1`) |
| `oob ls [filter]`, `add`, `rm`, `edit` | Manage the host list. `ls` and `status` filters are a glob (`web*`), a substring, or `tag:x`, all case-insensitive |
| `oob add <name> [ip]` | New host, or on an existing host change only the flags you pass. `--pass-stdin` reads the password from stdin |
| `oob import file.csv` | Bulk add from CSV: `name,ip,user,pass,port,cipher,notes,tags` |
| `oob mcp` | Serve your hosts and their consoles to an AI agent over MCP, see [Use it with an AI agent](#use-it-with-an-ai-agent-mcp) |
| `oob version` | Print the version |
| `oob ipmi <name> -- <args>` | Run any other ipmitool command |

CSV tags are separated by `;` (for example `rack1;compute`). A header row is not needed but is
tolerated (skipped when the first cell is `name`), as are a UTF-8 BOM and blank lines. The import is
all-or-nothing: if any row is bad, oob lists every problem with its row number and imports nothing.
Existing hosts are skipped unless you pass `--overwrite`.

Anything that powers off, resets or clears asks first. Add `-y` to skip the prompt. Run
`oob <command> --help` for details, and `oob completion zsh` (or bash, fish) for tab completion
of host names.

## Vendor support

oob picks a driver from the host's `type` (see `oob add --type`). If you leave it empty, oob saves
the type the first time an SSH session shows an iLO, iDRAC or Supermicro banner or prompt. Set
`"type": "ipmi"` to pin a host to plain IPMI and turn detection off.

oob detects the vendor over SSH the first time you use a host with no type: `sol`, `power`, `boot`,
`uid`, `nmi`, `sel`, `mcreset` and `status` log in once (same keys, password and host key pinning as
`oob ssh`), read the banner and the first prompt, and save the type. Set `--type` to skip this, or
`--type ipmi` to force generic IPMI. If SSH is unreachable or nothing is recognized, nothing is
saved and the command goes on as generic IPMI (the next run tries again). `OOB_NO_PROBE=1` turns the
probe off.

| Vendor | `type` | Status |
| --- | --- | --- |
| HPE iLO 7 | `ilo` | Tested: SSH, the VSP console, power status. Redfish write operations (power on/off/reset, PXE boot) are written to the HPE spec but untested |
| Dell iDRAC | `idrac` | Untested. Written from Dell's racadm documentation |
| Supermicro | `supermicro` | Untested. ipmitool with cipher fallback, plus an SSH console fallback |
| Generic IPMI | empty or `ipmi` | ipmitool over lanplus |

When a Dell or Supermicro host changes state, oob prints `note: <vendor> support is untested; please
report results` once on stderr.

## HPE iLO

For `"type": "ilo"` oob uses Redfish (HTTPS, basic auth, 15 second timeout) for power and one-time
PXE boot, and the iLO CLI over SSH for everything else. iLO 7 does not run SSH exec requests, only
an interactive shell, and IPMI over LAN is off by default, so ipmitool is not used for these.

    oob add ilo01 192.0.2.30 --user Administrator --type ilo

Redfish certificates on a BMC are self-signed, so oob does not verify them against a CA. It pins
the certificate key on first use in `redfish_pins` (next to `known_hosts`) and refuses a different
key later; `oob ssh <name> --reset-hostkey` clears the pin after a BMC reimage. The first contact is
trusted, so make it on a management network you trust. If Redfish is
unreachable (connection refused) or rejects the login (401, 403, 404), oob says so on stderr and
falls back to the SSH CLI. oob also saves the iLO generation (`ilo_gen`) when the banner shows it.

| Command | On an iLO host |
| --- | --- |
| `oob sol <name>` | Starts the virtual serial port (`VSP`) with the console bar (kind `vsp`). `~.` or `ctrl+] .` sends `ESC (` then `exit`. `s` types `stty` as with SOL. VSP has no BREAK, so `b` and `r` show a note |
| `oob power <name> [status\|on\|off\|cycle\|reset\|soft]` | Redfish: status reads `PowerState`; on is `On`, off is `ForceOff`, cycle and reset are `ForceRestart`, soft is `GracefulShutdown`. SSH CLI fallback: `power`, `power on`, `power off`, and cycle and reset as off, wait, on. `soft` has no CLI fallback |
| `oob boot <name> pxe` | Redfish one-time PXE override, UEFI (`--legacy` for Legacy). CLI fallback: `onetimeboot netdev1` (no `--legacy`). `--persistent` and other devices are refused |
| `oob uid <name> [on\|off]` | `uid`, `uid on`, `uid off` over the CLI |
| `oob nmi <name>` | `nmi server`, after confirmation |

On a console, `ctrl+] p` sends a Redfish `ForceRestart` when Redfish works. Otherwise it opens a
second SSH connection and runs `power off`, waits until it reports off, then `power on`. Other
commands (`sel`, `sensors`, `fru`, `info`) still use IPMI and need IPMI over LAN.

If ipmitool cannot open a session, oob prints a hint (IPMI over LAN may be disabled; on an iLO it is
under iLO Settings > Access > IPMI/DCMI over LAN, off by default on iLO 7). oob passes `-N 2 -R 2` so
a dead BMC fails in about 11 seconds instead of 21.

## Dell iDRAC (untested)

`"type": "idrac"`. iDRAC runs SSH exec, so oob runs `racadm` commands over SSH. Any output line
starting with `ERROR` is shown as a failure.

| Command | racadm |
| --- | --- |
| `oob power <name> ...` | `serveraction powerstatus`, `powerup`, `powerdown`, `powercycle`, `hardreset`, `graceshutdown` |
| `oob boot <name> pxe` | `set iDRAC.ServerBoot.FirstBootDevice PXE`, then `set iDRAC.ServerBoot.BootOnce Enabled` (skipped with `--persistent`). Applies on the next boot. `--legacy` is refused |
| `oob uid <name> [on\|off]` | `setled -l 1` or `0`; `getled` to read |
| `oob nmi <name>` | `serveraction nmi` |
| `oob sel <name> [list\|clear]` | `getsel`, `clrsel` |
| `oob sol <name>` | Interactive SSH, types `console com2` (bar kind `com2`). Disconnect sends `Ctrl+\` then `exit`. No BREAK |

## Supermicro (untested)

`"type": "supermicro"`. ipmitool is the main path. With no `cipher` set oob tries `-C 17`, and if
that fails with a cipher or RMCP+ error it retries once with `-C 3`; if that works it saves the
cipher on the host and says so. `oob sol` always runs `sol deactivate` first. `oob boot` is
one-time only (`--persistent` is refused because the BMC reverts it). `oob sol --ssh <name>` uses the
BMC's SMASH-CLP shell (`cd /system1/sol1`, `start`); its escape sequence (`ESC (`) is a guess.

## The console bar

`oob sol` and `oob <name>` (interactive SSH) keep a one-line bar on the bottom row of your terminal.
Your terminal's normal scrollback still works, because oob only reserves the last row (a scroll
region) and never switches to a full-screen mode. If a remote program like BIOS setup or vi resets
the screen, oob puts the bar back. If your terminal is not interactive, there is no bar.

Press `ctrl+]`, then one key. Everything else you type goes to the machine.

| Key | What it does |
| --- | --- |
| `s` | Send the window size (`stty rows R cols C` over SOL, a window change over SSH) |
| `a` | Toggle sending the size whenever you resize (off by default for SOL, since it types into the shell) |
| `l` | Start or stop logging (includes the last 10000 lines you already saw) |
| `b` | Send BREAK |
| `c` | Toggle between the host console and the BMC's own CLI prompt without disconnecting (iLO `ESC (` and `vsp`, iDRAC `ctrl+\` and `console com2`, Supermicro SSH SOL `ESC (` (unverified) and `cd /system1/sol1`, `start`). The bar label shows where you are (`vsp` or `ilo cli`, `com2` or `idrac cli`, `sol` or `smash`). On `oob ssh` to an iLO, iDRAC or Supermicro host you start at the prompt, and `c` starts the console. IPMI SOL has no BMC prompt, so `c` only shows a note. `~.` works from either state |
| `r` then a letter | SysRq: BREAK, then the letter (for example `r` `b` reboots a Linux console) |
| `w` | Toggle raw paste for this session (shown as `[raw paste]` on the bar). Default is paced |
| `x` | Stop a paste in progress (`esc` does the same) |
| `p` | Power cycle with confirmation (`y` to go ahead) |
| `]` | Send `ctrl+]` itself |
| `.` | Disconnect |
| `?` | Show every key |
| `esc` | Back to the console |
| `~.` | At the start of a line (ssh style): disconnect. `~~` sends one `~`; `~` plus anything else is sent as typed. Inside an outer ssh session, type `~~.` because the outer ssh eats the first `~` |

Pasting into a serial console (IPMI SOL, iLO VSP, iDRAC `com2`, Supermicro SOL) is paced. A serial line
can't take a whole paste at once, and over a laggy link the BMC's buffer overflows and drops characters. oob
types each pasted line a few characters ahead of the echo, presses Enter only when the echoed line matches
what it typed, waits for the prompt (or for quiet) before the next line, and stops at the first error message
or question from the device. Tabs become spaces. A paste with no newline is typed and left at the prompt. The bar shows
`pasting 3/12` and, if the paste stops, why. `ctrl+] x` (or `ctrl+] esc`) cancels it. Keystrokes you type
during a paste are dropped with a note on the bar, because a held Enter would run half a pasted line. The `s`
size line goes through the same echo check, so a dropped first byte can't turn `stty` into `tty`. If the remote
doesn't echo (a program with echo off), the paste stops before Enter; `ctrl+] w` switches to raw paste, which
writes the paste in one go as before. Plain `oob ssh` sessions and the BMC's own CLI prompt have flow
control and are never paced. The MCP server's `send_command` types each line the same way.

Logs go to `~/.local/state/oob/logs/<name>-<timestamp>.log` (or under `$XDG_STATE_HOME`), with
escape sequences removed, readable only by you. oob handles `~.` itself, and ipmitool's own escape is remapped so only oob interprets `~`. Use `oob sol --raw <name>` for the plain `ipmitool` behavior.

Parts of the bar are adapted from [bauddy](https://github.com/ssimpson89/bauddy) (MIT), see
[THIRD_PARTY.md](THIRD_PARTY.md).

## Use it with an AI agent (MCP)

`oob mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server on stdin and stdout. It lets an
AI agent such as Claude look at your BMCs and read a server's serial console: "node01 stopped answering,
check its event log and see what's on the console". It uses the same drivers as the command line, so vendor
detection applies.

### Add it

```
claude mcp add oob -- oob mcp                   # this project only, just you (the default: --scope local)
claude mcp add --scope user oob -- oob mcp      # every project, just you
claude mcp remove oob                           # remove it, from whichever scope it is in
```

For another MCP client, run `oob mcp` as a stdio server: command `oob`, args `["mcp"]`. Use the full path to
`oob` if the client doesn't see your `PATH`, and `-c FILE` to point it at another host file. Add `--read-only`
after `oob mcp` and the agent gets only the read-only host tools: no changes, no console.

### What the agent can do

Host tools, which take host names from `list_hosts`:

| Tool | What it does |
| --- | --- |
| `list_hosts` | Name, BMC address, type, tags and notes. Never passwords, keys or key paths. Long lists come in pages (`limit`, `offset`, `next_offset`) |
| `power_status`, `status` | Power state of one host, or of many at once (glob, text or `tag:x`; paged like `list_hosts`) |
| `sel_list`, `sensors`, `bmc_info` | Event log (newest entries when it is long), sensor readings, BMC identity |
| `power`, `boot`, `uid`, `nmi`, `sel_clear`, `mc_reset` | Change state. Always asks you first |

Console tools, for the server's own serial console (its login prompt, GRUB, kernel messages), reached the
same way as `oob sol`: IPMI SOL, iLO VSP over SSH, iDRAC `console com2`:

| Tool | What it does |
| --- | --- |
| `connect` | Open the console and return a session id. `force` clears a stale IPMI SOL session (asks first) |
| `read_output`, `read_history` | What is new since the last read (it can wait for some text), and earlier output (the last 10000 lines, searchable) |
| `send_command` | Run a command and return its output. Stops at a `[y/N]` question or a pager |
| `send_keys` | Text and keys: enter, arrows, ctrl+c, function keys, for answers and for GRUB or BIOS menus |
| `send_break` | Serial BREAK, IPMI SOL only (iLO and iDRAC have none, and say so) |
| `bmc_prompt` | iLO and iDRAC: switch between the server console and the BMC's own command line |
| `disconnect` | Close the session |

Several sessions can be open at once, one per host. They close when the client exits and after 15 idle
minutes (`--idle-timeout`). With `--log`, each session is also saved as a log in
`~/.local/state/oob/logs` (or `--log-dir`), readable only by you, with what the console printed and what the
agent typed. Logs are off by default because a console can show secrets.

### What always asks you first

The agent can't answer for you. Your client shows the exact request and you accept or decline:

- every state change: `power` (all actions, including `on`), `boot`, `uid`, `nmi`, `sel_clear`, `mc_reset`
- `connect` with `force`
- console commands that reboot, power off, wipe, format or repartition, remove packages, cut the network or
  change accounts, and anything that triggers SysRq
- anything typed at a bootloader prompt (GRUB, UEFI shell, U-Boot), or at the BMC's own command line
- `send_break`, and the first keys typed after one (on Linux the next key is a SysRq command)
- an answer to a yes/no question the agent's own command raised, unless the answer is no or ctrl+c

If your client can't ask, oob refuses and tells the agent so. There is no switch to skip this, and
`-y` doesn't apply. State changes take exact host names, never a prefix.

The console check is a safety net, not a guarantee: it reads the command line, so scripts, aliases and
menus in firmware or BIOS setup can get past it. Read what the agent wants to run before you allow it.

### What the agent never sees

Passwords and keys stay inside oob: tools take host names, credentials are looked up from your host file when a
tool runs, and no tool returns them. Anything a BMC or console prints passes through the same sanitizer as
the rest of oob (escape sequences, control and bidi characters removed) and is treated as data, not
instructions. Results are cut to a fixed size, every tool has a time limit, and nothing but the protocol is
written to stdout.

Parts of the console and approval logic are adapted from [bauddy](https://github.com/ssimpson89/bauddy)'s
MCP server (MIT), see [THIRD_PARTY.md](THIRD_PARTY.md).

More background on how it works is in [DESIGN.md](DESIGN.md).
