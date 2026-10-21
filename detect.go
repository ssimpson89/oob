package main

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"sync"
)

// Host types. "" means "not set": generic IPMI, and eligible for vendor
// auto-detection over SSH. typeIPMI is an explicit generic IPMI host that is
// never auto-detected.
const (
	typeILO        = "ilo"
	typeIDRAC      = "idrac"
	typeSupermicro = "supermicro"
	typeIPMI       = "ipmi"
)

// validType reports whether t is a value for a host's type field.
func validType(t string) bool {
	switch t {
	case "", typeILO, typeIDRAC, typeSupermicro, typeIPMI:
		return true
	}
	return false
}

// vendorName is the human name for a host type, for messages.
func vendorName(t string) string {
	switch t {
	case typeILO:
		return "HPE iLO"
	case typeIDRAC:
		return "Dell iDRAC"
	case typeSupermicro:
		return "Supermicro"
	}
	return "generic IPMI"
}

var (
	iloBannerRE = regexp.MustCompile(`(?i)integrated lights-out(?:\s+(\d+))?`)
	// Dell: the SMCLP prompt at the start of a line, the racadm prompt, or the
	// exact product word (iDRAC, iDRAC9). Only the banner and the first output
	// are ever matched, so text that merely mentions iDRAC later (a log, a
	// page on the host) does not count.
	idracRE = regexp.MustCompile(`(?im)^\s*/?admin1->|racadm>>|\bidrac\d{0,2}\b`)
	// Supermicro: the SMASH-CLP shell or an ATEN banner. A bare "->" prompt is
	// not enough, other vendors use it too.
	smcRE = regexp.MustCompile(`(?i:SMASH-CLP)|\bATEN\b`)
)

// detectVendor looks for a vendor's banner or prompt in s. gen is the iLO
// generation when the text names one, else 0. iLO is checked first because its
// CLI can mention SMASH-CLP.
func detectVendor(s string) (typ string, gen int) {
	if m := iloBannerRE.FindStringSubmatch(s); m != nil {
		gen, _ = strconv.Atoi(m[1])
		return typeILO, gen
	}
	if idracRE.MatchString(s) {
		return typeIDRAC, 0
	}
	if smcRE.MatchString(s) {
		return typeSupermicro, 0
	}
	return "", 0
}

// markTypeFn saves the detected type; tests replace it.
var markTypeFn = markType

// Detection looks at the SSH pre-auth banner and only the start of what the
// session says after login: the first detectWindow bytes, or up to the first
// prompt. Nothing is saved from the banner alone, and nothing is saved before
// the login worked.
const detectWindow = 4096

// promptEndRE is the end of a line that looks like a CLI prompt.
var promptEndRE = regexp.MustCompile(`(?:->|[#>$%])[ \t]*$`)

type vendorHit struct {
	typ string
	gen int
}

// pendingBanner holds a banner hit until a session for the same host name sees
// output after login (the banner callback and the session may belong to
// different detectors, e.g. in oob ssh).
var pendingBanner = struct {
	sync.Mutex
	m map[string]vendorHit
}{m: map[string]vendorHit{}}

// detector watches SSH output for a vendor's banner or prompt and saves the
// type once it has seen one after a successful login. A nil *detector does
// nothing.
type detector struct {
	name   string
	only   string // if set, only this vendor counts (an iLO host still missing its generation)
	mu     sync.Mutex
	win    []byte     // the post-login output seen so far (at most detectWindow bytes)
	cand   *vendorHit // from the banner
	closed bool       // saved, or the window is over
}

// newDetector returns nil (a no-op detector) when there is nothing to learn: the
// host has a type already, except an iLO whose generation is still unknown.
func newDetector(name string, h Host) *detector {
	switch {
	case h.Type == "":
		return &detector{name: name}
	case h.Type == typeILO && h.ILOGen == 0:
		return &detector{name: name, only: typeILO}
	}
	return nil
}

