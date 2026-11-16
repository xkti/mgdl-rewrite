//go:build !windows

package main

// enableVT is a no-op: Unix terminals interpret ANSI escapes natively.
func enableVT() {}
