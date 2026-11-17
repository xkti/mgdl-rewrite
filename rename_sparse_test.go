package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSanitizeReasons(t *testing.T) {
	cases := map[string]string{
		"fine.txt":  "",
		"a/b":       "path separator",
		"tab\there": "control characters",
		"":          "empty",
	}
	if runtime.GOOS == "windows" {
		cases["CON.txt"] = "reserved name on Windows"
		cases["a:b"] = "characters Windows doesn't allow"
		cases["dot."] = "ends in a dot or space"
	} else {
		cases[".."] = `"." and ".."`
	}
	for in, want := range cases {
		_, why := sanitize(in)
		if (want == "") != (why == "") || !strings.Contains(why, want) {
			t.Errorf("sanitize(%q) reason %q, want it to mention %q", in, why, want)
		}
	}
}

func TestRenameNotes(t *testing.T) {
	m := newFakeMega(t)
	a := m.addFile("FiLeAaAa", "x.txt", 10, 50, false)
	b := m.addFile("FiLeBbBb", "x.txt", 10, 51, false)     // exact duplicate
	c := m.addFile("FiLeCcCc", "X.TXT", 10, 52, false)     // duplicate ignoring case
	d := m.addFile("FiLeDdDd", "a/b.txt", 10, 53, false)   // separator in a name
	e := m.addFile("FiLeEeEe", "plain.txt", 10, 54, false) // untouched
	link := m.publishFolder("Share", []*fakeFile{a, b, c}, []*fakeFile{d, e})
	l, _ := parseLink(link)
	tr, err := resolve(context.Background(), m.apiClient(), l)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(renameNotes(tr, tr.files), "\n")
	for _, want := range []string{
		"x.txt → x (2).txt  (another item in this folder has the same name)",
		"X.TXT → X (3).TXT  (another item in this folder has the same name, ignoring case)",
		"sub/a/b.txt → a_b.txt  (contains a path separator)",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("notes missing %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "plain.txt") || strings.Count(all, "\n") != 2 {
		t.Errorf("unexpected notes:\n%s", all)
	}
	// Only the selected files are reported.
	sel, _ := tr.selectPaths([]string{"sub/plain.txt"})
	if notes := renameNotes(tr, sel); len(notes) != 0 {
		t.Errorf("notes for an unrenamed selection: %v", notes)
	}
}

func noSparse(t *testing.T) {
	t.Helper()
	old := markSparseFn
	markSparseFn = func(*os.File) error { return errors.New("sparse files not supported") }
	t.Cleanup(func() { markSparseFn = old })
}

// Without sparse support, a multi-chunk file is fetched sequentially (one
// connection, never preallocated) instead of being allocated in full.
func TestNoSparseFallsBackToSequential(t *testing.T) {
	noSparse(t)
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "ns.bin", 4<<20, 55, false)
	out := t.TempDir()
	cfg := m.config(out)
	cfg.connections = 4
	part := filepath.Join(out, "ns.bin.part")
	var sawFull atomic.Bool
	m.hooks(func(total int64) {
		if fi, err := os.Stat(part); err == nil && fi.Size() == int64(len(f.plain)) && total < int64(len(f.plain)) {
			sawFull.Store(true)
		}
	}, time.Millisecond)
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	if sawFull.Load() {
		t.Fatal(".part was preallocated without sparse support")
	}
	if got := m.maxInflight.Load(); got != 1 {
		t.Fatalf("%d concurrent requests, want 1", got)
	}
	mustEqualFile(t, filepath.Join(out, "ns.bin"), f.plain)
}

// The switch is remembered: a resumed download stays sequential.
func TestNoSparseStaysSequentialOnResume(t *testing.T) {
	noSparse(t)
	m := newFakeMega(t)
	size := 4 << 20
	f := m.addFile("FiLe0001", "ns.bin", size, 56, false)
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
	if st := d.Files["FiLe0001"]; st == nil || st.Mode != modeSequential || !st.NoSparse {
		t.Fatalf("state after interrupt: %s", raw)
	}
	// Let the first run's server-side handlers notice their dead connections.
	for deadline := time.Now().Add(5 * time.Second); m.inflight.Load() > 0; {
		if time.Now().After(deadline) {
			t.Fatal("storage handlers from the first run never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.maxInflight.Store(0)
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	if got := m.maxInflight.Load(); got != 1 {
		t.Fatalf("resume used %d concurrent requests, want 1", got)
	}
	mustEqualFile(t, filepath.Join(out, "ns.bin"), f.plain)
}

// Single-chunk files gain nothing from switching, so they don't.
func TestNoSparseSingleChunkStaysParallel(t *testing.T) {
	noSparse(t)
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "small.bin", 500*1024, 57, false)
	out := t.TempDir()
	if _, err := m.run(context.Background(), m.config(out), m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out, "small.bin"), f.plain)
}

// A file that fails before any progress leaves no .part or control file behind.
func TestEmptyPartDiscarded(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "q.bin", 3<<20, 58, false)
	m.linkErrs = []int{-17, -17, -17, -17}
	out := t.TempDir()
	if _, err := m.run(context.Background(), m.config(out), m.fileLink(f)); err == nil {
		t.Fatal("expected failure")
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("left behind: %v", names)
	}

	// But a file with progress keeps its .part for resuming.
	m2 := newFakeMega(t)
	g := m2.addFile("FiLe0002", "r.bin", 3<<20, 59, false)
	m2.dropAfter, m2.drops = 1<<20, 1000 // every request dies after 1 MiB
	out2 := t.TempDir()
	cfg := m2.config(out2)
	cfg.retries = 1
	m2.run(context.Background(), cfg, m2.fileLink(g))
	if !fileExists(filepath.Join(out2, "r.bin.part")) || !fileExists(filepath.Join(out2, ".mgdl-FiLe0002")) {
		t.Fatal("a file with progress lost its .part or state")
	}
}
