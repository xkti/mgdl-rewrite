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
	"maps"
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
	extra  map[string][]rawNode // further shares (publishShare), by share handle

	mu         sync.Mutex
	dropAfter  int64 // drop a storage response after this many bytes…
	drops      int   // …for this many responses
	throttles  int   // answer this many storage requests with 509
	hangups    int   // close this many storage connections before any response
	served     atomic.Int64
	requests   atomic.Int64
	linkCalls  atomic.Int64
	proxied    atomic.Int64
	onServe    func(total int64) // called as bytes go out (to simulate Ctrl+C)
	slowPerBuf time.Duration
	piece      int     // bytes per storage write (0 = 32 KiB)
	starts     []int64 // start offset of every storage request, in order

	linkErrs   []int // API error codes to answer download-link requests with, in order
	metaErrs   []int // likewise for metadata requests (g without a link, and f)
	linkReqs   atomic.Int64
	apiRetries int // the client's retry budget for metadata calls

	inflight    atomic.Int64 // storage requests being served right now
	maxInflight atomic.Int64
}

func TestMain(m *testing.M) {
	apiThrottleWait = 20 * time.Millisecond // keep throttle tests fast
	os.Exit(m.Run())
}

// popErr takes the next queued API error, if any.
func (m *fakeMega) popErr(q *[]int) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(*q) == 0 {
		return 0, false
	}
	code := (*q)[0]
	*q = (*q)[1:]
	return code, true
}

type fakeFile struct {
	handle string
	name   string
	plain  []byte
	enc    []byte
	key    []byte // 32-byte node key
}

