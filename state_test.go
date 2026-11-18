package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A save that fails at any step, the final rename included, is tried again on the
// next save, and leaves no temporary file behind.
func TestControlSaveRetriesAfterFailure(t *testing.T) {
	dir := t.TempDir()
	c, err := loadControl(dir, "HaNdLe01")
	if err != nil {
		t.Fatal(err)
	}
	c.put("FiLe0001", &fileState{Size: 1, Mode: modeParallel, Chunks: planChunks(1, 1<<20)})

	// A non-empty folder in the way makes the rename fail.
	if err := os.MkdirAll(filepath.Join(c.path, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.save(); err == nil {
		t.Fatal("save over a folder should fail")
	}
	if fileExists(c.path + ".tmp") {
		t.Fatal("failed save left its temporary file")
	}

	os.RemoveAll(c.path)
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	back, err := loadControl(dir, "HaNdLe01")
	if err != nil || back.get("FiLe0001") == nil {
		t.Fatalf("state not saved on the retry: %v", err)
	}
}
