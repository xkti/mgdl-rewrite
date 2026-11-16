package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMega is a local stand-in for the MEGA API, its storage servers and the proxy.
// Every key and handle is a synthetic placeholder; nothing touches the network.
type fakeMega struct {
	t       *testing.T
	api     *httptest.Server
	storage *httptest.Server
	proxy   *httptest.Server

	files  map[string]*fakeFile // by handle
	folder string               // share handle, if a folder is published
	master []byte
	nodes  []rawNode

	mu         sync.Mutex
	dropAfter  int64 // drop a storage response after this many bytes…
	drops      int   // …for this many responses
	throttles  int   // answer this many storage requests with 509
	served     atomic.Int64
	requests   atomic.Int64
	linkCalls  atomic.Int64
	proxied    atomic.Int64
	onServe    func(total int64) // called as bytes go out (to simulate Ctrl+C)
	slowPerBuf time.Duration
}

type fakeFile struct {
	handle string
	name   string
	plain  []byte
	enc    []byte
	key    []byte // 32-byte node key
}

func newFakeMega(t *testing.T) *fakeMega {
	m := &fakeMega{t: t, files: map[string]*fakeFile{}}
	m.api = httptest.NewServer(http.HandlerFunc(m.serveAPI))
	m.storage = httptest.NewServer(http.HandlerFunc(m.serveStorage))
	m.proxy = httptest.NewServer(http.HandlerFunc(m.serveProxy))
	t.Cleanup(func() { m.api.Close(); m.storage.Close(); m.proxy.Close() })
	return m
}

// addFile creates a random file, encrypts it, and computes the key MEGA would store
// (including the condensed MAC). If badMAC, the stored MAC is wrong.
func (m *fakeMega) addFile(handle, name string, size int, seed int64, badMAC bool) *fakeFile {
	rng := rand.New(rand.NewSource(seed))
	plain := make([]byte, size)
	rng.Read(plain)
	var aesKey [16]byte
	var nonce [8]byte
	rng.Read(aesKey[:])
	rng.Read(nonce[:])

	block, _ := aes.NewCipher(aesKey[:])
	enc := make([]byte, size)
	cipher.NewCTR(block, ctrIV(nonce, 0)).XORKeyStream(enc, plain)
	mac := condense(refMeta(aesKey, nonce, plain))
	if badMAC {
		mac[0] ^= 0xff
	}
	key := make([]byte, 32)
	copy(key[16:24], nonce[:])
	copy(key[24:32], mac[:])
	for i := 0; i < 16; i++ {
		key[i] = aesKey[i] ^ key[i+16]
	}
	f := &fakeFile{handle: handle, name: name, plain: plain, enc: enc, key: key}
	m.files[handle] = f
	return f
}

func (m *fakeMega) fileLink(f *fakeFile) string {
	return "https://mega.nz/file/" + f.handle + "#" + b64enc(f.key)
}

// publishFolder arranges files into a share: root/ with the given files at the
// top level and in sub/.
func (m *fakeMega) publishFolder(root string, top []*fakeFile, sub []*fakeFile) string {
	m.folder = "FoLdEr01"
	m.master = bytes.Repeat([]byte{0x42}, 16)
	wrap := func(k []byte) string {
		b, _ := aes.NewCipher(m.master)
		out := make([]byte, len(k))
		for i := 0; i < len(k); i += 16 {
			b.Encrypt(out[i:i+16], k[i:i+16])
		}
		return b64enc(out)
	}
	dirKey := func(seed byte) []byte { return bytes.Repeat([]byte{seed}, 16) }
	m.nodes = append(m.nodes,
		rawNode{Handle: "RoOtNoDe", Parent: "OuTsIdE0", Type: 1, Attr: encryptAttrs(dirKey(1), root), Key: "RoOtNoDe:" + wrap(dirKey(1))},
		rawNode{Handle: "SuBdIr01", Parent: "RoOtNoDe", Type: 1, Attr: encryptAttrs(dirKey(2), "sub"), Key: "RoOtNoDe:" + wrap(dirKey(2))},
	)
	add := func(f *fakeFile, parent string) {
		fk, _ := unpackFileKey(f.key)
		m.nodes = append(m.nodes, rawNode{Handle: f.handle, Parent: parent, Type: 0, Size: int64(len(f.plain)),
			Attr: encryptAttrs(fk.aes[:], f.name),
			// An unrelated share's key first, as real listings sometimes have.
			Key: "XxXxXxXx:" + b64enc(bytes.Repeat([]byte{9}, 32)) + "/RoOtNoDe:" + wrap(f.key)})
	}
	for _, f := range top {
		add(f, "RoOtNoDe")
	}
	for _, f := range sub {
		add(f, "SuBdIr01")
	}
	return "https://mega.nz/folder/" + m.folder + "#" + b64enc(m.master)
}

