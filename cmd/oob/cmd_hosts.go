package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/ssimpson89/oob/internal/app"
	"github.com/ssimpson89/oob/internal/bmc"
	"github.com/ssimpson89/oob/internal/config"
	"github.com/ssimpson89/oob/internal/console"
	"github.com/ssimpson89/oob/internal/detect"
	"golang.org/x/term"
)

// clampWorkers bounds the worker count to [1, n].
func clampWorkers(w, n int) int {
	return max(1, min(w, n))
}

// Seams for tests.
var (
	resolveHost = bmc.Resolve
	driverFor   = func(name string, h config.Host) interface {
		PowerStatus(time.Duration) bmc.StatusResult
	} {
		return bmc.DriverFor(name, h)
	}
)

// powerStatus reads one host's power state. An untyped host is probed for its
// vendor first, quietly and within the same per-host deadline; the read gets
// only what is left of it.
func powerStatus(name string, h config.Host, timeout time.Duration) bmc.StatusResult {
	start := time.Now()
	h = resolveHost(name, h, timeout)
	left := timeout - time.Since(start)
	if left <= 0 {
		err := &console.TimeoutError{Host: name, Via: "status", After: timeout}
		return bmc.StatusResult{Text: err.Error(), Failed: true}
	}
	return driverFor(name, h).PowerStatus(left)
}

func statusCmd() *cobra.Command {
	var workers int
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "status [selector]",
		Short: "Power status for many hosts concurrently (selector: glob, substring or tag:<tag>)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, cfg, err := load()
			if err != nil {
				return err
			}
			names := selectHosts(cfg, argOr(args, ""))
			if len(names) == 0 {
				return errors.New("no hosts match")
			}
			setQuietNotes(true)
			res := make([]bmc.StatusResult, len(names))
			jobs := make(chan int)
			var wg sync.WaitGroup
			for w := 0; w < clampWorkers(workers, len(names)); w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range jobs {
						_, h, _ := cfg.Lookup(names[i])
						res[i] = powerStatus(names[i], h, timeout)
					}
				}()
			}
			for i := range names {
				jobs <- i
			}
			close(jobs)
			wg.Wait()
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			bad := 0
			for i, n := range names {
				if res[i].Failed {
					bad++
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", n, cfg.Hosts[n].IP, console.CleanRemoteLine(res[i].Text))
			}
			tw.Flush()
			if bad > 0 {
				return app.ExitCode(1)
			}
			return nil
		},
	}
	c.Flags().IntVar(&workers, "workers", 16, "concurrent workers")
	c.Flags().DurationVar(&timeout, "timeout", 20*time.Second, "per-host timeout")
	return c
}

// selectHosts returns sorted names matching sel: "" (all), "tag:x", a glob, or a
// substring. Matching is case-insensitive.
func selectHosts(c *config.Config, sel string) []string {
	sel = strings.ToLower(sel)
	var out []string
	for _, n := range c.Names() {
		h := c.Hosts[n]
		ln := strings.ToLower(n)
		switch {
		case sel == "":
			out = append(out, n)
		case strings.HasPrefix(sel, "tag:"):
			want := strings.TrimPrefix(sel, "tag:")
			for _, t := range h.Tags {
				if strings.ToLower(t) == want {
					out = append(out, n)
					break
				}
			}
		default:
			if ok, _ := path.Match(sel, ln); ok || strings.Contains(ln, sel) {
				out = append(out, n)
			}
		}
	}
	return out
}

func lsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls [filter]", Short: "List hosts (passwords are never shown)", Args: cobra.MaximumNArgs(1),
		Long: "List hosts (passwords are never shown). The optional filter is one of:\n" +
			"  a glob such as 'web*' or 'r?-bmc' (matched against the whole name)\n" +
			"  a substring such as 'rack2'\n" +
			"  tag:<tag> for hosts carrying that tag\n" +
			"Matching is case-insensitive. The same filters work for `status`.",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := load()
			if err != nil {
				return err
			}
			names := selectHosts(c, argOr(args, ""))
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tIP\tUSER\tTAGS\tNOTES")
			for _, n := range names {
				_, h, _ := c.Lookup(n)
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n, h.IP, h.User, strings.Join(h.Tags, ","), h.Notes)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if len(names) == 0 && len(args) > 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no hosts match", args[0])
			}
			return nil
		},
	}
}

