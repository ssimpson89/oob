package console

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ssimpson89/oob/internal/app"
)

// Minimum terminal height for the bar: the bar row plus a few usable rows.
const minBarRows = 5

// Timing the console depends on.
const (
	// idleRepaint: the remote quiet this long means it's between bursts, so
	// the periodic repaint can put the region and bar back without cutting in.
	idleRepaint = 300 * time.Millisecond
	// stuckSeqQuiet: quiet this long while the tracker thinks a sequence is open
	// means the sequence was cut off or was noise; give up on it so the bar
	// isn't held off forever.
	stuckSeqQuiet = time.Second
	// tickEvery is how often the backstop repaint runs.
	tickEvery = time.Second
	// noteTTL is how long a bar note stays up before it clears itself.
	noteTTL = 5 * time.Second
)

// Bracketed paste on and off. The console turns it on so a paste can't be
// mistaken for typed keys (~. or ctrl+]); the key parser strips the markers.
const (
	pasteOn  = "\x1b[?2004h"
	pasteOff = "\x1b[?2004l"
)

// console owns the bottom row of the terminal. It stays on the normal screen:
// a DECSTBM scroll region covers rows 1..H-1, so output scrolls as usual and
// lines leave the top into the terminal's own scrollback, while row H holds
// the bar. All writes to the terminal go through it, under one lock, so the
// bar is never painted into the middle of the remote's output.
type console struct {
	mu     sync.Mutex
	out    io.Writer
	rows   int
	cols   int
	closed bool

	tr      escTracker
	lastOut time.Time
	dirty   bool // the region or bar needs repainting once the stream is idle
	// staleRow is the row an old, shorter window's bar was on, to blank at the
	// next repaint (0 for none).
	staleRow int

	st      barState
	hist    *history
	pending int // bytes still queued to type at the remote
	log     *logWriter
	logs    []string
	note    *time.Timer
	// sticky is true while the bar note is a "connecting" message that stays
	// until the remote first outputs.
	sticky bool
	stopC  chan struct{}
}

func newConsole(out io.Writer, host, kind string, rows, cols int) *console {
	return &console{
		out: out, rows: rows, cols: cols,
		st:    barState{host: host, kind: kind},
		hist:  newHistory(historyLinesMax),
		stopC: make(chan struct{}),
	}
}

// DECSTBM, save and restore cursor: ESC 7 and ESC 8 around everything, because
// setting the region homes the cursor.
func (c *console) regionSeq() string {
	return fmt.Sprintf("\x1b[1;%dr", c.rows-1)
}

// paintSeq draws the bar on the last row and puts the cursor back. Autowrap
// is turned off so filling the last column can't leave a pending wrap.
func (c *console) paintSeq() string {
	return fmt.Sprintf("\x1b[%d;1H\x1b[?7l%s\x1b[?7h", c.rows, barLine(c.st, c.cols))
}

// Start reserves the bottom row and starts the periodic repaint.
func (c *console) Start() {
	c.mu.Lock()
	c.write(pasteOn)
	if c.rows < minBarRows { // too small for a bar: leave the screen alone
		c.mu.Unlock()
		go c.tick()
		return
	}
	// A newline scrolls the screen up one row if the cursor is on the last row,
	// and the up-move puts it back on the row above. Either way, the last row
	// is free and the cursor is inside the region.
	c.write("\n\x1b[A\x1b7" + c.regionSeq() + c.paintSeq() + "\x1b8")
	c.mu.Unlock()
	go c.tick()
}

func (c *console) write(s string) { _, _ = io.WriteString(c.out, s) }

// reassert re-applies the region and repaints the bar. Caller holds mu and has
// checked that the stream is idle.
func (c *console) reassert() {
	small := c.rows < minBarRows
	if small && !c.dirty && c.staleRow == 0 {
		return
	}
	c.dirty = false
	seq := "\x1b7"
	if c.staleRow > 0 {
		seq += fmt.Sprintf("\x1b[%d;1H\x1b[2K", c.staleRow)
		c.staleRow = 0
	}
	if small { // no bar: the whole screen is the remote's
		seq += "\x1b[r"
	} else {
		seq += c.regionSeq() + c.paintSeq()
	}
	c.write(seq + "\x1b8")
}

