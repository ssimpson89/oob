package mcpserver

import "testing"

func TestRiskFlagsRebootAndWipeLikeCommands(t *testing.T) {
	for _, l := range []string{
		"reboot", "sudo reboot", "sudo -i reboot", "/sbin/reboot", "bash -c 'reboot'", "ls; reboot", "true && reboot",
		"echo `reboot`", "systemctl reboot", "systemctl --force poweroff", "shutdown -h now", "shutdown now", "shutdown -r +5",
		"poweroff", "halt", "init 0", "telinit 6", "MODE=x reboot", "env A=1 reboot", "xargs reboot", "'reboot'", `\reboot`,
		"mkfs.ext4 /dev/sda1", "wipefs -a /dev/sda", "dd if=/dev/zero of=/dev/sda", "rm -rf /", "rm -r /var/lib/x", "rm -Rf /x",
		"rm --recursive /x", "shred /dev/sda", "blkdiscard /dev/nvme0n1", "fdisk /dev/sda", "parted /dev/sda rm 1", "sgdisk -Z /dev/sda",
		"lvremove vg/lv", "vgremove vg", "mdadm --stop /dev/md0", "mdadm --zero-superblock /dev/sdb", "zpool destroy tank",
		"nvme format /dev/nvme0n1", "echo b > /proc/sysrq-trigger", "echo c >/proc/sysrq-trigger", "kexec -e",
		"passwd root", "usermod -L bob", "ifdown eth0", "ip link set eth0 down", "nmcli connection down x", "iptables -F",
		"systemctl stop sshd", "dnf remove openssh", "yum erase kernel", "rpm -e foo", "apt-get purge x",
		"ipmitool chassis power cycle", "ipmitool mc reset cold", "racadm serveraction powerdown",
		"!!", "!reb", "sudo !!", "factory-reset", "efibootmgr -B -b 0001", "grub2-install /dev/sda",
	} {
		if risk(l) == "" {
			t.Errorf("%q not flagged", l)
		}
	}
}

func TestRiskLeavesReadOnlyCommandsAlone(t *testing.T) {
	for _, l := range []string{
		"ls /", "cat /etc/os-release", "dmesg | tail -50", "journalctl -b -p err", "uname -r", "df -h", "lsblk", "ip a", "ip route",
		"systemctl status sshd", "systemctl is-active network", "rpm -qa | grep kernel", "dnf list installed", "dnf check-update",
		"echo hello!", "echo reboot is a word", "ls /system/reboot", "grep reboot /var/log/messages", "mdadm --detail /dev/md0",
		"zpool status", "nvme list", "fdisk -l", "ssacli ctrl all show", "cat /proc/sysrq-trigger-notes",
	} {
		if why := risk(l); why != "" {
			t.Errorf("%q flagged (%s)", l, why)
		}
	}
}

func TestLineRiskSeesTypedTextAfterPrompt(t *testing.T) {
	for _, l := range []string{"[root@node01 ~]# reboot", "node01:~$ sudo poweroff", "# shutdown -h now"} {
		if lineRisk(l) == "" {
			t.Errorf("%q not flagged", l)
		}
	}
	if lineRisk("[root@node01 ~]# ls") != "" {
		t.Error("ls flagged")
	}
}

func TestPromptHelpers(t *testing.T) {
	if !looksLikePrompt("[root@node01 ~]# ") || !looksLikePrompt("grub> ") || looksLikePrompt("Loading kernel") {
		t.Error("looksLikePrompt wrong")
	}
	l := line{cursor: "[root@node01 ~]# reb", prompt: "[root@node01 ~]# "}
	if l.typed() != "reb" {
		t.Errorf("typed = %q", l.typed())
	}
	if !(line{cursor: "grub> "}).atBootloader() || (line{cursor: "# "}).atBootloader() {
		t.Error("atBootloader wrong")
	}
	if !(line{cursor: "Is this ok [y/N]: "}).confirming() {
		t.Error("confirming missed a dnf question")
	}
	if !safeAnswer("n", "enter") || safeAnswer("y", "enter") || !safeAnswer("", "ctrl+c") || safeAnswer("", "enter") {
		t.Error("safeAnswer wrong")
	}
	var ty typing
	ty.add("reb", "", 1)
	ty.add("oot", "", 1)
	if p, _ := ty.pending(1); p != "reboot" {
		t.Errorf("pending = %q", p)
	}
	ty.add("", "ctrl+u", 1)
	if p, _ := ty.pending(1); p != "" {
		t.Errorf("pending after ctrl+u = %q", p)
	}
	ty.add("", "up", 1)
	if _, unknown := ty.pending(1); !unknown {
		t.Error("up should make the line unknown")
	}
	if p, u := ty.pending(2); p != "" || u {
		t.Error("typing should reset at a new prompt")
	}
}
