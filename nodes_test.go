package main

import (
	"strings"
	"testing"
)

// The listing's folder count includes the shared folder itself, so a folder
// link without subfolders reports one folder. A file link has no summary.
func TestPrintTreeCountsRootFolder(t *testing.T) {
	f := &node{handle: "FFFFFFFF", origName: "a.txt", size: 3}
	root := &node{handle: "RRRRRRRR", isDir: true, origName: "share", children: []*node{f}}
	var b strings.Builder
	printTree(&b, &tree{root: root, files: []*node{f}}, false)
	if !strings.Contains(b.String(), "1 folder, 1 file") {
		t.Errorf("folder link listing:\n%s", b.String())
	}
	b.Reset()
	printTree(&b, &tree{root: f, files: []*node{f}}, false)
	if strings.Contains(b.String(), "folder") || strings.Count(b.String(), "\n") != 1 {
		t.Errorf("file link listing:\n%s", b.String())
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int]string{0: "0 files", 1: "1 file", 2: "2 files"} {
		if got := plural(n, "file"); got != want {
			t.Errorf("plural(%d) = %q, want %q", n, got, want)
		}
	}
}