// repaint paints now if the stream is between sequences, else when it is.
func (c *console) repaint() {
	if c.closed {
		return
	}
	if c.rows < minBarRows {
		return
	}
	c.dirty = true
	if c.tr.Idle() {
		c.reassert()
	}
}

// tick backs up the clobber detection: when the remote has been quiet a
// moment, put the region and bar back whatever happened.
func (c *console) tick() {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-c.stopC:
			return
		case <-t.C:
			c.tickOnce(time.Now())
		}
	}
}

func (c *console) tickOnce(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	quiet := now.Sub(c.lastOut)
	if !c.tr.Idle() && quiet > stuckSeqQuiet {
		c.tr.Reset()
		c.dirty = true
	}
	if c.tr.Idle() && quiet > idleRepaint {
		c.reassert()
	}
}

// Output shows the remote's bytes, records them for history and the log, and
// repairs the region and bar if the remote reset them.
func (c *console) Output(b []byte) {
	if len(b) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	_, _ = c.out.Write(b)
	if c.sticky {
		c.sticky, c.st.note = false, ""
		c.dirty = true
	}
	c.lastOut = time.Now()
	c.tr.Feed(b)
	c.hist.Feed(b)
	if c.log != nil {
		if err := c.log.Write(b); err != nil {
			c.stopLogLocked("log write failed, logging stopped: " + err.Error())
		}
	}
	if clobbers(b) {
		c.dirty = true
	}
	if c.dirty && c.tr.Idle() {
		c.reassert()
	}
}

// SetSize adopts a new terminal size and redraws.
func (c *console) SetSize(rows, cols int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	old := c.rows
	c.rows, c.cols = rows, cols
	if old >= minBarRows && old < rows { // the old bar row is in the output area now
		c.staleRow = old
	}
	c.dirty = true
	if c.tr.Idle() {
		c.reassert()
	}
}

// Size returns the terminal size.
func (c *console) Size() (rows, cols int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rows, c.cols
}

// SetMode shows the bar for a key mode.
func (c *console) SetMode(m keyMode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.mode != m {
		c.st.mode = m
		c.repaint()
	}
}

// SetKind sets the bar's session label and whether the session is at the BMC
// prompt (which flips the c hint).
func (c *console) SetKind(kind string, atPrompt bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.st.kind, c.st.atPrompt = kind, atPrompt
	c.repaint()
}

// SetAuto shows whether auto-size is on.
func (c *console) SetAuto(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.st.auto = on
	c.repaint()
}

// Note shows a short message on the bar for a few seconds.
func (c *console) Note(msg string, isErr bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.noteLocked(msg, isErr)
}

func (c *console) noteLocked(msg string, isErr bool) { c.noteTTLLocked(msg, isErr, noteTTL) }

func (c *console) noteTTLLocked(msg string, isErr bool, ttl time.Duration) {
	if c.note != nil {
		c.note.Stop()
	}
	c.st.note, c.st.noteErr = CleanRemoteLine(msg), isErr
	c.note = time.AfterFunc(ttl, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.st.note = ""
		c.repaint()
	})
	c.repaint()
}

// SetSend is told how much is still queued to be typed and how much the
// queue held in all. The bar shows it, for a queue big enough that it takes a
// while; a few keystrokes need no meter.
func (c *console) SetSend(pending, total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = pending
	label := ""
	if pending > sendShowMin {
		label = sendLabel(total-pending, total)
	}
	if c.st.sending != label {
		c.st.sending = label
		c.repaint()
	}
}

// Pending is how many bytes are still queued to be typed.
func (c *console) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending
}

