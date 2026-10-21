// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// The command checks below are adapted from bauddy (internal/paste/paste.go and
// internal/mcpserver/guard.go): the list of commands that reboot a device or
// erase or replace its configuration, the shell-chain and wrapper parsing that
// finds the real command in `sudo -i reboot` or `ls; reboot`, and the prompt,
// typed-text and yes/no tracking that keeps an agent from slipping a risky
// line past the check by splitting it across keystrokes. Server-console
// additions (storage, accounts, network, package removal, SysRq) follow.

package mcpserver

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// dangerous matches commands, as splitCommands returns them, that can reboot a
// device or erase or replace its configuration or software.
var dangerousRE = regexp.MustCompile(`^((reload|write erase|erase|format|delete|factory-?reset|reboot|poweroff|halt|` +
	`mkfs\S*|wipefs|(tel)?init (0|6|--reboot|--poweroff|--halt)|request system (reboot|halt|power-off|zeroize|software add)|` +
	`request vmhost (reboot|halt|power-off)|request (restart|shutdown) system|request platform software package install|` +
	`load (override|factory-default)|configure (replace|factory-default)|crypto key zeroize|` +
	`boot (system|nxos|kickstart)|install (all|add|activate)|redundancy force-switchover|system switchover|` +
	`/?system (reset-configuration|reboot|shutdown)|` +
	`systemctl( -\S+)* ((reboot|poweroff|halt|kexec|soft-reboot|suspend|hibernate|hybrid-sleep)|` +
	`(start|isolate)( -\S+)* (reboot|poweroff|halt|kexec|shutdown|rescue|emergency)(\.target)?)|` +
	`loginctl( -\S+)* (reboot|poweroff|halt|suspend|hibernate)|` +
	`shutdown|rm -(rf|fr)|racadm (serveraction|racresetcfg))\b|` +
	`clear config|copy \S+ (nvram:)?start|execute (factory|reboot|shutdown|restore|formatlogdisk|erase-disk)|` +
	`dd .*of=/dev/|ipmitool .*(chassis power (off|cycle|reset)|mc reset)|` +
	`!(!|-?\d+|\??[\w./-]+)$)`) // shell history recall (!!, !-1, !reb): no telling what it runs

// abbreviated lists dangerous commands as IOS-style CLIs accept them, with
// each word shortened to no less than its minimum ("rel" is reload, "wr er"
// is write erase). "*" stands for any word.
var abbreviated = [][]struct {
	word string
	min  int
}{
	{{"reload", 3}},
	{{"erase", 3}},
	{{"delete", 3}},
	{{"format", 4}},
	{{"write", 2}, {"erase", 2}},
	{{"copy", 3}, {"*", 1}, {"startup-config", 2}},
	{{"configure", 4}, {"replace", 3}},
	{{"boot", 4}, {"system", 3}},
	{{"request", 3}, {"system", 3}, {"reboot", 3}},
	{{"execute", 3}, {"reboot", 3}},
}

var (
	// chain splits shell command chains and substitutions.
	chain = regexp.MustCompile("&&|\\|\\||[;|`]|\\$\\(")
	// assignment sets a shell variable for the command after it.
	assignment = regexp.MustCompile(`^[a-z_][a-z0-9_]*=`)
	// wrappers run the command that follows them: do (IOS, from config
	// mode), run (Junos, VyOS), sudo and the like. Not sh, which is show
	// on Cisco-style CLIs.
	wrappers = map[string]bool{"do": true, "run": true, "sudo": true, "doas": true, "su": true, "exec": true,
		"command": true, "nohup": true, "time": true, "bash": true, "env": true, "eval": true, "xargs": true,
		"busybox": true}
)

