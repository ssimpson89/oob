package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"

	"github.com/ssimpson89/oob/internal/app"
)

// Host is one BMC entry. Empty User/Pass/Port/Cipher fall back to Defaults.
type Host struct {
	IP     string   `json:"ip,omitempty"`
	User   string   `json:"user,omitempty"`
	Pass   string   `json:"pass,omitempty"`
	Port   int      `json:"port,omitempty"`   // SSH port, default 22
	Cipher string   `json:"cipher,omitempty"` // ipmitool lanplus cipher suite (-C)
	Key    string   `json:"key,omitempty"`    // SSH private key file (~ expanded); used instead of the ssh-agent
	Notes  string   `json:"notes,omitempty"`
	Type   string   `json:"type,omitempty"`    // "" = generic IPMI (auto-detects the vendor over SSH), "ilo", "idrac", "supermicro", "ipmi" = generic, no detection
	ILOGen int      `json:"ilo_gen,omitempty"` // iLO generation (5, 6, 7) when seen in the SSH banner
	Tags   []string `json:"tags,omitempty"`
	// CharDelayMS and LineDelayMS set the gap between typed bytes on a serial
	// console, and after Enter. Unset keeps the built-in 10 and 200; 0 turns
	// the delay off.
	CharDelayMS *int `json:"char_delay_ms,omitempty"`
	LineDelayMS *int `json:"line_delay_ms,omitempty"`

	// Name is the host's name in the config. It is set by Lookup, never saved,
	// and only used to name the host in messages.
	Name string `json:"-"`

	// extra holds keys this version does not know, so a load and save keeps
	// them (for example fields written by a newer oob).
	extra map[string]json.RawMessage
}

// jsonKeys lists the JSON keys of the fields of struct type t.
func jsonKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if n, _, _ := strings.Cut(f.Tag.Get("json"), ","); n != "-" {
			if n == "" {
				n = f.Name
			}
			keys[n] = true
		}
	}
	return keys
}

var hostKeys = jsonKeys(reflect.TypeOf(Host{}))

// splitExtra returns the keys of the JSON object b that are not in known.
func splitExtra(b []byte, known map[string]bool) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	for k := range all {
		if known[k] {
			delete(all, k)
		}
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// withExtra adds the unknown keys to the JSON object b.
func withExtra(b []byte, extra map[string]json.RawMessage) ([]byte, error) {
	if len(extra) == 0 {
		return b, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

type hostPlain Host

// UnmarshalJSON decodes the known fields and keeps the unknown ones.
func (h *Host) UnmarshalJSON(b []byte) error {
	var p hostPlain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	extra, err := splitExtra(b, hostKeys)
	if err != nil {
		return err
	}
	*h = Host(p)
	h.extra = extra
	return nil
}

// MarshalJSON encodes the known fields and the kept unknown ones.
func (h Host) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(hostPlain(h))
	if err != nil {
		return nil, err
	}
	return withExtra(b, h.extra)
}

// Config is the on-disk JSON document.
type Config struct {
	Defaults Host            `json:"defaults"`
	Hosts    map[string]Host `json:"hosts"`

	extra map[string]json.RawMessage // unknown top-level keys, kept on save
}

type configPlain Config

var configKeys = jsonKeys(reflect.TypeOf(Config{}))

// UnmarshalJSON decodes the known keys and keeps the unknown ones.
func (c *Config) UnmarshalJSON(b []byte) error {
	var p configPlain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	extra, err := splitExtra(b, configKeys)
	if err != nil {
		return err
	}
	*c = Config(p)
	c.extra = extra
	return nil
}

// MarshalJSON encodes the known keys and the kept unknown ones.
func (c Config) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(configPlain(c))
	if err != nil {
		return nil, err
	}
	return withExtra(b, c.extra)
}

// ConfigPath returns the config file location: explicit override, env var,
// $XDG_CONFIG_HOME, then ~/.config.
func ConfigPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if v := os.Getenv(strings.ToUpper(app.Name) + "_CONFIG"); v != "" {
		return v, nil
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, app.Name, "hosts.json"), nil
}

// LoadConfig reads the file. A missing file yields an empty config. warn, if non-nil,
// receives a message when permissions are looser than 0600.
func LoadConfig(p string, warn func(string)) (*Config, error) {
	c := &Config{Hosts: map[string]Host{}}
	st, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if warn != nil && st.Mode().Perm()&0o077 != 0 {
		warn(fmt.Sprintf("%s has mode %04o, expected 0600 (contains passwords); fix with: chmod 600 %s", p, st.Mode().Perm(), p))
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
	}
	if c.Hosts == nil {
		c.Hosts = map[string]Host{}
	}
	return c, nil
}

