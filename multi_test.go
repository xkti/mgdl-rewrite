package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Files of different links download at the same time: with one connection per
// file, two storage requests in flight at once can only come from two links.
func TestLinksDownloadTogether(t *testing.T) {
	m := newFakeMega(t)
	a := m.addFile("FiLeAaAa", "a.bin", 600<<10, 1, false)
	b := m.addFile("FiLeBbBb", "b.bin", 600<<10, 2, false)
	m.hooks(nil, 2*time.Millisecond)
	out := t.TempDir()
	cfg := m.config(out)
	cfg.connections = 1

	st, runs, err := m.runAll(context.Background(), cfg, m.fileLink(a), m.fileLink(b))
	if err != nil {
		t.Fatal(err)
	}
	if st.completed != 2 || runs[0].status != statusDone || runs[1].status != statusDone {
		t.Fatalf("stats %+v, statuses %v %v", st, runs[0].status, runs[1].status)
	}
	if got := m.maxInflight.Load(); got != 2 {
		t.Fatalf("at most %d request(s) in flight: the links ran one after another", got)
	}
	mustEqualFile(t, filepath.Join(out, "a.bin"), a.plain)
	mustEqualFile(t, filepath.Join(out, "b.bin"), b.plain)
}

// Two folder links with the same name merge into one folder; a file name both
// have goes to the earlier link, and the later link's copy gets its handle.
func TestSameNamedFoldersMerge(t *testing.T) {
	m := newFakeMega(t)
	a1 := m.addFile("FiLeA1a1", "a.txt", 100, 3, false)
	b1 := m.addFile("FiLeB1b1", "one.bin", 300, 4, false)
	a2 := m.addFile("FiLeA2a2", "A.TXT", 200, 5, false) // clashes ignoring case
	b2 := m.addFile("FiLeB2b2", "two.bin", 400, 6, false)
	l1 := m.publishFolder("Pack", []*fakeFile{a1}, []*fakeFile{b1})
	l2 := m.publishShare("ShArE002", "RoOt0002", "SuBd0002", "Pack", []*fakeFile{a2}, []*fakeFile{b2})
	out := t.TempDir()

	st, runs, err := m.runAll(context.Background(), m.config(out), l1, l2)
	if err != nil {
		t.Fatal(err)
	}
	if st.completed != 4 {
		t.Fatalf("stats %+v", st)
	}
	mustEqualFile(t, filepath.Join(out, "Pack", "a.txt"), a1.plain)
	mustEqualFile(t, filepath.Join(out, "Pack", "A_FiLeA2a2.TXT"), a2.plain)
	mustEqualFile(t, filepath.Join(out, "Pack", "sub", "one.bin"), b1.plain)
	mustEqualFile(t, filepath.Join(out, "Pack", "sub", "two.bin"), b2.plain)

	var dups []string
	for _, lr := range runs {
		for _, f := range lr.tree.files {
			if f.dup {
				dups = append(dups, f.relPath)
			}
		}
	}
	if strings.Join(dups, ",") != "a.txt,A_FiLeA2a2.TXT" {
		t.Fatalf("duplicates marked: %v", dups)
	}
	if !strings.Contains(strings.Join(renameNotes(runs[1].tree, runs[1].files), "\n"), "Pack/A.TXT → A_FiLeA2a2.TXT  (duplicate") {
		t.Fatalf("notes: %q", renameNotes(runs[1].tree, runs[1].files))
	}
	report := strings.Join(linkReport(runs), "\n")
	if !strings.Contains(report, "Links: 2 done") || !strings.Contains(report, "Pack (2 links)  done") {
		t.Fatalf("report:\n%s", report)
	}
}