func newFakeMega(t *testing.T) *fakeMega {
	m := &fakeMega{t: t, files: map[string]*fakeFile{}, apiRetries: 3}
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
	for i := range 16 {
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
	m.nodes = shareNodes(m.master, "RoOtNoDe", "SuBdIr01", root, top, sub)
	return "https://mega.nz/folder/" + m.folder + "#" + b64enc(m.master)
}

// publishShare publishes a further share, like publishFolder, under the given
// share, root and subfolder handles (all distinct from other shares').
func (m *fakeMega) publishShare(share, rootH, subH, root string, top, sub []*fakeFile) string {
	master := bytes.Repeat([]byte{share[0]}, 16)
	if m.extra == nil {
		m.extra = map[string][]rawNode{}
	}
	m.extra[share] = shareNodes(master, rootH, subH, root, top, sub)
	return "https://mega.nz/folder/" + share + "#" + b64enc(master)
}

func (m *fakeMega) shareListing(share string) ([]rawNode, bool) {
	if share != "" && share == m.folder {
		return m.nodes, true
	}
	nodes, ok := m.extra[share]
	return nodes, ok
}

func shareNodes(master []byte, rootH, subH, root string, top, sub []*fakeFile) []rawNode {
	wrap := func(k []byte) string {
		b, _ := aes.NewCipher(master)
		out := make([]byte, len(k))
		for i := 0; i < len(k); i += 16 {
			b.Encrypt(out[i:i+16], k[i:i+16])
		}
		return b64enc(out)
	}
	dirKey := func(seed byte) []byte { return bytes.Repeat([]byte{seed}, 16) }
	nodes := []rawNode{
		{Handle: rootH, Parent: "OuTsIdE0", Type: 1, Attr: encryptAttrs(dirKey(1), root), Key: rootH + ":" + wrap(dirKey(1))},
		{Handle: subH, Parent: rootH, Type: 1, Attr: encryptAttrs(dirKey(2), "sub"), Key: rootH + ":" + wrap(dirKey(2))},
	}
	add := func(f *fakeFile, parent string) {
		fk, _ := unpackFileKey(f.key)
		nodes = append(nodes, rawNode{Handle: f.handle, Parent: parent, Type: 0, Size: int64(len(f.plain)),
			Attr: encryptAttrs(fk.aes[:], f.name),
			// An unrelated share's key first, as real listings sometimes have.
			Key: "XxXxXxXx:" + b64enc(bytes.Repeat([]byte{9}, 32)) + "/" + rootH + ":" + wrap(f.key)})
	}
	for _, f := range top {
		add(f, rootH)
	}
	for _, f := range sub {
		add(f, subH)
	}
	return nodes
}

func (m *fakeMega) serveAPI(w http.ResponseWriter, r *http.Request) {
	var cmds []map[string]any
	if err := json.NewDecoder(r.Body).Decode(&cmds); err != nil || len(cmds) != 1 {
		w.Write([]byte("-2"))
		return
	}
	cmd := cmds[0]
	queue := &m.metaErrs
	if cmd["g"] != nil {
		m.linkReqs.Add(1)
		queue = &m.linkErrs
	}
	if code, ok := m.popErr(queue); ok {
		fmt.Fprintf(w, "[%d]", code)
		return
	}
	switch cmd["a"] {
	case "g":
		h, _ := cmd["p"].(string)
		if n, ok := cmd["n"].(string); ok {
			if _, ok := m.shareListing(r.URL.Query().Get("n")); !ok {
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
		nodes, ok := m.shareListing(r.URL.Query().Get("n"))
		if !ok {
			w.Write([]byte("[-9]"))
			return
		}
		json.NewEncoder(w).Encode([]any{map[string]any{"f": nodes}})
	default:
		w.Write([]byte("[-2]"))
	}
}

// serveStorage answers GET /dl/HANDLE/START-END, MEGA's byte-range URL suffix.
func (m *fakeMega) serveStorage(w http.ResponseWriter, r *http.Request) {
	m.requests.Add(1)
	now := m.inflight.Add(1)
	defer m.inflight.Add(-1)
	for {
		top := m.maxInflight.Load()
		if now <= top || m.maxInflight.CompareAndSwap(top, now) {
			break
		}
	}
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
	hangup := m.hangups > 0
	if hangup {
		m.hangups--
	}
	throttle := m.throttles > 0
	if throttle {
		m.throttles--
	}
	m.starts = append(m.starts, start)
	piece := m.piece
	if piece == 0 {
		piece = 32 * 1024
	}
	drop := int64(-1)
	if m.drops > 0 {
		m.drops--
		drop = m.dropAfter
	}
	m.mu.Unlock()
	if hangup {
		panic(http.ErrAbortHandler) // no status line: the client's Do fails
	}
	if throttle {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(509)
		return
	}

	body := f.enc[start : end+1]
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(200)
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
		if m.piece > 0 {
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush() // make the client see each piece as its own read
			}
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
	maps.Copy(w.Header(), resp.Header)
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
	c := newAPIClient(newAPIHTTPClient(), m.apiRetries)
	c.endpoint = m.api.URL + "/cs"
	return c
}

func (m *fakeMega) run(ctx context.Context, cfg *config, link string) (runStats, error) {
	st, _, err := m.runAll(ctx, cfg, link)
	return st, err
}

// runAll runs several links as one, as the command line does.
func (m *fakeMega) runAll(ctx context.Context, cfg *config, links ...string) (runStats, []*linkRun, error) {
	c := *cfg
	c.links = links
	return runLinks(ctx, &c, m.apiClient(), newDownloadHTTPClient())
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

// Reads are gathered into a buffer before being decrypted, MACed and written; odd
// read sizes that straddle blocks, buffers and MAC chunks must change nothing, with
// or without verification, in either mode.
func TestOddReadSizes(t *testing.T) {
	for _, piece := range []int{7, 16*1024 + 3} {
		for _, seq := range []bool{false, true} {
			for _, verify := range []bool{true, false} {
				m := newFakeMega(t)
				m.piece = piece
				size := 600*1024 + 11
				if piece > 1024 {
					size = 5<<20 + 11
				}
				f := m.addFile("FiLe0001", "odd.bin", size, 9, false)
				out := t.TempDir()
				cfg := m.config(out)
				cfg.sequential, cfg.verify, cfg.direct = seq, verify, true
				st, err := m.run(context.Background(), cfg, m.fileLink(f))
				if err != nil {
					t.Fatalf("piece %d seq %v verify %v: %v", piece, seq, verify, err)
				}
				if st.completed != 1 {
					t.Fatalf("piece %d seq %v verify %v: stats %+v", piece, seq, verify, st)
				}
				mustEqualFile(t, filepath.Join(out, "odd.bin"), f.plain)
			}
		}
	}
}

// A connection that drops with data still gathered in the buffer must write it
// and checkpoint the MAC boundary it crossed, so the retry starts there and not
// at the last boundary written before it.
func TestDropFlushesGatheredData(t *testing.T) {
	for _, verify := range []bool{true, false} {
		m := newFakeMega(t)
		m.piece = 16 * 1024
		// Reads 0–256 KiB fill the buffer (checkpoint 128 KiB); 256–400 KiB are
		// still gathered when the connection drops, past the 384 KiB boundary.
		m.dropAfter, m.drops = 400*1024, 1
		f := m.addFile("FiLe0001", "drop.bin", 2<<20, 10, false)
		out := t.TempDir()
		cfg := m.config(out)
		cfg.sequential, cfg.verify, cfg.direct = true, verify, true
		if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
			t.Fatal(err)
		}
		mustEqualFile(t, filepath.Join(out, "drop.bin"), f.plain)
		m.mu.Lock()
		starts := m.starts
		m.mu.Unlock()
		// (The second download chunk, at 1280 KiB, is fetched after these.)
		if len(starts) < 2 || starts[0] != 0 || starts[1] != 384*1024 {
			t.Fatalf("verify %v: requests started at %v, want 0 then %d", verify, starts, 384*1024)
		}
	}
}

// A connection dropped before the response makes net/http name the URL in its
// error; the retry warning must not show it (only -v does).
func TestRetryWarningHidesURL(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "hangup.bin", 256<<10, 5, false)
	m.hangups = 1
	out := t.TempDir()
	cfg := m.config(out)
	stderr := captureStderr(t, func() {
		if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
			t.Error(err)
		}
	})
	mustEqualFile(t, filepath.Join(out, "hangup.bin"), f.plain)
	if !strings.Contains(stderr, "retry") {
		t.Fatalf("expected a retry warning, got:\n%s", stderr)
	}
	if strings.Contains(stderr, "/dl/") || strings.Contains(stderr, "http://") {
		t.Fatalf("warning shows the URL:\n%s", stderr)
	}
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
	mustEqualFile(t, filepath.Join(out, "My Share", "a_FiLeDdDd.txt"), d.plain)
	mustEqualFile(t, filepath.Join(out, "My Share", "sub", "c.bin"), c.plain)
	if fileExists(filepath.Join(out, "My Share", ".mgdl-"+m.folder)) {
		t.Fatal("control file left after completion")
	}

	// Subfolder link: the share's tree is kept, only the subfolder is downloaded.
	out2 := t.TempDir()
	if _, err := m.run(context.Background(), m.config(out2), link+"/folder/SuBdIr01"); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out2, "My Share", "sub", "b.bin"), b.plain)
	mustEqualFile(t, filepath.Join(out2, "My Share", "sub", "c.bin"), c.plain)
	if fileExists(filepath.Join(out2, "My Share", "a.txt")) {
		t.Fatal("a subfolder link downloaded a file outside its subfolder")
	}

	// -p is relative to the share's top folder and narrows within the subfolder.
	out3 := t.TempDir()
	cfg3 := m.config(out3)
	cfg3.paths = []string{"sub/c.bin"}
	if _, err := m.run(context.Background(), cfg3, link+"/folder/SuBdIr01"); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out3, "My Share", "sub", "c.bin"), c.plain)
	if fileExists(filepath.Join(out3, "My Share", "sub", "b.bin")) {
		t.Fatal("--path didn't narrow the subfolder link")
	}
	cfg3.paths = []string{"a.txt"} // outside the subfolder: matches nothing
	if _, err := m.run(context.Background(), cfg3, link+"/folder/SuBdIr01"); err == nil || !strings.Contains(err.Error(), "matches nothing") {
		t.Fatalf("--path outside the subfolder: %v", err)
	}
}

