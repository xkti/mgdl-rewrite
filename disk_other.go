//go:build !linux && !darwin && !freebsd && !windows

package main

import (
	"errors"
	"os"
)

// diskFree is not implemented on this platform; the space check is skipped.
func diskFree(string) (int64, error) {
	return 0, errors.New("free space can't be determined on this platform")
}

func markSparse(*os.File) error { return nil }