// Two file links with the same name: both are kept.
func TestSameNamedFilesKeepBoth(t *testing.T) {
	m := newFakeMega(t)
	a := m.addFile("FiLeAaAa", "x.bin", 100, 7, false)
	b := m.addFile("FiLeBbBb", "x.bin", 200, 8, false)
	out := t.TempDir()
	if _, _, err := m.runAll(context.Background(), m.config(out), m.fileLink(a), m.fileLink(b)); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out, "x.bin"), a.plain)
	mustEqualFile(t, filepath.Join(out, "x_FiLeBbBb.bin"), b.plain)
}

// The same link twice runs once; a broken link is reported and the others run.
func TestRepeatedAndBrokenLinks(t *testing.T) {
	m := newFakeMega(t)
	a := m.addFile("FiLeAaAa", "a.bin", 100, 9, false)
	out := t.TempDir()
	bad := "https://mega.nz/file/FiLeAaAa#tooshort"
	st, runs, err := m.runAll(context.Background(), m.config(out), m.fileLink(a), m.fileLink(a), bad)
	if err == nil {
		t.Fatal("a broken link should make the run fail")
	}
	if st.completed != 1 {
		t.Fatalf("stats %+v", st)
	}
	want := []linkStatus{statusDone, statusSkipped, statusFailed}
	for i, lr := range runs {
		if lr.status != want[i] {
			t.Fatalf("link %d: status %v, want %v", i+1, lr.status, want[i])
		}
	}
	report := linkReport(runs)
	if report[0] != "Links: 1 done, 1 couldn't be opened, 1 skipped" {
		t.Fatalf("report header %q", report[0])
	}
	if !strings.Contains(report[1], "a.bin (2 links)") || !strings.Contains(report[1], "1 done, 1 skipped") {
		t.Fatalf("report line %q", report[1])
	}
	// A reason shared by every link in a group is shown; mixed ones aren't.
	same := []*linkRun{{status: statusSkipped, note: "selection cancelled"}, {status: statusSkipped, note: "selection cancelled"}}
	if got := groupOutcome(same); got != "skipped (selection cancelled)" {
		t.Fatalf("shared reason: %q", got)
	}
	same[1].note = "nothing selected"
	if got := groupOutcome(same); got != "skipped" {
		t.Fatalf("mixed reasons: %q", got)
	}
	if !strings.Contains(report[2], "link 3") || !strings.Contains(report[2], "couldn't be opened: ") {
		t.Fatalf("report line %q", report[2])
	}
}

// A failed file marks its link partly failed; other links are unaffected.
func TestPartialLinkInReport(t *testing.T) {
	m := newFakeMega(t)
	good := m.addFile("FiLeAaAa", "good.bin", 100, 10, false)
	bad := m.addFile("FiLeBbBb", "bad.bin", 100, 11, true) // wrong MAC every time
	cfg := m.config(t.TempDir())
	cfg.retries = 2
	_, runs, err := m.runAll(context.Background(), cfg, m.fileLink(good), m.fileLink(bad))
	if err == nil || runs[0].status != statusDone || runs[1].status != statusPartial {
		t.Fatalf("err %v, statuses %v %v", err, runs[0].status, runs[1].status)
	}
	if line := linkReport(runs)[2]; !strings.Contains(line, "1 of 1 files failed") {
		t.Fatalf("report line %q", line)
	}
}

// Not enough space for all links together stops them all before any request.
func TestSpaceCheckedForAllLinks(t *testing.T) {
	m := newFakeMega(t)
	a := m.addFile("FiLeAaAa", "a.bin", 3<<20, 12, false)
	b := m.addFile("FiLeBbBb", "b.bin", 3<<20, 13, false)
	fakeFree(t, fixedFree(5<<20)) // enough for either, not both
	_, runs, err := m.runAll(context.Background(), m.config(t.TempDir()), m.fileLink(a), m.fileLink(b))
	if !errors.Is(err, errNoSpace) {
		t.Fatalf("err %v", err)
	}
	if runs[0].status != statusNotStarted || runs[1].status != statusNotStarted || m.requests.Load() != 0 {
		t.Fatalf("statuses %v %v, %d requests", runs[0].status, runs[1].status, m.requests.Load())
	}
}