// baseName strips quotes and any directory from an executable word.
func baseName(w string) string {
	w = strings.Trim(w, `'"\`)
	if i := strings.LastIndex(w, "/"); i > 0 {
		w = w[i+1:]
	}
	return w
}

// splitCommands splits a line into the commands it runs, in lower case with
// single spaces: a shell chain is split up, and wrappers (sudo -u root),
// quotes (bash -c 'reboot') and paths (/sbin/reboot) are removed.
func splitCommands(line string) []string {
	var cmds []string
	for _, part := range chain.Split(strings.ToLower(line), -1) {
		w := strings.Fields(part)
		for len(w) > 0 {
			if assignment.MatchString(w[0]) { // MODE=x reboot, env A=1 reboot
				w = w[1:]
				continue
			}
			w[0] = baseName(w[0]) // /usr/bin/sudo, /sbin/reboot
			if !wrappers[w[0]] {
				break
			}
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") { // the wrapper's options
				if (w[0] == "-u" || w[0] == "-g") && len(w) > 1 {
					w = w[1:] // and the user or group after it
				}
				w = w[1:]
			}
		}
		if len(w) == 0 {
			continue
		}
		cmds = append(cmds, strings.Trim(strings.Join(w, " "), `'"`))
	}
	return cmds
}

// isDangerous reports whether line runs a command that can reboot a device or
// erase or replace its configuration, spelled out or abbreviated.
func isDangerous(line string) bool {
	for _, cmd := range splitCommands(line) {
		if dangerousCommand(cmd) {
			return true
		}
	}
	return false
}

func dangerousCommand(cmd string) bool {
	words := strings.Fields(cmd)
	if len(words) == 2 && words[1] == "cancel" && abbreviates(words[0], "reload", 3) {
		return false // undoes "reload in 10"
	}
	if dangerousRE.MatchString(cmd) {
		return true
	}
next:
	for _, c := range abbreviated {
		if len(words) < len(c) {
			continue
		}
		for i, w := range c {
			if w.word != "*" && !abbreviates(words[i], w.word, w.min) {
				continue next
			}
		}
		return true
	}
	return false
}

// abbreviates reports whether typed is word, shortened to no less than min.
func abbreviates(typed, word string, min int) bool {
	return len(typed) >= min && strings.HasPrefix(word, typed)
}

// Why something needs the user's approval, completing "It ...".
const (
	whyRestart = "can restart or power off the host, or erase or replace its configuration or software"
	whyStorage = "can destroy or repartition disks, volumes and arrays"
	whyAccount = "changes account credentials or access"
	whyNetwork = "can cut the host off the network"
	whyPackage = "removes software from the host"
	whySysrq   = "triggers a kernel SysRq action, which can crash, reboot or sync-and-halt the host"
	whyAnswer  = "answers the host's question (a yes/no prompt that no approved command raised)"
	whyBoot    = "is typed at a bootloader or firmware prompt, where commands can boot, erase or replace the software"
	whyRecall  = "runs a line edited with cursor or editing keys, recalled from history or completed with tab, which can't be read for certain"
	whyBreak   = "sends a serial BREAK: on Linux the next key typed is a SysRq command, and b reboots at once"
	whyAfter   = "is typed right after a BREAK, where the Linux kernel takes the next key as a SysRq command"
	whyFirmKey = "is a key that commonly boots, saves or resets in a bootloader or firmware menu"
	whyBMC     = "is typed at the BMC's own command line, where commands can power off or reset the server or the BMC"
	whyEscape  = "leaves the host console for the BMC's own command line"
	whyForce   = "ends another SOL session on this host"
)

