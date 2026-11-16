//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT turns on ANSI escape processing for the console, which Windows 10+
// supports but leaves off by default. Without it the dashboard would print raw
// escape codes. Failure (old Windows, no console) just leaves it off.
func enableVT() {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) == nil {
			windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
		}
	}
}
