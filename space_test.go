package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFree replaces the free-space query for one test.
func fakeFree(t *testing.T, fn func(dir string) (int64, error)) {
	t.Helper()
	old := freeSpaceFn
	freeSpaceFn = fn
	t.Cleanup(func() { freeSpaceFn = old })
}

func fixedFree(n int64) func(string) (int64, error) {
	return func(string) (int64, error) { return n, nil }
}

func TestSpaceAbortsBeforeStarting(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "big.bin", 4<<20, 40, false)
	fakeFree(t, fixedFree(3<<20))
	out := t.TempDir()
	_, err := m.run(context.Background(), m.config(out), m.fileLink(f))
	if !errors.Is(err, errNoSpace) {
		t.Fatalf("want errNoSpace, got %v", err)
	}
	if m.requests.Load() != 0 || m.linkReqs.Load() != 0 {
		t.Fatal("download started despite insufficient space")
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Fatalf("wrote to disk: %v", entries)
	}
}

func TestSpaceCountsOnlyWhatIsLeft(t *testing.T) {
	m := newFakeMega(t)
	size := 6 << 20
	f := m.addFile("FiLe0001", "big.bin", size, 41, false)
	out := t.TempDir()
	cfg := m.config(out)

	ctx, cancel := context.WithCancel(context.Background())
	m.hooks(func(total int64) {
		if total > int64(size)/2 {
			cancel()
		}
	}, 2*time.Millisecond)
	m.run(ctx, cfg, m.fileLink(f))
	m.hooks(nil, 0)

	raw, err := os.ReadFile(filepath.Join(out, ".mgdl-FiLe0001"))
	if err != nil {
		t.Fatal(err)
	}
	var d ctlData
	json.Unmarshal(raw, &d)
	left := int64(size) - d.Files["FiLe0001"].doneBytes()

	// Enough for what's left, not for the whole file.
	fakeFree(t, fixedFree(left+spaceMargin(left)+64<<10))
	if left+spaceMargin(left)+64<<10 >= int64(size) {
		t.Fatalf("test needs a larger resume gap (left %d of %d)", left, size)
	}
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out, "big.bin"), f.plain)

	// Already complete: needs nothing, even on a full disk.
	fakeFree(t, fixedFree(0))
	if st, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil || st.skipped != 1 {
		t.Fatalf("complete file on a full disk: %+v %v", st, err)
	}
}

func TestNoSpaceCheckFlag(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "x.bin", 1<<20, 42, false)
	fakeFree(t, fixedFree(0))
	out := t.TempDir()
	cfg := m.config(out)
	cfg.noSpaceCheck = true
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out, "x.bin"), f.plain)
}

func TestUnknownFreeSpaceDoesNotBlock(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "x.bin", 1<<20, 43, false)
	fakeFree(t, func(string) (int64, error) { return 0, errors.New("unsupported") })
	out := t.TempDir()
	if _, err := m.run(context.Background(), m.config(out), m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out, "x.bin"), f.plain)
}

// Space that disappears mid-run fails the next file at its start, without retries,
// while the file already in progress finishes.
func TestSpaceRecheckedPerFile(t *testing.T) {
	m := newFakeMega(t)
	a := m.addFile("FiLeAaAa", "a.bin", 2<<20, 44, false)
	b := m.addFile("FiLeBbBb", "b.bin", 2<<20, 45, false)
	link := m.publishFolder("Share", []*fakeFile{a, b}, nil)
	var calls atomic.Int64
	fakeFree(t, func(string) (int64, error) {
		if calls.Add(1) <= 2 { // up-front check and the first file's start
			return 1 << 30, nil
		}
		return 1 << 20, nil // then something fills the disk
	})
	out := t.TempDir()
	cfg := m.config(out)
	cfg.jobs = 1
	st, err := m.run(context.Background(), cfg, link)
	if err == nil || st.completed != 1 || st.failed != 1 {
		t.Fatalf("want 1 done + 1 failed, got %+v %v", st, err)
	}
	mustEqualFile(t, filepath.Join(out, "Share", "a.bin"), a.plain)
	if fileExists(filepath.Join(out, "Share", "b.bin.part")) {
		t.Fatal("b.bin was started")
	}
	if got := m.requests.Load(); got != int64(len(planChunks(2<<20, cfg.chunkSize))) {
		t.Fatalf("%d storage requests: the second file was attempted or retried", got)
	}
}

func TestSpaceGuardCountsFilesInFlight(t *testing.T) {
	fakeFree(t, fixedFree(10<<20))
	g := newSpaceGuard("/")
	j1, j2 := &job{}, &job{}
	if err := g.claim(j1, 6<<20); err != nil {
		t.Fatal(err)
	}
	if err := g.claim(j2, 6<<20); !errors.Is(err, errNoSpace) {
		t.Fatalf("6+6 MiB in 10 MiB should not fit, got %v", err)
	}
	var le *localError
	if err := g.claim(j2, 6<<20); !errors.As(err, &le) {
		t.Fatal("a space failure should be a local (non-retried) error")
	}
	j1.spaceWritten.Store(4 << 20) // j1 is now mostly on disk: 2 MiB left to write
	if err := g.claim(j2, 6<<20); err != nil {
		t.Fatalf("2+6 MiB in 10 MiB should fit: %v", err)
	}
	g.release(j1)
	g.release(j2)
	if len(g.active) != 0 {
		t.Fatal("release left jobs behind")
	}
	var nilGuard *spaceGuard
	if err := nilGuard.claim(&job{}, 1<<40); err != nil {
		t.Fatal("a nil guard must not check")
	}
}

func TestSpaceReport(t *testing.T) {
	if (spaceReport{need: 0, free: 0}).short() {
		t.Fatal("nothing to download is never short")
	}
	if !(spaceReport{need: 10 << 20, free: 10 << 20}).short() {
		t.Fatal("no headroom should be short")
	}
	if (spaceReport{need: 10 << 20, free: 20 << 20}).short() {
		t.Fatal("plenty of room is not short")
	}
	if (spaceReport{need: 10 << 20, err: errors.New("x")}).short() {
		t.Fatal("unknown free space is not short")
	}
}

func TestExistingDir(t *testing.T) {
	base := t.TempDir()
	if got := existingDir(filepath.Join(base, "a", "b", "c")); got != base {
		t.Fatalf("got %q, want %q", got, base)
	}
	if got := existingDir(base); got != base {
		t.Fatalf("got %q", got)
	}
}

// The real query works on this machine's temp directory.
func TestDiskFreeReal(t *testing.T) {
	n, err := diskFree(t.TempDir())
	if err != nil || n <= 0 {
		t.Fatalf("diskFree: %d, %v", n, err)
	}
}