// extras are risks checked on top of dangerousRE, each with its reason. They
// match commands as splitCommands returns them (lower case, single spaces).
var extras = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`^(rm( -\S+)*( -[a-z]*r[a-z]*| --recursive)\b|shred\b|blkdiscard\b|truncate\b|mkswap\b|` +
		`(sgdisk|sfdisk|cfdisk|gdisk|fdisk|parted)\b|(pv|vg|lv)(remove|reduce|resize|extend)\b|` +
		`mdadm( \S+)* (--stop|--zero-superblock|--remove|--fail|--create|-[sf])\b|` +
		`zpool (destroy|labelclear|detach|remove)\b|zfs destroy\b|` +
		`cryptsetup .*(luksformat|luksremovekey|erase|luksdump --dump-master-key)|nvme (format|sanitize|fw-)|` +
		`(ss|hpss)acli .*(delete|erase|modify)|(stor|perc)cli.* (del|delete|clear)\b)`), whyStorage},
	{regexp.MustCompile(`^(?:(?:echo|printf|tee)\b.*sysrq-trigger( |$)|.*>+ ?/proc/sysrq-trigger( |$)|kexec\b|` +
		`systemctl( -\S+)* (isolate|rescue|emergency)\b|` +
		`grub2?-(install|mkconfig|set-default)\b|efibootmgr .*(-b\b|--delete)|update-grub\b|dracut\b|` +
		`flashrom\b|fwupdmgr (install|update|downgrade)\b|ilorest\b)`), whyRestart},
	{regexp.MustCompile(`^(passwd\b|chpasswd\b|usermod .*-[pl]\b|userdel\b|visudo\b|chage\b)`), whyAccount},
	{regexp.MustCompile(`^(ifdown\b|ip (link|addr|address|route) (set .* down|del|delete|flush)\b|ifconfig \S+ down\b|` +
		`nmcli (c|con|conn|connection|d|dev|device) (down|delete|disconnect)\b|` +
		`iptables( -\S+)* -[fxp]\b|ip6tables( -\S+)* -[fxp]\b|nft flush\b|firewall-cmd .*--panic-on|` +
		`systemctl( -\S+)* (stop|disable|mask|restart) (sshd?|networkmanager|network|firewalld|systemd-networkd)\b)`), whyNetwork},
	{regexp.MustCompile(`^((dnf|yum|microdnf|zypper|apt|apt-get|aptitude) .*(remove|erase|purge|autoremove|downgrade)\b|rpm( \S+)* (-e|--erase)\b|dpkg( \S+)* (-r|-p|--remove|--purge)\b)`), whyPackage},
}

// listOnly matches partitioning tools asked only to show what is there. It
// lists the exact read-only forms and nothing else: the whole command has to
// match, so a write flag or subcommand anywhere (sgdisk --zap-all --print,
// parted -s /dev/sda rm 1 print) leaves it needing approval.
var listOnly = regexp.MustCompile(`^(` +
	`fdisk (-l|--list)( /dev/[\w./-]+)*|` +
	`sfdisk (-l|--list)( /dev/[\w./-]+)*|` +
	`gdisk -l /dev/[\w./-]+|` +
	`sgdisk (-p|--print) /dev/[\w./-]+|` +
	`parted (-l|--list)|` +
	`parted( (-s|--script))?( /dev/[\w./-]+)?( unit [a-z]+)? print( (free|all|/dev/[\w./-]+))?` +
	`)$`)

// risk says why line needs approval, or "" if it doesn't.
func risk(line string) string {
	for _, cmd := range splitCommands(line) {
		if listOnly.MatchString(cmd) {
			continue
		}
		if dangerousCommand(cmd) {
			return whyRestart
		}
		for _, e := range extras {
			if e.re.MatchString(cmd) {
				return e.why
			}
		}
	}
	return ""
}

var (
	// bootloader matches a bootloader or firmware prompt: GRUB, the UEFI
	// shell, U-Boot, a loader.
	bootloader = regexp.MustCompile(`(?i)^(grub( rescue)?>|(uefi )?shell>|=>|u-boot>|loader>|ok|rommon ?\d* ?>) ?$`)
	// promptLike matches a cursor line that looks like a prompt with nothing
	// typed after it.
	promptLike = regexp.MustCompile(`[#>$%] ?$`)
	// confirming matches the cursor line when the host wants a yes or no
	// before doing something.
	confirming = regexp.MustCompile(`(?i)\[confirm\]|\[y(es)?/n(o)?\]|\(y(es)?/n(o)?\)|\[yes,\s*no\]|` +
		`are you sure|proceed\s*\?|continue\s*\?|is this ok|do you want to`)
	// promptEnd finds where a prompt may end and typing begin.
	promptEnd = regexp.MustCompile(`[#>$%]`)
	// paging matches a pager's question, such as --More--.
	paging = regexp.MustCompile(`(?i)-\s*more|\(more|q quit|\(end\)|lines \d+-\d+`)
	// loginPrompt matches a login or password question on the cursor line.
	loginPrompt = regexp.MustCompile(`(?i)(login|username|password)( for \S+)?: ?$`)
)

// lineRisk is risk for the host's cursor line, prompt and all. The prompt
// can't be told apart from what was typed for certain, so every place a
// prompt could end is tried.
func lineRisk(line string) string {
	if why := risk(line); why != "" {
		return why
	}
	for _, loc := range promptEnd.FindAllStringIndex(line, -1) {
		if why := risk(line[loc[1]:]); why != "" {
			return why
		}
	}
	return ""
}