func (m *fakeMega) serveAPI(w http.ResponseWriter, r *http.Request) {
	var cmds []map[string]any
	if err := json.NewDecoder(r.Body).Decode(&cmds); err != nil || len(cmds) != 1 {
		w.Write([]byte("-2"))
		return
	}
	cmd := cmds[0]
	switch cmd["a"] {
	case "g":
		h, _ := cmd["p"].(string)
		if n, ok := cmd["n"].(string); ok {
			if r.URL.Query().Get("n") != m.folder {
				w.Write([]byte("[-11]"))
				return
			}
			h = n
		}
		f := m.files[h]
		if f == nil {
			w.Write([]byte("[-9]"))
			return
		}
		fk, _ := unpackFileKey(f.key)
		res := map[string]any{"s": len(f.plain), "at": encryptAttrs(fk.aes[:], f.name)}
		if cmd["g"] != nil {
			m.linkCalls.Add(1)
			res["g"] = m.storage.URL + "/dl/" + h
		}
		json.NewEncoder(w).Encode([]any{res})
	case "f":
		if r.URL.Query().Get("n") != m.folder {
			w.Write([]byte("[-9]"))
			return
		}
		json.NewEncoder(w).Encode([]any{map[string]any{"f": m.nodes}})
	default:
		w.Write([]byte("[-2]"))
	}
}

// serveStorage answers GET /dl/HANDLE/START-END, MEGA's byte-range URL suffix.
func (m *fakeMega) serveStorage(w http.ResponseWriter, r *http.Request) {
	m.requests.Add(1)
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/dl/"), "/")
	f := m.files[parts[0]]
	if f == nil || len(parts) != 2 {
		http.Error(w, "nope", 404)
		return
	}
	a, b, _ := strings.Cut(parts[1], "-")
	start, _ := strconv.ParseInt(a, 10, 64)
	end, _ := strconv.ParseInt(b, 10, 64)
	if start < 0 || end >= int64(len(f.enc)) || start > end {
		http.Error(w, "bad range", 416)
		return
	}

	m.mu.Lock()
	throttle := m.throttles > 0
	if throttle {
		m.throttles--
	}
	drop := int64(-1)
	if m.drops > 0 {
		m.drops--
		drop = m.dropAfter
	}
	m.mu.Unlock()
	if throttle {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(509)
		return
	}

	body := f.enc[start : end+1]
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(200)
	const piece = 32 * 1024
	for sent := 0; sent < len(body); {
		n := min(piece, len(body)-sent)
		if drop >= 0 && int64(sent+n) > drop {
			n = int(drop) - sent
			if n > 0 {
				w.Write(body[sent : sent+n])
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			panic(http.ErrAbortHandler) // cut the connection mid-body
		}
		if _, err := w.Write(body[sent : sent+n]); err != nil {
			return
		}
		sent += n
		total := m.served.Add(int64(n))
		m.mu.Lock()
		onServe, slow := m.onServe, m.slowPerBuf
		m.mu.Unlock()
		if onServe != nil {
			onServe(total)
		}
		if slow > 0 {
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			time.Sleep(slow)
		}
	}
}

// serveProxy mimics the Worker: GET /<full storage URL> is fetched and relayed.
func (m *fakeMega) serveProxy(w http.ResponseWriter, r *http.Request) {
	m.proxied.Add(1)
	target := strings.TrimPrefix(r.RequestURI, "/")
	if !strings.HasPrefix(target, m.storage.URL+"/dl/") {
		http.Error(w, "403", 403)
		return
	}
	resp, err := http.Get(target)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
}

// hooks sets the per-write callback and delay, safely against in-flight handlers.
func (m *fakeMega) hooks(onServe func(int64), slow time.Duration) {
	m.mu.Lock()
	m.onServe, m.slowPerBuf = onServe, slow
	m.mu.Unlock()
}

func (m *fakeMega) config(out string) *config {
	return &config{
		output: out, jobs: 2, connections: 3, chunkSize: 1 << 20, retries: 5,
		proxies: []string{m.proxy.URL}, verify: true,
	}
}

func (m *fakeMega) apiClient() *apiClient {
	c := newAPIClient(newAPIHTTPClient(), 3)
	c.endpoint = m.api.URL + "/cs"
	return c
}

func (m *fakeMega) run(ctx context.Context, cfg *config, link string) (runStats, error) {
	return processLink(ctx, cfg, m.apiClient(), newDownloadHTTPClient(), link)
}

func mustEqualFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: content differs (got %d bytes, want %d)", path, len(got), len(want))
	}
}

