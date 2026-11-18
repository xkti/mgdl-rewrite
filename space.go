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
	if done, ok := resumable(ctl, n, final); ok {
		return n.size - done
	}
	return n.size
}

// resumable reports whether a run would resume a file going to final, and from
// how far: its saved state is for the file's current size and its .part is there.
func resumable(ctl *controlFile, n *node, final string) (done int64, ok bool) {
	done, size, ok := ctl.progress(n.handle)
	if !ok || size != n.size || !fileExists(final+".part") {
		return 0, false
	}
	return done, true
}

// spaceReport is the up-front check.
type spaceReport struct {
	dir  string
	need int64
	free int64
	err  error // free space unknown
}

// spaceFor adds up what the links' selected files still need, against the free
// space under --output. Every link lands there, so one figure covers them all (a
// link's own folder wouldn't: once some links' folders exist and others' don't,
// links on the same disk would be checked, and guarded, apart).
func spaceFor(cfg *config, runs []*linkRun) spaceReport {
	r := spaceReport{dir: existingDir(cfg.output)}
	for _, lr := range runs {
		for _, n := range lr.files {
			r.need += fileNeed(cfg, lr.tree, lr.ctl, n)
		}
	}
	r.free, r.err = freeSpaceFn(r.dir)
	return r
}

// short reports whether the selection doesn't fit.
func (r spaceReport) short() bool {
	return r.err == nil && r.need > 0 && r.free < r.need+spaceMargin(r.need)
}

// shortfall says by how much a selection doesn't fit, after "not enough disk
// space"; a real run and a dry run word it the same.
func (r spaceReport) shortfall() string {
	return fmt.Sprintf("in %s: %s to download, %s free (%s short, including %s headroom)",
		r.dir, formatBytes(r.need), formatBytes(r.free),
		formatBytes(r.need+spaceMargin(r.need)-r.free), formatBytes(spaceMargin(r.need)))
}

func (r spaceReport) asError() error {
	return fmt.Errorf("%w %s.\n"+
		"Free up space, choose another --output, or download less with --path or --select. "+
		"--no-space-check skips this check",
		errNoSpace, r.shortfall())
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