func TestMergeTreesClashes(t *testing.T) {
	file := func(h, name string) *node { return &node{handle: h, origName: name, name: name} }
	dir := func(h, name string, kids ...*node) *node {
		d := &node{handle: h, isDir: true, origName: name, name: name, children: kids}
		for _, k := range kids {
			k.parent = d
		}
		return d
	}
	mk := func(root *node) *tree {
		t := &tree{root: root}
		if root.isDir {
			finishTree(root, &t.files)
		} else {
			root.relPath = root.name
			t.files = []*node{root}
		}
		return t
	}
	// Link 1: folder "Set" with x and sub/; link 2: folder "set" whose x is a
	// folder, and which already has a file called "x_HhHh2222"; link 3: a file "Set".
	t1 := mk(dir("HhHh1111", "Set", file("FfFf1111", "x"), dir("DdDd1111", "sub", file("FfFf1112", "y"))))
	x2 := dir("HhHh2222", "x", file("FfFf2221", "inner"))
	t2 := mk(dir("RrRr2222", "set", x2, file("FfFf2222", "x_HhHh2222"), dir("DdDd2222", "sub", file("FfFf2223", "y"))))
	t3 := mk(file("FfFf3333", "Set"))
	mergeTrees([]*tree{t1, t2, t3})

	child := func(d *node, orig string) *node {
		for _, c := range d.children {
			if c.origName == orig {
				return c
			}
		}
		t.Fatalf("%s has no %s", d.origName, orig)
		return nil
	}
	if x2.name != "x_HhHh2222_HhHh2222" {
		t.Fatalf("folder x from link 2 named %q", x2.name)
	}
	if got := x2.children[0].relPath; got != "x_HhHh2222_HhHh2222/inner" {
		t.Fatalf("child path not updated: %q", got)
	}
	if child(t2.root, "x_HhHh2222").name != "x_HhHh2222" {
		t.Fatal("a name the link already had was taken by a generated one")
	}
	y1, y2 := child(child(t1.root, "sub"), "y"), child(child(t2.root, "sub"), "y")
	if y2.name != "y_FfFf2223" || y2.relPath != "sub/y_FfFf2223" || !y2.dup || !y1.dup {
		t.Fatalf("clash inside a merged subfolder: %q dup=%v", y2.name, y2.dup)
	}
	if t3.root.name != "Set_FfFf3333" || t3.root.relPath != "Set_FfFf3333" {
		t.Fatalf("file clashing with a folder: %q", t3.root.name)
	}
	if child(t1.root, "x").name != "x" || t2.root.name != "set" {
		t.Fatal("the earlier link's names changed")
	}
}

// A dry run of several links reports space once, for all of them together, and
// writes nothing.
func TestDryRunSpaceForAllLinks(t *testing.T) {
	m := newFakeMega(t)
	a := m.addFile("FiLeAaAa", "a.bin", 3<<20, 14, false)
	b := m.addFile("FiLeBbBb", "b.bin", 3<<20, 15, false)
	fakeFree(t, fixedFree(5<<20)) // enough for either, not both
	out := t.TempDir()
	cfg := m.config(out)
	cfg.dryRun = true
	stdout := captureStdout(t, func() {
		if _, _, err := m.runAll(context.Background(), cfg, m.fileLink(a), m.fileLink(b)); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Count(stdout, "NOT ENOUGH SPACE") != 1 || !strings.Contains(stdout, "a real run wouldn't start 2 link(s)") ||
		!strings.Contains(stdout, "All links: 2 file(s) from 2 link(s)") {
		t.Fatalf("output:\n%s", stdout)
	}
	if m.requests.Load() != 0 || fileExists(filepath.Join(out, ".mgdl-FiLeAaAa")) {
		t.Fatal("a dry run transferred or wrote something")
	}
}

// captureStdout runs fn with stdout going to a pipe, and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	got := make(chan []byte)
	go func() { b, _ := io.ReadAll(r); got <- b }()
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	return string(<-got)
}
