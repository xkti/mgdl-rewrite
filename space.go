package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Free-space checking. Files are written into sparse .part files, which take up
// disk space only as data arrives, so without a check a download would start
// happily and fail with ENOSPC hours in. Two checks run:
//
//   - up front, for the whole selection of a link: if it can't fit, nothing starts;
//   - as each file starts, since space can change mid-run (other programs, other
//     links): free space must cover this file plus what the files already in
//     flight still have to write.
//
// "Need" is always what is still to be written: files already complete count
// nothing, and a resumable file counts only what is past its checkpoint.

// freeSpaceFn reports the free bytes on the filesystem holding a directory.
// A variable so tests can simulate a full disk.
var freeSpaceFn = diskFree

var errNoSpace = errors.New("not enough disk space")

// spaceMargin is headroom on top of the data itself, for filesystem metadata and
// the control file: 1 MiB plus 0.5%.
func spaceMargin(need int64) int64 {
	return 1<<20 + need/200
}

// existingDir returns dir, or its nearest ancestor that exists, which is on the
// filesystem the download will land on (the output directory may not exist yet).
func existingDir(dir string) string {
	d, err := filepath.Abs(dir)
	if err != nil {
		d = filepath.Clean(dir)
	}
	for {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return d
		}
		d = parent
	}
}

// fileNeed is how many bytes a file still has to put on disk.
func fileNeed(cfg *config, t *tree, ctl *controlFile, n *node) int64 {
	final, _, err := finalPath(cfg, t, n)
	if err != nil {
		return 0 // can't be placed; it fails when it comes up, writing nothing
	}
	return fileNeedAt(final, ctl, n)
}

// fileNeedAt is fileNeed for a file going to final.
func fileNeedAt(final string, ctl *controlFile, n *node) int64 {
	if fi, err := os.Stat(final); err == nil && !fi.IsDir() && fi.Size() == n.size {
		return 0 // complete (a failed --verify-existing deletes it first, so still 0 net)
	}
	if done, size, ok := ctl.progress(n.handle); ok && size == n.size && fileExists(final+".part") {
		return n.size - done
	}
	return n.size
}

// spaceReport is the up-front check for one link.
type spaceReport struct {
	dir  string
	need int64
	free int64
	err  error // free space unknown
}

func checkLinkSpace(cfg *config, t *tree, ctl *controlFile, files []*node) spaceReport {
	r := spaceReport{dir: existingDir(t.baseDir(cfg.output))}
	for _, n := range files {
		r.need += fileNeed(cfg, t, ctl, n)
	}
	r.free, r.err = freeSpaceFn(r.dir)
	return r
}

// short reports whether the selection doesn't fit.
func (r spaceReport) short() bool {
	return r.err == nil && r.need > 0 && r.free < r.need+spaceMargin(r.need)
}

func (r spaceReport) asError() error {
	return fmt.Errorf("%w in %s: %s to download, %s free (%s short, including %s headroom).\n"+
		"Free up space, choose another --output, or download less with --path or --select. "+
		"--no-space-check skips this check",
		errNoSpace, r.dir, formatBytes(r.need), formatBytes(r.free),
		formatBytes(r.need+spaceMargin(r.need)-r.free), formatBytes(spaceMargin(r.need)))
}

// spaceGuard re-checks free space as each file starts. A nil guard checks nothing.
type spaceGuard struct {
	dir    string
	mu     sync.Mutex
	active map[*job]struct{}
}

func newSpaceGuard(dir string) *spaceGuard {
	return &spaceGuard{dir: dir, active: map[*job]struct{}{}}
}

// claim checks that need bytes fit alongside what the files already in flight
// still have to write, and registers the job as in flight.
func (g *spaceGuard) claim(j *job, need int64) error {
	j.spaceNeed.Store(need)
	j.spaceWritten.Store(0)
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var others int64
	for o := range g.active {
		others += max(0, o.spaceNeed.Load()-o.spaceWritten.Load())
	}
	if need > 0 {
		free, err := freeSpaceFn(g.dir)
		if err == nil && free < need+others+spaceMargin(need+others) {
			return &localError{fmt.Errorf("%w for this file: it needs %s, %s is free and other downloads in progress still need %s",
				errNoSpace, formatBytes(need), formatBytes(free), formatBytes(others))}
		}
	}
	g.active[j] = struct{}{}
	return nil
}

// release forgets a job once it has finished, failed or stopped.
func (g *spaceGuard) release(j *job) {
	if g == nil {
		return
	}
	g.mu.Lock()
	delete(g.active, j)
	g.mu.Unlock()
}