// validTypes lists the accepted --type values (excluding the empty default).
func validTypes() []string {
	var out []string
	for _, t := range []string{detect.TypeILO, detect.TypeIDRAC, detect.TypeSupermicro, detect.TypeIPMI} {
		if detect.ValidType(t) {
			out = append(out, t)
		}
	}
	return out
}

func readPassStdin(r io.Reader) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			if err == io.EOF && sb.Len() > 0 {
				break
			}
			if err == io.EOF {
				return "", errors.New("--pass-stdin: no password on stdin")
			}
			return "", err
		}
	}
	pw := strings.TrimRight(sb.String(), "\r")
	if pw == "" {
		return "", errors.New("--pass-stdin: empty password on stdin")
	}
	return pw, nil
}

// promptPassword asks for a password on the terminal without echo.
func promptPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("no password: use --pass, --pass-stdin or run in a terminal")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	stop := func() {}
	if st, err := term.GetState(fd); err == nil {
		stop = console.RestoreOnSignal(fd, st, nil, syscall.SIGINT)
	}
	b, err := term.ReadPassword(fd)
	stop()
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// hostFields holds the per-host flags shared by add and edit <name>.
type hostFields struct {
	h         config.Host
	tags      []string
	passStdin bool
}

func (hf *hostFields) bind(f *pflag.FlagSet) {
	f.StringVar(&hf.h.User, "user", "", "BMC user (default from defaults block)")
	f.StringVar(&hf.h.Pass, "pass", "", "BMC password (visible in shell history; prefer the prompt or --pass-stdin)")
	f.BoolVar(&hf.passStdin, "pass-stdin", false, "read the BMC password from one line of stdin")
	f.StringVar(&hf.h.Key, "key", "", "SSH private key file (used instead of the ssh-agent; the BMC needs the public key; IPMI/Redfish still need the password; \"\" clears it)")
	f.IntVar(&hf.h.Port, "port", 0, "SSH port")
	f.StringVar(&hf.h.Cipher, "cipher", "", "ipmitool lanplus cipher suite, e.g. 17 or 3")
	f.StringVar(&hf.h.Notes, "notes", "", "free-form notes (also --note)")
	f.StringVar(&hf.h.Type, "type", "", "BMC type: "+strings.Join(validTypes(), ", ")+` ("ilo": HPE iLO, Redfish and SSH CLI; "idrac" and "supermicro" are untested; "ipmi" pins generic IPMI); default: generic, and the vendor is auto-detected over SSH on first use`)
	f.StringSliceVar(&hf.tags, "tag", nil, "tag (repeatable; replaces existing tags)")
	f.SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "note" {
			name = "notes"
		}
		return pflag.NormalizedName(name)
	})
}

