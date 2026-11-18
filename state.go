package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The control file (.mgdl-HANDLE) records how far every unfinished file of one link
// has got, so an interrupted run resumes where it stopped instead of starting over.
// There is exactly one per link: beside the file for a file link, at the top of the
// downloaded folder for a folder link. It holds, per file:
//
//   - the chunk layout, and how many bytes of each chunk are safely on disk, and
//   - the verification state: the meta-MAC folded so far, plus the per-MAC-chunk
//     MACs of chunks that finished ahead of their predecessors (parallel mode) and
//     so could not be folded yet.
//
// Every recorded "done" offset sits on a MAC chunk boundary, so resuming never needs
// to reread anything from disk: the MAC computation restarts cleanly at that point.
// Data written past the last checkpoint is simply downloaded again.

const (
	ctlVersion      = 1
	ctlSaveInterval = 2 * time.Second
	modeParallel    = "parallel"
	modeSequential  = "sequential"
)

// block16 is a 16-byte value stored as hex.
type block16 [16]byte

func (b block16) MarshalText() ([]byte, error) { return []byte(hex.EncodeToString(b[:])), nil }

func (b *block16) UnmarshalText(t []byte) error {
	raw, err := hex.DecodeString(string(t))
	if err != nil || len(raw) != 16 {
		return fmt.Errorf("bad 16-byte hex value %q", t)
	}
	copy(b[:], raw)
	return nil
}

// chunkState is one download chunk: the byte range [Start, End) of the file.
type chunkState struct {
	Start int64     `json:"s"`
	End   int64     `json:"e"`
	Done  int64     `json:"d"`           // bytes from Start that are on disk (and MACed)
	MACs  []block16 `json:"m,omitempty"` // MAC-chunk MACs over [Start, Start+Done) not yet folded
}

func (c *chunkState) complete() bool { return c.Start+c.Done >= c.End }

// fileState is the resumable state of one file.
type fileState struct {
	Path   string `json:"path"` // for humans reading the file; not used to resume
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
	Verify bool   `json:"verify"` // false if started with --no-verify: cannot verify later
	// NoSparse: the filesystem can't hold sparse files, so the file was switched to
	// sequential mode and must stay there (preallocating would zero-fill it).
	NoSparse bool          `json:"nosparse,omitempty"`
	Chunks   []*chunkState `json:"chunks"`
	Folded   int           `json:"folded"` // chunks [0, Folded) are fully folded into Meta
	Meta     block16       `json:"meta"`   // running meta-MAC
}

// doneBytes is how much of the file is checkpointed.
func (s *fileState) doneBytes() int64 {
	var n int64
	for _, c := range s.Chunks {
		n += c.Done
	}
	return n
}

// contiguous is the end of the checkpointed prefix of the file.
func (s *fileState) contiguous() int64 {
	var pos int64
	for _, c := range s.Chunks {
		pos = c.Start + c.Done
		if !c.complete() {
			break
		}
	}
	return pos
}

func (s *fileState) complete() bool {
	for _, c := range s.Chunks {
		if !c.complete() {
			return false
		}
	}
	return true
}

type ctlData struct {
	Version int                   `json:"version"`
	Link    string                `json:"link"`
	Files   map[string]*fileState `json:"files"`
}

// controlFile owns a link's .mgdl-HANDLE. All fileState mutation happens under mu,
// via update, so the periodic saver always marshals a consistent snapshot.
type controlFile struct {
	path string

	mu      sync.Mutex
	data    ctlData
	dirty   bool
	created bool // the file exists on disk (or will after the next save)
}

func controlPath(dir, handle string) string { return filepath.Join(dir, ".mgdl-"+handle) }

// loadControl reads a link's control file, or starts an empty one in memory. Nothing
// is written until there is state worth saving, so --dry-run and --list leave no trace.
func loadControl(dir, handle string) (*controlFile, error) {
	c := &controlFile{path: controlPath(dir, handle)}
	c.data = ctlData{Version: ctlVersion, Link: handle, Files: map[string]*fileState{}}
	raw, err := os.ReadFile(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading control file: %w", err)
	}
	var d ctlData
	if err := json.Unmarshal(raw, &d); err != nil || d.Version != ctlVersion || d.Files == nil {
		warnf("control file %s is unreadable or from another version; partial downloads will restart", c.path)
		c.created = true
		return c, nil
	}
	c.data = d
	c.created = true
	debugf("state: loaded %s with %d unfinished file(s)", c.path, len(d.Files))
	return c, nil
}

// get returns a copy-free pointer to a file's state; callers mutate it only via update.
func (c *controlFile) get(handle string) *fileState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.Files[handle]
}

// progress reports a file's checkpointed bytes and recorded size, if it has state.
func (c *controlFile) progress(handle string) (done, size int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.data.Files[handle]
	if s == nil {
		return 0, 0, false
	}
	return s.doneBytes(), s.Size, true
}

// put records a new state for a file.
func (c *controlFile) put(handle string, s *fileState) {
	c.mu.Lock()
	c.data.Files[handle] = s
	c.dirty, c.created = true, true
	c.mu.Unlock()
}

// remove forgets a file (finished, or restarting from scratch).
func (c *controlFile) remove(handle string) {
	c.mu.Lock()
	if _, ok := c.data.Files[handle]; ok {
		delete(c.data.Files, handle)
		c.dirty = true
	}
	c.mu.Unlock()
}

// update runs fn under the lock and marks the state dirty.
func (c *controlFile) update(fn func()) {
	c.mu.Lock()
	fn()
	c.dirty = true
	c.mu.Unlock()
}

// save writes the control file atomically if anything changed. Once no unfinished
// files remain, the control file is deleted instead.
func (c *controlFile) save() error {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	c.dirty = false
	empty := len(c.data.Files) == 0
	var raw []byte
	var err error
	if !empty {
		raw, err = json.Marshal(&c.data)
	}
	created := c.created
	c.mu.Unlock()
	if err != nil {
		return err
	}

	if empty {
		if created {
			if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			debugf("state: all files finished, removed %s", c.path)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(c.path, raw); err != nil {
		c.mu.Lock()
		c.dirty = true // try again next tick
		c.mu.Unlock()
		return err
	}
	return nil
}

// writeAtomic replaces path with data via a synced temporary file, so a crash
// leaves either the old file or the new one, never an empty or partial one.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// saveLoop saves every control file periodically until ctx ends. The caller saves
// once more afterwards, so the last checkpoints are kept.
func saveLoop(ctx context.Context, ctls []*controlFile) {
	t := time.NewTicker(ctlSaveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, c := range ctls {
				if err := c.save(); err != nil {
					warnf("saving control file: %v", err)
				}
			}
		}
	}
}

// planChunks splits a file into download chunks of roughly chunkSize bytes. Every
// boundary is moved up to the next MAC chunk boundary, so each chunk's MACs can be
// computed on its own without knowing anything about its neighbours.
func planChunks(size, chunkSize int64) []*chunkState {
	var out []*chunkState
	for start := int64(0); start < size; {
		end := macBoundaryAtOrAfter(start + chunkSize)
		if end > size || size-end < macSteadyStep/2 {
			end = size // don't leave a sliver of a final chunk
		}
		out = append(out, &chunkState{Start: start, End: end})
		start = end
	}
	return out
}