// detect is detectVendor with this detector's vendor filter.
func (d *detector) detect(s string) (vendorHit, bool) {
	typ, gen := detectVendor(s)
	if typ == "" || (d.only != "" && typ != d.only) {
		return vendorHit{}, false
	}
	return vendorHit{typ, gen}, true
}

// fire saves the result. The caller holds d.mu.
func (d *detector) fire(h vendorHit) {
	d.closed = true
	_ = markTypeFn(d.name, h.typ, h.gen) // quick and atomic; must finish before the process can exit
}

// Banner takes the SSH pre-auth banner. It only remembers a hit; the type is
// saved once output arrives from the logged-in session.
func (d *detector) Banner(s string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	if h, ok := d.detect(s); ok {
		pendingBanner.Lock()
		pendingBanner.m[d.name] = h
		pendingBanner.Unlock()
	}
}

// Feed scans session output, which only exists after login. An empty b says
// the login worked and nothing was printed.
func (d *detector) Feed(b []byte) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	if d.cand == nil {
		pendingBanner.Lock()
		if h, ok := pendingBanner.m[d.name]; ok {
			d.cand = &h
			delete(pendingBanner.m, d.name)
		}
		pendingBanner.Unlock()
		// A banner hit that names everything we want is saved right away.
		if d.cand != nil && (d.cand.typ != typeILO || d.cand.gen > 0) {
			d.fire(*d.cand)
			return
		}
	}
	if len(b) == 0 {
		if d.cand != nil {
			d.fire(*d.cand)
		}
		return
	}
	if room := detectWindow - len(d.win); room > 0 {
		d.win = append(d.win, b[:min(room, len(b))]...)
	}
	text := cleanRemoteN(string(d.win), 0)
	if h, ok := d.detect(text); ok {
		d.fire(h)
		return
	}
	// No match yet: the window ends at the first prompt or when it is full.
	if len(d.win) >= detectWindow || promptEndRE.MatchString(text) {
		if d.cand != nil {
			d.fire(*d.cand)
			return
		}
		d.closed = true
	}
}

type detectWriter struct {
	w io.Writer
	d *detector
}

func (dw detectWriter) Write(b []byte) (int, error) { dw.d.Feed(b); return dw.w.Write(b) }

type detectReader struct {
	r io.Reader
	d *detector
}

func (dr detectReader) Read(p []byte) (int, error) {
	n, err := dr.r.Read(p)
	if n > 0 {
		dr.d.Feed(p[:n])
	}
	return n, err
}

// wrapWriter and wrapReader return w and r unchanged when there is nothing to detect.
func (d *detector) wrapWriter(w io.Writer) io.Writer {
	if d == nil {
		return w
	}
	return detectWriter{w, d}
}

func (d *detector) wrapReader(r io.Reader) io.Reader {
	if d == nil {
		return r
	}
	return detectReader{r, d}
}

// markType sets the detected type on the saved host if it has none (and the iLO
// generation if that is new), keeping the rest of the config, and queues a
// one-time note for stderr. It goes through UpdateConfig, so it cannot undo a
// concurrent change.
func markType(name, typ string, gen int) error {
	var msg string
	err := UpdateConfig(func(c *Config) error {
		h, ok := c.Hosts[name]
		if !ok {
			return errNoUpdate
		}
		changed := false
		if h.Type == "" {
			h.Type = typ
			changed = true
			msg = fmt.Sprintf("detected %s on %s; saved type %q", vendorName(typ), name, typ)
			switch typ {
			case typeILO:
				msg += " (sol, power, boot, uid and nmi now use iLO Redfish and the SSH CLI)"
			case typeIDRAC, typeSupermicro:
				msg += " (support for this vendor is untested; please report results)"
			}
		}
		if h.Type == typeILO && typ == typeILO && gen > 0 && h.ILOGen != gen {
			h.ILOGen = gen
			changed = true
		}
		if !changed {
			return errNoUpdate
		}
		c.Hosts[name] = h
		return nil
	})
	if err == nil && msg != "" {
		addNotice(msg)
	}
	return err
}