// Links to different parts of the same share download into one copy of its tree,
// as one run with one control file; the same link twice is still a repeat.
func TestLinksIntoSameShare(t *testing.T) {
	m := newFakeMega(t)
	a := m.addFile("FiLeAaAa", "a.txt", 100, 8, false)
	x := m.addFile("FiLeXxXx", "x.txt", 70, 12, false)
	b := m.addFile("FiLeBbBb", "b.bin", 2<<20+5, 9, false)
	link := m.publishFolder("My Share", []*fakeFile{a, x}, []*fakeFile{b})
	out := t.TempDir()

	st, runs, err := m.runAll(context.Background(), m.config(out),
		link+"/folder/SuBdIr01", link+"/file/FiLeAaAa", link+"/folder/SuBdIr01")
	if err != nil {
		t.Fatal(err)
	}
	if st.completed != 2 {
		t.Fatalf("stats %+v", st)
	}
	mustEqualFile(t, filepath.Join(out, "My Share", "sub", "b.bin"), b.plain)
	mustEqualFile(t, filepath.Join(out, "My Share", "a.txt"), a.plain)
	if fileExists(filepath.Join(out, "My Share", "x.txt")) {
		t.Fatal("a file no link pointed at was downloaded")
	}
	if fileExists(filepath.Join(out, "My Share_FoLdEr01")) || fileExists(filepath.Join(out, "My Share", "a_FiLeAaAa.txt")) {
		t.Fatal("the share was downloaded twice")
	}
	if runs[0].status != statusDone || runs[1].status != statusDone || runs[1].joined != runs[0] {
		t.Fatalf("statuses %v %v, joined %v", runs[0].status, runs[1].status, runs[1].joined)
	}
	if runs[2].status != statusSkipped || runs[2].note != "same as link 1" {
		t.Fatalf("repeat: %v %q", runs[2].status, runs[2].note)
	}
	if got := linkReport(runs, 0); len(got) != 2 || !strings.Contains(got[1], "My Share (3 links)") {
		t.Fatalf("report %q", got)
	}
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
	j := &job{e: &engine{}, lk: &linkRun{ctl: &controlFile{data: ctlData{Files: map[string]*fileState{}}}}, st: st, n: &node{key: fk}}
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
	if !apiError(-9).permanent() || !apiError(-17).permanent() || apiError(-3).permanent() || apiError(-4).permanent() {
		t.Fatal("permanence")
	}
	if !apiError(-4).throttled() || !apiError(-6).throttled() || apiError(-3).throttled() {
		t.Fatal("throttling")
	}
	_ = fmt.Sprint(apiError(-3))
}

