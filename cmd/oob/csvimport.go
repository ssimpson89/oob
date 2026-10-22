package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/ssimpson89/oob/internal/config"
)

// csvMaxCols is the number of columns oob understands:
// name,ip,user,pass,port,cipher,notes,tags.
const csvMaxCols = 8

// CSVResult is the outcome of parsing a host CSV.
type CSVResult struct {
	Hosts    map[string]config.Host
	Order    []string // names in file order
	Skipped  int      // blank rows, # comment rows and the header row
	Warnings []string
	Errors   []string // sorted by row; non-empty means nothing may be imported
}

var hostnameRE = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9._-]*[A-Za-z0-9_])?$`)

// validIPOrHost reports whether s parses as an IP address or looks like a
// hostname (no spaces, sane characters).
func validIPOrHost(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	if net.ParseIP(s) != nil {
		return true
	}
	return hostnameRE.MatchString(s)
}

// validHostName checks a host entry name, returning a reason when it is bad.
func validHostName(n string) error {
	if n == "" {
		return errors.New("blank name")
	}
	if strings.HasPrefix(n, "-") {
		return fmt.Errorf("name %q must not start with '-'", n)
	}
	if i := strings.IndexAny(n, ":*?[]"); i >= 0 {
		return fmt.Errorf("name %q must not contain %q (reserved for host selection)", n, n[i])
	}
	for _, r := range n {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("name %q must not contain spaces or control characters", n)
		}
	}
	if isCommandName(n) {
		return fmt.Errorf("name %q is a command name; `%s %s` would run the command, so the shorthand won't reach this host", n, AppName, n)
	}
	return nil
}

// cmdNames caches the subcommand names; newRoot fills it. Building a root
// resets the global flag variables, so it must never be rebuilt mid-command.
var cmdNames []string

func recordCommandNames(root *cobra.Command) {
	seen := map[string]bool{"help": true, "completion": true}
	for _, c := range root.Commands() {
		seen[c.Name()] = true
		for _, a := range c.Aliases {
			seen[a] = true
		}
	}
	cmdNames = cmdNames[:0]
	for n := range seen {
		if !strings.HasPrefix(n, "__") {
			cmdNames = append(cmdNames, n)
		}
	}
	sort.Strings(cmdNames)
}

// commandNames returns every subcommand name and alias, including the ones
// cobra adds on its own.
func commandNames() []string {
	if cmdNames == nil {
		cf, yf := cfgFlag, yesFlag
		newRoot()
		cfgFlag, yesFlag = cf, yf
	}
	return cmdNames
}

func isCommandName(n string) bool {
	for _, c := range commandNames() {
		if c == n {
			return true
		}
	}
	return false
}

// validPort checks an SSH port value.
func validPort(p int) error {
	if p < 1 || p > 65535 {
		return errors.New("port must be a number between 1 and 65535")
	}
	return nil
}

// ParseHostsCSV parses name,ip,user,pass,port,cipher,notes,tags (tags separated
// by ';'). A UTF-8 BOM is stripped, a header row (first cell "name") and blank
// lines are skipped. The caller must import nothing when Errors is non-empty.
func ParseHostsCSV(in io.Reader) (*CSVResult, error) {
	raw, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	res := &CSVResult{Hosts: map[string]config.Host{}}

	// Blank out whitespace-only lines (keeps line numbers) and count them.
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		last := i == len(lines)-1 // the piece after a final newline is not a line
		switch {
		case strings.TrimSpace(l) == "":
			lines[i] = ""
			if !last {
				res.Skipped++
			}
		case strings.HasPrefix(l, "#"): // csv.Reader drops these; count them
			res.Skipped++
		}
	}
	r := csv.NewReader(strings.NewReader(strings.Join(lines, "\n")))
	r.Comment = '#' // must match the HasPrefix("#") counting above
	r.FieldsPerRecord = -1

	type rowErr struct {
		row int
		msg string
	}
	var errs []rowErr
	bad := func(row int, f string, a ...any) { errs = append(errs, rowErr{row, fmt.Sprintf(f, a...)}) }
	first := true
	firstRow := map[string]int{}

	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				bad(pe.StartLine, "%v", pe.Err)
				continue
			}
			return nil, err
		}
		line, _ := r.FieldPos(0)
		for j := range row {
			row[j] = strings.TrimSpace(row[j])
		}
		if first {
			first = false
			if strings.EqualFold(row[0], "name") {
				res.Skipped++
				continue
			}
		}
		empty := true
		for _, c := range row {
			if c != "" {
				empty = false
			}
		}
		if empty {
			res.Skipped++
			continue
		}
		if len(row) > csvMaxCols {
			res.Warnings = append(res.Warnings, fmt.Sprintf("row %d: %d columns, ignoring everything after column %d (tags)", line, len(row), csvMaxCols))
		}
		get := func(k int) string {
			if k < len(row) {
				return row[k]
			}
			return ""
		}
		name := row[0]
		ok := true
		if err := validHostName(name); err != nil {
			bad(line, "%v", err)
			ok = false
		} else if prev, dup := firstRow[name]; dup {
			bad(line, "duplicate name %s (also row %d)", name, prev)
			ok = false
		} else {
			firstRow[name] = line
		}
		h := config.Host{IP: get(1), User: get(2), Pass: get(3), Cipher: get(5), Notes: get(6)}
		if h.IP == "" {
			bad(line, "blank ip")
		} else if !validIPOrHost(h.IP) {
			bad(line, "ip %q is not an IP address or hostname", h.IP)
		}
		if s := get(4); s != "" {
			p, err := strconv.Atoi(s)
			if err != nil {
				bad(line, "port %q is not a number", s)
			} else if err := validPort(p); err != nil {
				bad(line, "port %d is out of range (1-65535)", p)
			} else {
				h.Port = p
			}
		}
		for _, t := range strings.Split(get(7), ";") {
			if t = strings.TrimSpace(t); t != "" {
				h.Tags = append(h.Tags, t)
			}
		}
		if ok {
			res.Hosts[name] = h
			res.Order = append(res.Order, name)
		}
	}
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].row < errs[j].row })
	for _, e := range errs {
		res.Errors = append(res.Errors, fmt.Sprintf("row %d: %s", e.row, e.msg))
	}
	return res, nil
}