// Sticky shows msg on the bar until the remote first outputs something.
func (c *console) Sticky(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.note != nil {
		c.note.Stop()
	}
	c.st.note, c.st.noteErr, c.sticky = CleanRemoteLine(msg), false, true
	c.repaint()
}

// Print shows local text (the help) in the output area, on lines of its own.
func (c *console) Print(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.write("\r\n" + strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\r\n") + "\r\n")
}

// Mark writes a marker line in the log, if one is open.
func (c *console) Mark(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.log != nil {
		_ = c.log.Mark(msg)
	}
}

// ToggleLog starts or stops logging. A new log begins with the history, so it
// includes what was already on screen.
func (c *console) ToggleLog() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.log != nil {
		path := c.log.Path()
		err := c.log.Close()
		c.log, c.st.logging = nil, false
		if err != nil {
			c.noteLocked("can't save the log: "+err.Error(), true)
			return
		}
		c.noteLocked("log saved: "+filepath.Base(path), false)
		return
	}
	now := time.Now()
	path, err := logPath(c.st.host, now)
	if err != nil {
		c.noteLocked("can't start the log: "+err.Error(), true)
		return
	}
	path = uniqueLogPath(path)
	header := fmt.Sprintf("%s log: %s (%s), started %s", app.Name, c.st.host, c.st.kind, now.Format("2006-01-02 15:04:05"))
	l, err := openLog(path, header, c.hist.Lines(), c.hist.Partial())
	if err != nil {
		c.noteLocked("can't start the log: "+err.Error(), true)
		return
	}
	c.log, c.st.logging = l, true
	if !slices.Contains(c.logs, path) {
		c.logs = append(c.logs, path)
	}
	c.noteLocked("logging to "+filepath.Base(path), false)
}

func (c *console) stopLogLocked(why string) {
	if c.log != nil {
		_ = c.log.Close()
	}
	c.log, c.st.logging = nil, false
	c.noteLocked(why, true)
}

// Close restores the terminal: the scroll region is cleared, the bar row is
// erased, and the cursor ends on a fresh line. Safe to call more than once and
// from a signal handler path.
func (c *console) Close() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.logs
	}
	c.closed = true
	close(c.stopC)
	if c.note != nil {
		c.note.Stop()
	}
	if c.log != nil {
		_ = c.log.Close()
		c.log, c.st.logging = nil, false
	}
	c.write(pasteOff + "\x1b[0m\x1b7\x1b[r" + fmt.Sprintf("\x1b[%d;1H\x1b[2K", c.rows) + "\x1b8\r\n")
	return c.logs
}

// SetShared shows who else is on a shared console.
func (c *console) SetShared(s SharedStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setSharedLocked(s)
}

func (c *console) setSharedLocked(s SharedStatus) {
	c.st.sharing = true
	c.st.aiPaused = s.Paused
	label := ""
	switch {
	case s.Agent != "" && s.Paused:
		label = "shared · " + s.Agent + " paused"
	case s.Agent != "" && s.Working:
		label = "shared · " + s.Agent + " working"
	case s.Agent != "":
		label = "shared · " + s.Agent + " connected"
	case s.Windows == 1:
		label = "shared · 1 other window"
	case s.Windows > 1:
		label = "shared · " + strconv.Itoa(s.Windows) + " other windows"
	case s.Paused:
		label = "shared · AI paused"
	}
	if label != c.st.shared {
		c.st.shared = CleanRemoteLine(label)
		c.repaint()
	}
}

// SharedNote applies a note from the hub and redraws for the new status.
func (c *console) SharedNote(n Note, s SharedStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if n.Text != "" {
		text := strings.ReplaceAll(n.Text, "{keys}", shortKey("ctrl+]"))
		if n.Mark {
			if c.log != nil {
				_ = c.log.Mark(text)
			}
		} else {
			c.noteLocked(text, false)
		}
	}
	c.setSharedLocked(s)
}