// A failed link request inside a chunk is one chunk attempt, not a nested API retry
// loop: with -r 2, a chunk whose link requests always fail makes exactly 2 of them.
func TestChunkLinkFailuresDontMultiply(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "x.bin", 100*1024, 20, false)
	m.linkErrs = []int{-3, -3, -3, -3, -3, -3, -3, -3}
	cfg := m.config(t.TempDir())
	cfg.retries = 2
	st, err := m.run(context.Background(), cfg, m.fileLink(f))
	if err == nil || st.failed != 1 {
		t.Fatalf("expected failure, got %+v %v", st, err)
	}
	if got := m.linkReqs.Load(); got != 2 {
		t.Fatalf("%d link requests, want 2", got)
	}
}

// API throttling (-4/-6) is waited out without spending retries, both for metadata
// and for download links, even with a budget of a single attempt.
func TestAPIThrottlingIsFree(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "t.bin", 2<<20, 21, false)
	m.apiRetries = 1
	m.metaErrs = []int{-4, -6, -4}
	m.linkErrs = []int{-6, -4, -6, -4}
	out := t.TempDir()
	cfg := m.config(out)
	cfg.retries = 1
	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, filepath.Join(out, "t.bin"), f.plain)
}

// -17 (over quota) is permanent: no retries, the file fails at once.
func TestOverQuotaIsPermanent(t *testing.T) {
	m := newFakeMega(t)
	f := m.addFile("FiLe0001", "q.bin", 100*1024, 22, false)
	m.linkErrs = []int{-17, -17, -17, -17, -17}
	cfg := m.config(t.TempDir())
	st, err := m.run(context.Background(), cfg, m.fileLink(f))
	if err == nil || st.failed != 1 {
		t.Fatalf("expected failure, got %+v %v", st, err)
	}
	if got := m.linkReqs.Load(); got != 1 {
		t.Fatalf("%d link requests, want 1", got)
	}

	// And while resolving a link, too.
	m2 := newFakeMega(t)
	f2 := m2.addFile("FiLe0002", "q.bin", 100, 23, false)
	m2.metaErrs = []int{-17, -17, -17}
	if _, err := m2.run(context.Background(), m2.config(t.TempDir()), m2.fileLink(f2)); !errors.Is(err, apiError(-17)) {
		t.Fatalf("want -17, got %v", err)
	}
}

