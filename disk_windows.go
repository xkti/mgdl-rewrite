//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// diskFree returns the bytes available to the current user on the volume holding
// dir (which honours per-user quotas).
func diskFree(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return int64(avail), nil
}

// markSparse flags a file as sparse on NTFS. Without it, extending the file with
// Truncate allocates the whole size, and a write far into the file makes Windows
// zero-fill everything before it first, which a parallel download does for every
// chunk but the first. Filesystems without sparse support (FAT32, exFAT) refuse;
// the caller treats that as harmless.
func markSparse(f *os.File) error {
	var ret uint32
	return windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &ret, nil)
}