// apply adds or updates host name, changing only the flags that were passed.
// ipSet says whether an address was given at all, so an explicitly empty one
// (say, a script variable that expanded to nothing) is rejected, not ignored. With mustExist (edit <name>) a missing host is an
// error instead of being created, so a typo can't add a stray entry.
func (hf *hostFields) apply(cmd *cobra.Command, name, ip string, ipSet, mustExist bool) error {
	fl, h := cmd.Flags(), hf.h
	if err := validHostName(name); err != nil {
		return err
	}
	if !detect.ValidType(h.Type) {
		return fmt.Errorf("unknown --type %q (valid: %s)", h.Type, strings.Join(validTypes(), ", "))
	}
	if fl.Changed("port") {
		if err := validPort(h.Port); err != nil {
			return err
		}
	}
	if ipSet && !validIPOrHost(ip) {
		return fmt.Errorf("ip %q is not an IP address or hostname", ip)
	}
	if hf.passStdin && fl.Changed("pass") {
		return errors.New("use only one of --pass and --pass-stdin")
	}
	noHost := fmt.Errorf("no such host %q (exact name required; use %s add to create it)", name, AppName)
	// Read once up front: it prints the "creating" and permission notes,
	// and decides whether a password prompt is needed. The prompt must
	// not happen under the config lock, so the change itself is applied
	// afterwards by UpdateConfig on a fresh read.
	var c *config.Config
	var err error
	if mustExist {
		_, c, err = load()
	} else {
		_, c, err = loadCreate(cmd.ErrOrStderr())
	}
	if err != nil {
		return err
	}
	cur, exists := c.Hosts[name]
	if mustExist && !exists {
		return noHost
	}
	if !exists && !ipSet {
		return fmt.Errorf("new host %q needs an ip: usage: %s add <name> <ip>", name, AppName)
	}
	var pw string
	var havePW bool
	switch {
	case hf.passStdin:
		if pw, err = readPassStdin(cmd.InOrStdin()); err != nil {
			return err
		}
		havePW = true
	case fl.Changed("pass"):
		pw, havePW = h.Pass, true
	case !mustExist && cur.Pass == "" && c.Defaults.Pass == "" && !hasKey(fl.Changed("key"), h.Key, cur.Key, c.Defaults.Key):
		if pw, err = promptPassword(); err != nil {
			return err
		}
		havePW = true
	}
	var changed []string
	set := func(label string) { changed = append(changed, label) }
	err = config.UpdateConfig(func(c *config.Config) error {
		changed = nil
		cur, exists = c.Hosts[name]
		if mustExist && !exists {
			return noHost
		}
		if !exists && !ipSet {
			return fmt.Errorf("new host %q needs an ip: usage: %s add <name> <ip>", name, AppName)
		}
		if ipSet {
			cur.IP = ip
			set("ip")
		}
		if fl.Changed("user") {
			cur.User = h.User
			set("user")
		}
		if fl.Changed("port") {
			cur.Port = h.Port
			set("port")
		}
		if fl.Changed("key") {
			cur.Key = h.Key
			set("key")
		}
		if fl.Changed("cipher") {
			cur.Cipher = h.Cipher
			set("cipher")
		}
		if fl.Changed("notes") {
			cur.Notes = h.Notes
			set("notes")
		}
		if fl.Changed("type") {
			cur.Type = h.Type
			set("type")
		}
		if fl.Changed("tag") {
			cur.Tags = hf.tags
			set("tags")
		}
		if havePW {
			cur.Pass = pw
			set("pass")
		}
		c.Hosts[name] = cur
		return nil
	})
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(cmd.OutOrStdout(), "added %s\n", name)
	} else if len(changed) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "updated %s (no changes)\n", name)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "updated %s (%s)\n", name, strings.Join(changed, ", "))
	}
	return nil
}

func addCmd() *cobra.Command {
	var hf hostFields
	c := &cobra.Command{
		Use: "add <name> [ip]", Short: "Add a host (or update one: only the fields you pass change)",
		Long: "Add a host. The ip is required for a new host.\n" +
			"On an existing host only the fields you pass are changed (a stored password is\n" +
			"kept unless --pass or --pass-stdin is given; --tag replaces the tag list).\n" +
			"For a new host without --pass/--pass-stdin and no default password, the password is\n" +
			"prompted for (needs a terminal), unless the host has a key: SSH then uses the key, but IPMI\n" +
			"and Redfish need a password, so add one with --pass-stdin if you use them.\n" +
			"To change an existing host without any risk of creating one, use `" + AppName + " edit <name>`.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ip := ""
			if len(args) == 2 {
				ip = args[1]
			}
			return hf.apply(cmd, args[0], ip, len(args) == 2, false)
		},
	}
	hf.bind(c.Flags())
	return c
}

func rmCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rm <name>", Short: "Remove a host (exact name required)", Args: cobra.ExactArgs(1), ValidArgsFunction: hostCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Check on a plain read first so a missing config or host does not
			// create the directory and lock file.
			_, c, err := load()
			if err != nil {
				return err
			}
			noHost := fmt.Errorf("no such host %q (exact name required)", args[0])
			if _, ok := c.Hosts[args[0]]; !ok {
				return noHost
			}
			err = config.UpdateConfig(func(c *config.Config) error {
				if _, ok := c.Hosts[args[0]]; !ok {
					return noHost
				}
				delete(c.Hosts, args[0])
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", args[0])
			return nil
		},
	}
}