func TestDownloadFileParallel(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "movie.bin", 5<<20+12345, 1, false)
	out := t.TempDir()
	st, err := m.run(context.Background(), m.config(out), m.fileLink(f))
	if err != nil {
		t.Fatal(err)
	}
	if st.completed != 1 || st.verified != 1 {
		t.Fatalf("stats %+v", st)
	}
	mustEqualFile(t, filepath.Join(out, "movie.bin"), f.plain)
	if fileExists(filepath.Join(out, "movie.bin.part")) || fileExists(filepath.Join(out, ".mgdl-FiLe0001")) {
		t.Fatal("leftover .part or control file")
	}
	// One fresh link per chunk request, and all data went through the proxy.
	chunks := len(planChunks(int64(len(f.plain)), 1<<20))
	if got := m.linkCalls.Load(); got != int64(chunks) {
		t.Fatalf("%d link requests for %d chunks", got, chunks)
	}
	if m.proxied.Load() != int64(chunks) {
		t.Fatalf("proxied %d of %d requests", m.proxied.Load(), chunks)
	}
}

func TestDownloadSequentialNoPreallocate(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "seq.bin", 3<<20+1, 2, false)
	out := t.TempDir()
	cfg := m.config(out)
	cfg.sequential = true
	cfg.direct = true

	// Watch the .part while it downloads: in sequential mode it only ever grows.
	part := filepath.Join(out, "seq.bin.part")
	var sawFull atomic.Bool
	m.hooks(func(total int64) {
		if fi, err := os.Stat(part); err == nil && fi.Size() == int64(len(f.plain)) && total < int64(len(f.plain)) {
			sawFull.Store(true)
		}
	}, 0)
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	if sawFull.Load() {
		t.Fatal(".part was preallocated in sequential mode")
	}
	if m.proxied.Load() != 0 {
		t.Fatal("--direct still used the proxy")
	}
	mustEqualFile(t, filepath.Join(out, "seq.bin"), f.plain)
}

func TestDownloadRetriesDroppedConnections(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "flaky.bin", 4<<20, 3, false)
	m.dropAfter, m.drops = 300*1024, 4
	m.throttles = 2
	out := t.TempDir()
	cfg := m.config(out)
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out, "flaky.bin"), f.plain)
}

func TestDownloadGivesUpAfterRetries(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "dead.bin", 1<<20, 4, false)
	m.dropAfter, m.drops = 1000, 1000
	cfg := m.config(t.TempDir())
	cfg.retries = 2
	st, err := m.run(context.Background(), cfg, m.fileLink(f))
	if err == nil || st.failed != 1 {
		t.Fatalf("expected a failure, got %+v, %v", st, err)
	}
}

func TestResumeAfterInterrupt(t *testing.T) {
	m := newFakeMega(t)
	size := 6<<20 + 777
	f := m.addFile("FiLe0001", "big.bin", size, 5, false)
	out := t.TempDir()
	cfg := m.config(out)
	cfg.connections = 4

	ctx, cancel := context.WithCancel(context.Background())
	m.hooks(func(total int64) {
		if total > int64(size)/2 {
			cancel()
		}
	}, 2*time.Millisecond)
	if _, err := m.run(ctx, cfg, m.fileLink(f)); !errors.Is(err, context.Canceled) {
		t.Fatalf("first run: %v", err)
	}
	ctl := filepath.Join(out, ".mgdl-FiLe0001")
	raw, err := os.ReadFile(ctl)
	if err != nil {
		t.Fatalf("control file not saved: %v", err)
	}
	var d ctlData
	if err := json.Unmarshal(raw, &d); err != nil || d.Files["FiLe0001"] == nil {
		t.Fatalf("control file: %v %s", err, raw)
	}
	saved := d.Files["FiLe0001"].doneBytes()
	if saved == 0 {
		t.Fatal("no progress checkpointed")
	}
	for _, c := range d.Files["FiLe0001"].Chunks {
		if pos := c.Start + c.Done; pos != c.End && !isMACBoundary(pos) {
			t.Fatalf("checkpoint %d is not on a MAC boundary", pos)
		}
	}

	m.hooks(nil, 0)
	before := m.served.Load()
	st, err := m.run(context.Background(), cfg, m.fileLink(f))
	if err != nil {
		t.Fatal(err)
	}
	if st.verified != 1 {
		t.Fatalf("resumed file not verified: %+v", st)
	}
	second := m.served.Load() - before
	if second > int64(size)-saved+int64(len(d.Files["FiLe0001"].Chunks))*macSteadyStep {
		t.Fatalf("resume re-downloaded too much: %d bytes after %d were saved (size %d)", second, saved, size)
	}
	mustEqualFile(t, filepath.Join(out, "big.bin"), f.plain)
	if fileExists(ctl) {
		t.Fatal("control file should be gone once complete")
	}
}