// looksLikePrompt reports whether the cursor line looks like a prompt with
// nothing typed after it.
func looksLikePrompt(cursor string) bool {
	return promptLike.MatchString(cursor) || bootloader.MatchString(cursor)
}

// line is where the cursor is: the line it's on, the last prompt seen, and
// how many prompts have appeared so far.
type line struct {
	cursor, prompt string
	prompts        int
}

// typed returns what's typed after the prompt on the cursor line: after the
// last prompt seen if the line starts with it, else after the line's last
// # > $ or %.
func (l line) typed() string {
	if l.prompt != "" && strings.HasPrefix(l.cursor, l.prompt) {
		return strings.TrimSpace(l.cursor[len(l.prompt):])
	}
	if i := strings.LastIndexAny(l.cursor, "#>$%"); i >= 0 {
		return strings.TrimSpace(l.cursor[i+1:])
	}
	return ""
}

// atBootloader reports whether the cursor is at a bootloader prompt, with
// or without something typed after it.
func (l line) atBootloader() bool {
	if bootloader.MatchString(l.cursor) {
		return true
	}
	return l.prompt != "" && strings.HasPrefix(l.cursor, l.prompt) && bootloader.MatchString(l.prompt)
}

// confirming reports whether the host is asking for a yes or no.
func (l line) confirming() bool { return confirming.MatchString(l.cursor) }

// typing is what send_keys has typed since Enter was last pressed, so a
// line can be checked even before the host echoes it.
type typing struct {
	text    string
	unknown bool // a key changed the line in ways the server can't follow
	at      int  // the session's prompt count when typing began
}

// add records sending text and then key. prompts is the session's prompt
// count: a prompt since typing began means the host took the line.
func (t *typing) add(text, key string, prompts int) {
	if t.at != prompts {
		*t = typing{at: prompts}
	}
	t.text += text
	switch key {
	case "space":
		t.text += " "
	case "backspace":
		if _, n := utf8.DecodeLastRuneInString(t.text); n > 0 {
			t.text = t.text[:len(t.text)-n]
		}
	case "enter", "ctrl+c", "ctrl+z":
		*t = typing{at: prompts}
	case "ctrl+u":
		// Readline's ctrl+u deletes only what is before the cursor. It clears the
		// line only when the cursor is known to be at the end, so an unknown
		// line (after cursor movement or an unmodelled edit) stays unknown.
		if !t.unknown {
			*t = typing{at: prompts}
		}
	case "ctrl+d":
		// On an empty line it is EOF (a logout or a shell exit); on a line with
		// text Readline deletes the character under the cursor, which edits the
		// line in a way the server doesn't follow.
		if t.text == "" && !t.unknown {
			*t = typing{at: prompts}
		} else {
			t.unknown = true
		}
	case "":
	default:
		// Any other key (tab, up, down, home, end, delete, left, right, esc,
		// ctrl+a/e/k/w/y/p/n/r/h/...) can move, change or replace the line, which
		// the server doesn't follow. The line stays unknown until it is cleared
		// (ctrl+c, enter) or a new prompt appears.
		if !paintKeys[key] {
			t.unknown = true
		}
	}
}

// paintKeys are keys that don't edit a command line: function keys and paging
// keys, which menus use.
var paintKeys = map[string]bool{
	"f1": true, "f2": true, "f3": true, "f4": true, "f5": true, "f6": true, "f7": true, "f8": true,
	"f9": true, "f10": true, "f11": true, "f12": true, "pageup": true, "pagedown": true, "insert": true,
}

// pending returns what's typed and not yet entered, as of prompt count
// prompts, and whether an editing key changed it.
func (t typing) pending(prompts int) (string, bool) {
	if t.at != prompts {
		return "", false
	}
	return t.text, t.unknown
}

// safeAnswer reports whether sending text and key at a yes-or-no question
// can only decline or stop it.
func safeAnswer(text, key string) bool {
	if no := strings.EqualFold(text, "n") || strings.EqualFold(text, "no"); no {
		return key == "" || key == "enter"
	}
	switch key {
	case "ctrl+c", "ctrl+z", "esc", "ctrl+u", "backspace":
		return text == ""
	}
	return false
}