// Save writes the config atomically with mode 0600 (directory 0700).
func (c *Config) Save(p string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(p, append(b, '\n'))
}

// ErrNoUpdate is returned by an UpdateConfig callback that changed nothing, so
// the file is left alone.
var ErrNoUpdate = errors.New("config unchanged")

// UpdateConfig is the safe way to change the saved config: it takes an
// exclusive lock on a lock file next to the config (so two oob processes,
// such as a status run detecting vendors and a console saving a cipher, cannot
// overwrite each other's changes), reloads the config, applies fn, and saves
// atomically. If fn returns ErrNoUpdate nothing is written and nil is returned.
func UpdateConfig(fn func(*Config) error) error {
	p, err := CurrentPath()
	if err != nil {
		return err
	}
	return updateConfigAt(p, fn)
}

// Override reports the config path given with -c/--config ("" for none). The
// command line front end sets it; the default is no override.
var Override = func() string { return "" }

// CurrentPath is ConfigPath for the path the command line asked for.
func CurrentPath() (string, error) { return ConfigPath(Override()) }

// NoUserError is the one message for a host with no user; who names the host.
func NoUserError(who string) error {
	return fmt.Errorf("%s: no user (set it on the host or in defaults)", who)
}

func updateConfigAt(p string, fn func(*Config) error) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", p+".lock", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	c, err := LoadConfig(p, nil)
	if err != nil {
		return err
	}
	if err := fn(c); err != nil {
		if errors.Is(err, ErrNoUpdate) {
			return nil
		}
		return err
	}
	return c.Save(p)
}

// WriteFileAtomic writes data to p via a synced temp file and rename, mode 0600.
// If p is a symlink, the file it points to is replaced and the link is kept.
func WriteFileAtomic(p string, data []byte) error {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".hosts-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// merge fills empty fields of h from defaults.
func (c *Config) merge(h Host) Host {
	if h.User == "" {
		h.User = c.Defaults.User
	}
	if h.Pass == "" {
		h.Pass = c.Defaults.Pass
	}
	if h.Key == "" {
		h.Key = c.Defaults.Key
	}
	if h.Port == 0 {
		h.Port = c.Defaults.Port
	}
	if h.Port == 0 {
		h.Port = 22
	}
	if h.Cipher == "" {
		h.Cipher = c.Defaults.Cipher
	}
	if h.Type == "" {
		h.Type = c.Defaults.Type
	}
	if h.CharDelayMS == nil {
		h.CharDelayMS = c.Defaults.CharDelayMS
	}
	if h.LineDelayMS == nil {
		h.LineDelayMS = c.Defaults.LineDelayMS
	}
	return h
}

// Lookup finds a host by exact name, else by unique prefix, and returns it with
// defaults merged.
func (c *Config) Lookup(name string) (string, Host, error) {
	if name == "" {
		return "", Host{}, errors.New("empty host name")
	}
	if h, ok := c.Hosts[name]; ok {
		h = c.merge(h)
		h.Name = name
		return name, h, nil
	}
	var matches []string
	for n := range c.Hosts {
		if strings.HasPrefix(n, name) {
			matches = append(matches, n)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", Host{}, fmt.Errorf("no such host %q (see: %s ls)", name, app.Name)
	case 1:
		h := c.merge(c.Hosts[matches[0]])
		h.Name = matches[0]
		return matches[0], h, nil
	}
	return "", Host{}, fmt.Errorf("%q is ambiguous: %s", name, strings.Join(matches, ", "))
}

// LookupExact finds a host by its exact name only (no prefix matching), with
// defaults merged. Commands that change state use it so a short name can never
// pick a different host than intended.
func (c *Config) LookupExact(name string) (Host, error) {
	h, ok := c.Hosts[name]
	if !ok {
		return Host{}, fmt.Errorf("no such host %q (see: %s ls)", name, app.Name)
	}
	h = c.merge(h)
	h.Name = name
	return h, nil
}

// Select returns sorted names matching sel: "" (all), "tag:x", a glob, or a
// substring. Matching is case-insensitive.
func (c *Config) Select(sel string) []string {
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

// Names returns sorted host names.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Hosts))
	for n := range c.Hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
