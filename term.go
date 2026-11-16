package main

import (
	"os"
	"strconv"

	"golang.org/x/term"
)

// isTerminal reports whether f is an interactive terminal, so the dashboard knows
// whether cursor movement is safe. TERM=dumb opts out.
func isTerminal(f *os.File) bool {
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// terminalWidth asks the terminal for its width (re-read every frame, so resizing
// works), falling back to COLUMNS and then 80.
func terminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w >= 20 {
		return w
	}
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 20 {
			return n
		}
	}
	return 80
}
