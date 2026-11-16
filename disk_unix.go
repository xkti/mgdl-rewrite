//go:build linux || darwin || freebsd

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// diskFree returns the bytes available to this (unprivileged) user on the
// filesystem holding dir. Bavail excludes blocks reserved for root.
func diskFree(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// markSparse is a no-op: on Unix filesystems, extending a file with Truncate
// already leaves the gap unallocated.
func markSparse(*os.File) error { return nil }
