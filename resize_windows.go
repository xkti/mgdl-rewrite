//go:build windows

package main

import "time"

// watchResize delivers a value whenever the console width changes. Windows has no
// resize signal for console programs, so the width is polled.
func watchResize() (<-chan struct{}, func()) {
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		last := terminalWidth()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if w := terminalWidth(); w != last {
					last = w
					select {
					case out <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	return out, func() { close(done) }
}
