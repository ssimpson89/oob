package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	cfgFlag string
	yesFlag bool
)

// exitCodeErr carries a child process exit code up to main.
type exitCodeErr int

func (e exitCodeErr) Error() string { return "exit status " + strconv.Itoa(int(e)) }

func main() {
	err := newRoot().Execute()
	flushNotices()
	if err == nil {
		return
	}
	var ec exitCodeErr
	if errors.As(err, &ec) {
		os.Exit(int(ec))
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", AppName, err)
	os.Exit(1)
}

func cfgPath() (string, error) { return ConfigPath(cfgFlag) }

// load reads the config. An explicitly given -c/--config file must exist.
// Permission warnings are only printed once the file parsed, so they never bury a
// parse error.
func load() (string, *Config, error) {
	p, err := cfgPath()
	if err != nil {
		return "", nil, err
	}
	if cfgFlag != "" {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("config file %s does not exist (add or import creates it)", p)
		}
	}
	var warns []string
	c, err := LoadConfig(p, func(m string) { warns = append(warns, m) })
	if err == nil {
		for _, m := range warns {
			fmt.Fprintln(os.Stderr, "warning:", m)
		}
	}
	return p, c, err
}

// loadCreate is load for commands that create the config file when missing
// (add, import).
func loadCreate(w io.Writer) (string, *Config, error) {
	p, err := cfgPath()
	if err != nil {
		return "", nil, err
	}
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(w, "creating %s\n", p)
		return p, &Config{Hosts: map[string]Host{}}, nil
	}
	return load()
}

func hostCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	_, c, err := load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, n := range c.Names() {
		if toComplete != "" && !strings.HasPrefix(n, toComplete) {
			continue
		}
		h := c.Hosts[n]
		desc := h.Notes
		if desc == "" {
			desc = h.IP
		}
		// A newline or tab in the description would corrupt the completion protocol.
		desc = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, desc)
		out = append(out, n+"\t"+desc)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// argOr returns args[0], or def when no optional argument was given.
func argOr(args []string, def string) string {
	if len(args) > 0 {
		return args[0]
	}
	return def
}

func code(n int, err error) error {
	if err != nil {
		return err
	}
	if n != 0 {
		return exitCodeErr(n)
	}
	return nil
}

func confirm(msg string) error {
	if yesFlag {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("confirmation required; rerun with --yes")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", msg)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if s := strings.ToLower(strings.TrimSpace(line)); s != "y" && s != "yes" {
		return errors.New("aborted")
	}
	return nil
}

// withHost resolves args[0] and runs fn.
func withHost(fn func(name string, h Host, args []string) error) func(*cobra.Command, []string) error {
	return withHostD("", fn)
}

// withHostD is withHost for commands that can be destructive. When action with
// the remaining args is in destructiveActions, --yes is only honored for an
// exact host name, never for a prefix match.
func withHostD(action string, fn func(name string, h Host, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		_, c, err := load()
		if err != nil {
			return err
		}
		name, h, err := c.Lookup(args[0])
		if err != nil {
			return err
		}
		if name != args[0] {
			fmt.Fprintf(os.Stderr, "-> %s\n", name)
			if yesFlag && isDestructive(action, args[1:]) {
				return fmt.Errorf("--yes requires the exact host name for this command (%q matched %q by prefix)", args[0], name)
			}
		}
		return fn(name, h, args[1:])
	}
}

func doSSH(h Host, name string, reset bool, remote []string) error {
	kh, err := knownHostsPath()
	if err != nil {
		return err
	}
	if err := checkSSHCreds(name, h); err != nil {
		return err
	}
	if reset {
		if err := resetHostKey(kh, net.JoinHostPort(h.IP, strconv.Itoa(h.Port))); err != nil {
			return err
		}
	}
	cl, err := sshConnectOpts(h, kh, newDetector(name, h).Banner)
	if err != nil {
		return fmt.Errorf("%s (%s): %w", name, h.IP, nameConnErr(err, name))
	}
	defer cl.Close()
	return code(sshSession(cl, remoteCommand(remote), name, h))
}

func newSSHCmd() *cobra.Command {
	var reset bool
	c := &cobra.Command{
		Use:   "ssh <name> [remote command...]",
		Short: "The BMC's own command line (e.g. </>hpeiLO->), not the server",
		Long: "SSH to the BMC itself: its own command line (</>hpeiLO-> on iLO, /admin1-> on iDRAC),\n" +
			"where power, uid and similar BMC commands run. This is not the server's console;\n" +
			"for the machine itself (its login prompt, GRUB, BIOS) use `" + AppName + " sol <name>`.\n" +
			"On iLO and iDRAC, ctrl+] then c switches between the two in one session.\n\n" +
			"Login uses the host's `key` if set, else ssh-agent keys, then the stored password.\n" +
			"Host keys are pinned on first connect; after a BMC reimage use --reset-hostkey.",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: hostCompletion,
		RunE: withHost(func(name string, h Host, rest []string) error {
			return doSSH(h, name, reset, rest)
		}),
	}
	c.Flags().BoolVar(&reset, "reset-hostkey", false, "forget the stored host key first (after a BMC reimage)")
	return c
}