func editCmd() *cobra.Command {
	var hf hostFields
	var ip string
	c := &cobra.Command{
		Use:   "edit [name]",
		Short: "Change a host's fields (e.g. edit node01 --note \"rack 4\"), or open the config file",
		Long: "With a host name, change only the fields you pass on that existing host:\n" +
			"  " + AppName + " edit node01 --note \"rack 4, PSU swapped\"\n" +
			"  " + AppName + " edit node01 --ip 192.0.2.20 --tag lab --tag gpu\n" +
			"The host must already exist (use " + AppName + " add to create one).\n\n" +
			"With no name, open the whole config in $VISUAL or $EDITOR (falls back to vi).",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: hostCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return hf.apply(cmd, args[0], ip, cmd.Flags().Changed("ip"), true)
			}
			// Only this command's own flags count: global ones like -c and -y
			// still mean "open the editor".
			changed := false
			cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) { changed = changed || f.Changed })
			if changed {
				return fmt.Errorf("field flags need a host name: usage: %s edit <name> --note ...", AppName)
			}
			return openEditor(cmd)
		},
	}
	hf.bind(c.Flags())
	c.Flags().StringVar(&ip, "ip", "", "BMC IP address or hostname")
	return c
}

// openEditor opens the config file in the user's editor and checks it still parses.
func openEditor(cmd *cobra.Command) error {
	p, c, err := load()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		if err := c.Save(p); err != nil {
			return err
		}
	}
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if strings.TrimSpace(ed) == "" {
		ed = "vi"
		fmt.Fprintln(cmd.ErrOrStderr(), "using vi (set EDITOR to change)")
	}
	parts := strings.Fields(ed)
	ec := exec.Command(parts[0], append(parts[1:], p)...)
	ec.Stdin, ec.Stdout, ec.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := ec.Run(); err != nil {
		return err
	}
	if _, err := config.LoadConfig(p, nil); err != nil {
		return fmt.Errorf("config is now invalid: %w", err)
	}
	return nil
}

func importCmd() *cobra.Command {
	var overwrite bool
	c := &cobra.Command{
		Use: "import <file.csv>", Short: "Bulk load hosts: name,ip,user,pass,port,cipher,notes,tags(;-separated)",
		Long: "Bulk load hosts from a CSV file with columns name,ip,user,pass,port,cipher,notes,tags.\n" +
			"Tags are separated by ';'. A header row is optional (a first cell of \"name\" is skipped).\n" +
			"Blank lines are skipped. The import is all-or-nothing: if any row is bad, every problem is\n" +
			"listed with its row number and nothing is imported. Existing hosts are skipped unless --overwrite.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			res, err := ParseHostsCSV(f)
			if err != nil {
				return err
			}
			for _, w := range res.Warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			if len(res.Errors) > 0 {
				return fmt.Errorf("%s has %d bad row(s), nothing imported:\n  %s", args[0], len(res.Errors), strings.Join(res.Errors, "\n  "))
			}
			// The read only prints the "creating" and permission notes.
			if _, _, err := loadCreate(cmd.ErrOrStderr()); err != nil {
				return err
			}
			added := 0
			var skipped []string
			err = config.UpdateConfig(func(c *config.Config) error {
				added, skipped = 0, nil
				for _, n := range res.Order {
					if _, ok := c.Hosts[n]; ok && !overwrite {
						skipped = append(skipped, n)
						continue
					}
					c.Hosts[n] = res.Hosts[n]
					added++
				}
				sort.Strings(skipped)
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "imported %d (skipped %d blank/header)\n", added, res.Skipped)
			if len(skipped) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "skipped %d existing: %s (use --overwrite to replace)\n", len(skipped), strings.Join(skipped, ", "))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&overwrite, "overwrite", false, "replace existing hosts")
	return c
}

// hasKey reports whether the host will have an SSH key after this add: the
// one passed (changed is true), else the stored one, else the default.
func hasKey(changed bool, flagKey, cur, def string) bool {
	if changed {
		return flagKey != ""
	}
	return cur != "" || def != ""
}