func TestParallelPartialRefusesSequential(t *testing.T) {
	m := newFakeMega(t)
	size := 4 << 20
	f := m.addFile("FiLe0001", "p.bin", size, 6, false)
	out := t.TempDir()
	cfg := m.config(out)
	ctx, cancel := context.WithCancel(context.Background())
	m.hooks(func(total int64) {
		if total > int64(size)/3 {
			cancel()
		}
	}, time.Millisecond)
	m.run(ctx, cfg, m.fileLink(f))
	m.hooks(nil, 0)

	cfg.sequential = true
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); !errors.Is(err, errModeMismatch) {
		t.Fatalf("want mode mismatch, got %v", err)
	}
	cfg.sequential = false
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out, "p.bin"), f.plain)
}

func TestMACMismatchRedownloadsThenFails(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "corrupt.bin", 2<<20, 7, true)
	out := t.TempDir()
	cfg := m.config(out)
	cfg.retries = 2
	st, err := m.run(context.Background(), cfg, m.fileLink(f))
	if err == nil || st.failed != 1 {
		t.Fatalf("expected failure, got %+v %v", st, err)
	}
	if m.served.Load() != 2*int64(len(f.plain)) {
		t.Fatalf("expected exactly 2 full downloads, served %d", m.served.Load())
	}
	if fileExists(filepath.Join(out, "corrupt.bin")) || fileExists(filepath.Join(out, "corrupt.bin.part")) {
		t.Fatal("corrupt file left behind")
	}

	// --no-verify accepts it.
	cfg.verify = false
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out, "corrupt.bin"), f.plain)
}

func TestFolderDownloadPathsAndSkip(t *testing.T) {
	m := newFakeMega(t)
	a := m.addFile("FiLeAaAa", "a.txt", 100, 8, false)
	b := m.addFile("FiLeBbBb", "b.bin", 2<<20+5, 9, false)
	c := m.addFile("FiLeCcCc", "c.bin", 0, 10, false)  // empty file
	d := m.addFile("FiLeDdDd", "a.txt", 50, 11, false) // duplicate name in same dir
	link := m.publishFolder("My Share", []*fakeFile{a, d}, []*fakeFile{b, c})
	out := t.TempDir()
	cfg := m.config(out)

	cfg.paths = []string{"sub/b.bin"}
	if _, err := m.run(context.Background(), cfg, link); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out, "My Share", "sub", "b.bin"), b.plain)
	if fileExists(filepath.Join(out, "My Share", "a.txt")) {
		t.Fatal("--path downloaded an unselected file")
	}

	cfg.paths = nil
	st, err := m.run(context.Background(), cfg, link)
	if err != nil {
		t.Fatal(err)
	}
	if st.skipped != 1 || st.completed != 3 {
		t.Fatalf("stats %+v", st)
	}
	mustEqualFile(t, filepath.Join(out, "My Share", "a.txt"), a.plain)
	mustEqualFile(t, filepath.Join(out, "My Share", "a (2).txt"), d.plain)
	mustEqualFile(t, filepath.Join(out, "My Share", "sub", "c.bin"), c.plain)
	if fileExists(filepath.Join(out, "My Share", ".mgdl-"+m.folder)) {
		t.Fatal("control file left after completion")
	}

	// Subfolder link: the subfolder becomes the root.
	out2 := t.TempDir()
	if _, err := m.run(context.Background(), m.config(out2), link+"/folder/SuBdIr01"); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out2, "sub", "b.bin"), b.plain)
}

