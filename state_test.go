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

// Older control files don't record the chunk size; it's read back from the layout.
func TestPlannedChunkFromLayout(t *testing.T) {
	for _, c := range []int64{128 << 20, 149 << 20, 512 << 20, 1 << 30} {
		s := &fileState{Chunks: planChunks(3*c+12345, c)}
		if got := s.plannedChunk(); got != c {
			t.Errorf("layout of %s read back as %s", formatSizeArg(c), formatSizeArg(got))
		}
	}
	if got := (&fileState{Chunks: planChunks(100, 128<<20)}).plannedChunk(); got != 0 {
		t.Errorf("one chunk can't tell the size, got %d", got)
	}
	if got := (&fileState{Chunk: 7 << 20, Chunks: planChunks(100, 7<<20)}).plannedChunk(); got != 7<<20 {
		t.Errorf("recorded size ignored, got %d", got)
	}
}

func TestFormatSizeArg(t *testing.T) {
	for n, want := range map[int64]string{149 << 20: "149M", 512 << 20: "512M", 1 << 30: "1G", 3 << 29: "1536M", 1500: "1.5 KiB"} {
		if got := formatSizeArg(n); got != want {
			t.Errorf("formatSizeArg(%d) = %q, want %q", n, got, want)
		}
	}
}
