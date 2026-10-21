// Command scrolltest is a throwaway check for the oob console bar: does
// output scrolled inside a DECSTBM region (rows 1..H-1) still reach this
// terminal's native scrollback, with a fake bar held on the last row?
//
// Run it in the terminal you care about (not inside tmux/screen unless that
// is what you want to test):
//
//	go run ./hack/scrolltest
//
// It prints 500 numbered lines, waits for Enter, then restores the terminal.
// After it exits, scroll up with your terminal's own scrollback (mouse wheel
// or Shift+PageUp) and look for lines 1..500. Use -n to change the line count.
package main

import (
	"flag"
	"fmt"
	"os"

	"golang.org/x/term"
)

func main() {
	n := flag.Int("n", 500, "lines to print")
	flag.Parse()
	fd := int(os.Stdout.Fd())
	w, h, err := term.GetSize(fd)
	if err != nil || h < 5 {
		fmt.Fprintln(os.Stderr, "scrolltest: needs a terminal at least 5 rows tall")
		os.Exit(1)
	}
	// Same reservation sequence as the console: free the last row, set the
	// region, paint the bar with the cursor saved and restored.
	bar := func(msg string) {
		if len(msg) > w-1 {
			msg = msg[:w-1]
		}
		fmt.Printf("\x1b7\x1b[%d;1H\x1b[?7l\x1b[7m%-*s\x1b[0m\x1b[?7h\x1b8", h, w, " "+msg)
	}
	restore := func() { fmt.Printf("\x1b[0m\x1b7\x1b[r\x1b[%d;1H\x1b[2K\x1b8\r\n", h) }
	defer restore()

	fmt.Print("\n\x1b[A\x1b7", fmt.Sprintf("\x1b[1;%dr", h-1), "\x1b8")
	bar("scrolltest: fake bar on the last row")
	for i := 1; i <= *n; i++ {
		fmt.Printf("line %d\r\n", i)
		if i%50 == 0 {
			bar(fmt.Sprintf("scrolltest: printed %d of %d", i, *n))
		}
	}
	bar("done: press Enter to restore the terminal")
	fmt.Print("\r\nPress Enter to restore. Then scroll up: lines 1.." + fmt.Sprint(*n) + " should be in native scrollback.")
	_, _ = fmt.Fscanln(os.Stdin)
}