// Any decryption failure stops the link before anything is downloaded.
func TestDecryptionFailuresHalt(t *testing.T) {
	expectHalt := func(t *testing.T, m *fakeMega, link, want string) {
		t.Helper()
		out := t.TempDir()
		_, err := m.run(context.Background(), m.config(out), link)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want error containing %q, got %v", want, err)
		}
		if m.requests.Load() != 0 || m.linkReqs.Load() != 0 {
			t.Fatalf("downloading started: %d storage requests, %d link requests", m.requests.Load(), m.linkReqs.Load())
		}
		if entries, _ := os.ReadDir(out); len(entries) != 0 {
			t.Fatalf("wrote to disk: %v", entries)
		}
	}

	t.Run("wrong file key", func(t *testing.T) {
		m := newFakeMega(t)
		f := m.addFile("FiLe0001", "a.bin", 1000, 30, false)
		good := f.key
		f.key = append([]byte{good[0] ^ 1}, good[1:]...) // link carries a wrong key…
		link := m.fileLink(f)
		f.key = good // …while MEGA has the file under the right one
		expectHalt(t, m, link, "link's key is wrong")
	})

	t.Run("wrong folder key", func(t *testing.T) {
		m := newFakeMega(t)
		a := m.addFile("FiLeAaAa", "a.txt", 100, 31, false)
		m.publishFolder("Share", []*fakeFile{a}, nil)
		bad := bytes.Repeat([]byte{0x43}, 16)
		expectHalt(t, m, "https://mega.nz/folder/"+m.folder+"#"+b64enc(bad), "link's key is wrong")
	})

	t.Run("one undecryptable file", func(t *testing.T) {
		m := newFakeMega(t)
		a := m.addFile("FiLeAaAa", "a.txt", 100, 32, false)
		b := m.addFile("FiLeBbBb", "b.txt", 100, 33, false)
		link := m.publishFolder("Share", []*fakeFile{a}, []*fakeFile{b})
		for i := range m.nodes {
			if m.nodes[i].Handle == "FiLeBbBb" {
				m.nodes[i].Attr = encryptAttrs(bytes.Repeat([]byte{1}, 16), "b.txt") // wrong key
			}
		}
		expectHalt(t, m, link, "cannot decrypt file FiLeBbBb")
	})

	t.Run("nameless attributes", func(t *testing.T) {
		m := newFakeMega(t)
		a := m.addFile("FiLeAaAa", "", 100, 34, false)
		link := m.publishFolder("Share", []*fakeFile{a}, nil)
		expectHalt(t, m, link, "no name")
	})

	t.Run("no key for this share", func(t *testing.T) {
		m := newFakeMega(t)
		a := m.addFile("FiLeAaAa", "a.txt", 100, 35, false)
		link := m.publishFolder("Share", []*fakeFile{a}, nil)
		for i := range m.nodes {
			if m.nodes[i].Handle == "FiLeAaAa" {
				m.nodes[i].Key = "XxXxXxXx:" + b64enc(bytes.Repeat([]byte{9}, 32))
			}
		}
		expectHalt(t, m, link, "no key for this share")
	})

	t.Run("malformed node key", func(t *testing.T) {
		m := newFakeMega(t)
		a := m.addFile("FiLeAaAa", "a.txt", 100, 36, false)
		link := m.publishFolder("Share", []*fakeFile{a}, nil)
		for i := range m.nodes {
			if m.nodes[i].Handle == "SuBdIr01" {
				m.nodes[i].Key = "RoOtNoDe:" + b64enc(make([]byte, 20)) // not a whole AES block
			}
		}
		expectHalt(t, m, link, "cannot decrypt folder SuBdIr01")
	})
}

