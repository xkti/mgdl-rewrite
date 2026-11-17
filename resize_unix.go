//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize delivers a value whenever the terminal is resized (SIGWINCH).
// Call stop when done.
func watchResize() (<-chan struct{}, func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-sig:
				select {
				case out <- struct{}{}:
				default: // one pending notice is enough
				}
			}
		}
	}()
	return out, func() { signal.Stop(sig); close(done) }
}