func newRoot() *cobra.Command {
	sshCmd := newSSHCmd()
	root := &cobra.Command{
		Use:     AppName + " <name> | <command>",
		Version: versionString(),
		Short:   "Shortcuts for managing BMCs over SSH and IPMI",
		Long: "Shortcuts for managing BMCs over SSH and IPMI.\n\n" +
			"Two ways in:\n" +
			"  " + AppName + " sol <name>   the server's own console (login prompt, GRUB, BIOS, kernel panics)\n" +
			"  " + AppName + " ssh <name>   the BMC's command line (</>hpeiLO->, /admin1->)\n" +
			"`" + AppName + " <name>` is shorthand for `" + AppName + " ssh <name>`. In either, ctrl+] opens the key\n" +
			"menu (on iLO/iDRAC, ctrl+] then c switches between the two) and ~. disconnects.",
		SilenceUsage:      true,
		SilenceErrors:     true,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: hostCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if err := suggestCommand(args[0]); err != nil {
				return err
			}
			return sshCmd.RunE(cmd, args)
		},
	}
	root.PersistentFlags().StringVarP(&cfgFlag, "config", "c", "", "config file (default ~/.config/"+AppName+"/hosts.json, or $XDG_CONFIG_HOME/"+AppName+"/hosts.json when set; must exist unless the command is add or import)")
	root.PersistentFlags().BoolVarP(&yesFlag, "yes", "y", false, "skip confirmation prompts")

	root.SetVersionTemplate(AppName + " {{.Version}}\n")
	root.AddCommand(sshCmd, &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run:   func(*cobra.Command, []string) { fmt.Println(AppName, versionString()) },
	})
	root.AddCommand(bmcCmds()...)
	root.AddCommand(statusCmd(), lsCmd(), addCmd(), rmCmd(), editCmd(), importCmd(), completionCmd())
	root.SetFlagErrorFunc(flagError)
	recordCommandNames(root)
	for _, c := range root.Commands() {
		friendlyArgs(c)
	}
	return root
}

var numFlagRE = regexp.MustCompile(`for "(?:-\w, )?--([\w-]+)" flag: strconv\.Parse(?:Int|Uint)`)

// flagError turns pflag's parse errors for numeric flags into short messages.
func flagError(_ *cobra.Command, err error) error {
	if m := numFlagRE.FindStringSubmatch(err.Error()); m != nil {
		return fmt.Errorf("%s must be a number", m[1])
	}
	return err
}

// friendlyArgs replaces cobra's "accepts N arg(s)" errors with the command's
// usage line.
func friendlyArgs(c *cobra.Command) {
	if orig := c.Args; orig != nil {
		c.Args = func(cmd *cobra.Command, args []string) error {
			if err := orig(cmd, args); err != nil {
				return fmt.Errorf("usage: %s%s", cmd.CommandPath(), strings.TrimPrefix(cmd.Use, cmd.Name()))
			}
			return nil
		}
	}
	for _, sub := range c.Commands() {
		friendlyArgs(sub)
	}
}

// completionCmd generates shell completions; unlike cobra's default it rejects
// unsupported shells with an error.
func completionCmd() *cobra.Command {
	shells := []string{"bash", "zsh", "fish", "powershell"}
	return &cobra.Command{
		Use:       "completion <" + strings.Join(shells, "|") + ">",
		Short:     "Generate a shell completion script",
		Args:      cobra.ExactArgs(1),
		ValidArgs: shells,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(w, true)
			case "zsh":
				return cmd.Root().GenZshCompletion(w)
			case "fish":
				return cmd.Root().GenFishCompletion(w, true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(w)
			}
			return fmt.Errorf("unsupported shell %q (use %s)", args[0], strings.Join(shells, ", "))
		},
	}
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// closestCommand returns the subcommand within edit distance 2 of arg, if any.
func closestCommand(arg string) string {
	best, bd := "", 3
	for _, n := range commandNames() {
		if d := editDistance(strings.ToLower(arg), n); d < bd {
			best, bd = n, d
		}
	}
	return best
}

// suggestCommand errors with a "did you mean" hint when arg is neither a host
// (exact or prefix) nor a command but is close to a command name.
func suggestCommand(arg string) error {
	p, err := cfgPath()
	if err != nil {
		return nil
	}
	c, err := LoadConfig(p, nil)
	if err != nil {
		return nil // let the normal path report it
	}
	if _, _, err := c.Lookup(arg); err == nil || strings.Contains(err.Error(), "ambiguous") {
		return nil
	}
	if s := closestCommand(arg); s != "" {
		return fmt.Errorf("no such host or command %q, did you mean %q?", arg, s)
	}
	return nil
}

// argsThen completes a host name first, then a fixed word list.
func argsThen(words []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, tc string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return hostCompletion(cmd, args, tc)
		}
		if len(args) == 1 {
			return words, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}