func TestVerifyExistingCatchesCorruption(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "v.bin", 1<<20+3, 12, false)
	out := t.TempDir()
	cfg := m.config(out)
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(out, "v.bin")
	data, _ := os.ReadFile(p)
	data[500000] ^= 1
	os.WriteFile(p, data, 0o644)

	// Without --verify-existing the (same-size) file is trusted.
	st, _ := m.run(context.Background(), cfg, m.fileLink(f))
	if st.skipped != 1 {
		t.Fatalf("expected skip, got %+v", st)
	}
	cfg.verifyExisting = true
	st, err := m.run(context.Background(), cfg, m.fileLink(f))
	if err != nil || st.completed != 1 {
		t.Fatalf("expected re-download, got %+v %v", st, err)
	}
	mustEqualFile(t, p, f.plain)
}

func TestOutOfOrderFoldMatchesSequential(t *testing.T) {
	// Parallel chunks report MACs in any order; the folded meta-MAC must match a
	// straight sequential computation.
	rng := rand.New(rand.NewSource(13))
	data := make([]byte, 9<<20+99)
	rng.Read(data)
	var key [16]byte
	var nonce [8]byte
	rng.Read(key[:])
	rng.Read(nonce[:])
	want := refMeta(key, nonce, data)

	fk := &fileKey{aes: key, nonce: nonce}
	st := &fileState{Size: int64(len(data)), Chunks: planChunks(int64(len(data)), 2<<20)}
	j := &job{e: &engine{ctl: &controlFile{data: ctlData{Files: map[string]*fileState{}}}}, st: st, n: &node{key: fk}}
	j.fold, _ = aes.NewCipher(key[:])

	order := rng.Perm(len(st.Chunks))
	for _, ci := range order {
		c := st.Chunks[ci]
		ms := newMACStream(key, nonce, c.Start, int64(len(data)))
		// Report in a few pieces, as checkpoints would.
		for pos := c.Start; pos < c.End; {
			n := min(int64(700000), c.End-pos)
			macs := ms.Write(data[pos : pos+n])
			pos += n
			if len(macs) > 0 {
				j.record(ci, ms.lastBoundary-c.Start, macs)
			}
		}
	}
	if st.Folded != len(st.Chunks) || [16]byte(st.Meta) != want {
		t.Fatalf("folded %d/%d, meta %x want %x", st.Folded, len(st.Chunks), st.Meta, want)
	}
}

func TestPlanChunks(t *testing.T) {
	for _, size := range []int64{1, 1000, 1 << 20, 5<<20 + 1, 300 << 20, 1<<30 + 12345} {
		for _, cs := range []int64{1 << 20, 3 << 20, 128 << 20, 256 << 20} {
			chunks := planChunks(size, cs)
			var pos int64
			for _, c := range chunks {
				if c.Start != pos || c.End <= c.Start {
					t.Fatalf("size %d cs %d: gap/overlap at %d", size, cs, c.Start)
				}
				if c.End != size && !isMACBoundary(c.End) {
					t.Fatalf("size %d cs %d: boundary %d not MAC-aligned", size, cs, c.End)
				}
				if c.End-c.Start > cs+macSteadyStep*2 {
					t.Fatalf("size %d cs %d: chunk too big (%d)", size, cs, c.End-c.Start)
				}
				pos = c.End
			}
			if pos != size {
				t.Fatalf("size %d cs %d: covers %d", size, cs, pos)
			}
		}
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "x.bin", 2<<20, 14, false)
	out := t.TempDir()
	cfg := m.config(out)
	cfg.dryRun = true
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 || m.requests.Load() != 0 {
		t.Fatalf("dry run touched disk or storage: %v, %d requests", entries, m.requests.Load())
	}
}

func TestAPIErrors(t *testing.T) {
	for raw, want := range map[string]error{
		`-9`:          apiError(-9),
		`[-11]`:       apiError(-11),
		`[{"e":-16}]`: apiError(-16),
	} {
		var out fileInfo
		if err := decodeAPIResult([]byte(raw), &out); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", raw, err, want)
		}
	}
	var out fileInfo
	if err := decodeAPIResult([]byte(`[{"s":5,"at":"x","g":"http://h/dl/t"}]`), &out); err != nil || out.Size != 5 {
		t.Fatalf("decode: %+v %v", out, err)
	}
	if !apiError(-9).permanent() || apiError(-3).permanent() {
		t.Fatal("permanence")
	}
	_ = fmt.Sprint(apiError(-3))
}