// A -C other than the one a partial file was planned with keeps that file's
// layout and says so once, after its resume line (and in a dry run), with the
// nouns fitted to how many partial and new files there are.
func TestChunkSizeChangeWarns(t *testing.T) {
	interrupt := func(m *fakeMega, cfg *config, link string, size int) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		m.hooks(func(total int64) {
			if total > int64(size)/2 {
				cancel()
			}
		}, 2*time.Millisecond)
		if _, err := m.run(ctx, cfg, link); !errors.Is(err, context.Canceled) {
			t.Fatalf("first run: %v", err)
		}
		m.hooks(nil, 0)
	}
	const head = "A different chunk size was specified to what was originally set (1M instead of 2M)! "

	t.Run("file link", func(t *testing.T) {
		m := newFakeMega(t)
		size := 7 << 20
		f := m.addFile("FiLe0001", "big.bin", size, 31, false)
		cfg := m.config(t.TempDir())
		cfg.chunkSize = 2 << 20
		interrupt(m, cfg, m.fileLink(f), size)
		before, _ := os.ReadFile(filepath.Join(cfg.output, ".mgdl-FiLe0001"))

		// Same size, or -C not given: nothing to say.
		cfg.chunkSet, cfg.dryRun = true, true
		if got := captureStderr(t, func() { m.run(context.Background(), cfg, m.fileLink(f)) }); strings.Contains(got, "chunk size") {
			t.Fatalf("same -C warned: %q", got)
		}
		cfg.chunkSize, cfg.chunkSet = 1<<20, false
		if got := captureStderr(t, func() { m.run(context.Background(), cfg, m.fileLink(f)) }); strings.Contains(got, "chunk size") {
			t.Fatalf("default -C warned: %q", got)
		}

		want := head + "Ignoring for the partial file."
		cfg.chunkSet = true
		if got := captureStderr(t, func() { m.run(context.Background(), cfg, m.fileLink(f)) }); strings.Count(got, want) != 1 {
			t.Fatalf("dry run: want %q once, got %q", want, got)
		}
		cfg.dryRun = false
		var err error
		got := captureStderr(t, func() { _, err = m.run(context.Background(), cfg, m.fileLink(f)) })
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(got, want) != 1 {
			t.Fatalf("run: want %q once, got %q", want, got)
		}
		mustEqualFile(t, filepath.Join(cfg.output, "big.bin"), f.plain)
		var d ctlData
		if json.Unmarshal(before, &d) != nil || d.Files["FiLe0001"].Chunk != 2<<20 {
			t.Fatalf("chunk size not recorded: %s", before)
		}
	})

	t.Run("folder", func(t *testing.T) {
		m := newFakeMega(t)
		size := 7 << 20
		big := m.addFile("FiLeAaAa", "big.bin", size, 32, false)
		x := m.addFile("FiLeBbBb", "x.bin", 3000, 33, false)
		y := m.addFile("FiLeCcCc", "y.bin", 4000, 34, false)
		link := m.publishFolder("Share", []*fakeFile{big}, []*fakeFile{x, y})
		cfg := m.config(t.TempDir())
		cfg.chunkSize = 2 << 20
		cfg.paths = []string{"big.bin"}
		interrupt(m, cfg, link, size)

		cfg.paths, cfg.chunkSize, cfg.chunkSet = nil, 1<<20, true
		want := head + "Ignoring for the partial file, changing for new files yet to be downloaded."
		var err error
		got := captureStderr(t, func() { _, err = m.run(context.Background(), cfg, link) })
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(got, want) != 1 {
			t.Fatalf("want %q once, got %q", want, got)
		}
		mustEqualFile(t, filepath.Join(cfg.output, "Share", "big.bin"), big.plain)
	})
}
