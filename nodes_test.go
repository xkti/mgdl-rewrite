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
	if !strings.Contains(b.String(), "1 folder(s), 1 file(s)") {
		t.Errorf("folder link listing:\n%s", b.String())
	}
	b.Reset()
	printTree(&b, &tree{root: f, files: []*node{f}}, false)
	if strings.Contains(b.String(), "folder(s)") || strings.Count(b.String(), "\n") != 1 {
		t.Errorf("file link listing:\n%s", b.String())
	}
}
