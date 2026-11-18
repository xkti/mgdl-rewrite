package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// readChunk is how much is read, decrypted, MACed and written per iteration.
	readChunk = 256 * 1024

	// stallTimeout drops a connection that has delivered nothing for this long.
	stallTimeout = 60 * time.Second

	// maxThrottleWaits caps the free (not counted as retries) waits on 429/503/509.
	maxThrottleWaits = 20

	// progressReset: an attempt that got at least this far resets the chunk's retry
	// counter, so scattered drops over a long download don't exhaust -r.
	progressReset = 8 << 20

	// macRetriesWhenInfinite bounds redownloads after a MAC mismatch when -r 0 asks
	// for unlimited retries: a file that keeps failing verification (wrong key, a
	// corrupt upload) would otherwise download forever.
	macRetriesWhenInfinite = 3
)

// errModeMismatch is returned when a parallel partial download is resumed with
// --sequential.
var errModeMismatch = errors.New("mode mismatch")

// localError is a problem with the local disk (full, read-only, permissions).
// Retrying cannot help, so it fails the file straight away.
type localError struct{ err error }

func (e *localError) Error() string { return e.err.Error() }
func (e *localError) Unwrap() error { return e.err }

// throttleError is an upstream (storage, proxy or the API) asking us to slow down
// rather than a failure.
type throttleError struct {
	what  string // "HTTP 509", "MEGA API error -4"
	after time.Duration
}

func (e *throttleError) Error() string {
	return fmt.Sprintf("%s (rate limited), waiting %s", e.what, e.after)
}

func isThrottle(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || status == 509
}

// retryAfter reads Retry-After with a fallback, clamped to [1s, 2m].
func retryAfter(resp *http.Response, fallback time.Duration) time.Duration {
	d := fallback
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			d = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(v); err == nil {
			d = time.Until(t)
		}
	}
	return max(time.Second, min(d, 2*time.Minute))
}

// engine downloads the selected files of every link in a run. What belongs to
// one link (tree, control file, space guard) travels with each job in its linkRun.
type engine struct {
	cfg   *config
	api   *apiClient
	http  *http.Client
	lim   *rateLimiter
	prog  *progress
	multi bool // several links in the run: status lines name each file's link folder

	noSparseOnce sync.Once // the "filesystem can't do sparse files" notice
}

// markSparseFn flags a file sparse (a no-op where files are sparse anyway). A
// variable so tests can simulate a filesystem without sparse support.
var markSparseFn = markSparse

// job is one file being downloaded.
type job struct {
	e     *engine
	n     *node
	final string
	part  string
	row   *slot

	lk    *linkRun // the link this file belongs to: its tree, control file and space guard
	shown string   // the file's path in status lines (displayPath)

	st     *fileState // owned by lk.ctl; mutate only inside lk.ctl.update
	verify bool       // compute and check the MAC for this file
	fold   cipher.Block
	f      *os.File

	// For the space guard: bytes this file still needed when it started, and bytes
	// checkpointed since, so other files can see what it has yet to write.
	spaceNeed    atomic.Int64
	spaceWritten atomic.Int64
}

// fileResult is what happened to one file.
type fileResult int

const (
	resDone fileResult = iota
	resVerified
	resSkipped
)

// fetchFile brings one file to completion: skip it if already present, otherwise
// download (resuming if possible), verify, and rename into place.
func (e *engine) fetchFile(ctx context.Context, lk *linkRun, n *node, row *slot) (fileResult, error) {
	final := lk.tree.localPath(e.cfg.output, n)
	j := &job{e: e, lk: lk, n: n, final: final, part: final + ".part", row: row, shown: displayPath(lk, n, e.multi)}

	if fi, err := os.Stat(final); err == nil {
		switch {
		case fi.IsDir():
			return 0, fmt.Errorf("%s exists and is a directory", final)
		case fi.Size() != n.size:
			warnf("%s exists but is %s, expected %s — downloading again", j.shown, formatBytes(fi.Size()), formatBytes(n.size))
			if err := os.Remove(final); err != nil {
				return 0, err
			}
		case e.cfg.verifyExisting && e.cfg.verify && n.size > 0:
			ok, err := j.verifyOnDisk(ctx, final)
			if err != nil {
				return 0, err
			}
			if ok {
				j.lk.ctl.remove(n.handle)
				infof("%s  %s  (already complete, %s)", colour("32", "verified"), j.shown, formatBytes(n.size))
				return resSkipped, nil
			}
			warnf("%s failed verification (MAC mismatch) — deleting and downloading again", j.shown)
			if err := os.Remove(final); err != nil {
				return 0, err
			}
		default:
			j.lk.ctl.remove(n.handle)
			infof("skip      %s  (already complete, %s)", j.shown, formatBytes(n.size))
			return resSkipped, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return 0, err
	}
	if err := j.lk.space.claim(j, fileNeed(e.cfg, j.lk.tree, j.lk.ctl, n)); err != nil {
		return 0, err
	}
	defer j.lk.space.release(j)

	macBudget := e.cfg.retries
	if macBudget == 0 {
		macBudget = macRetriesWhenInfinite
	}
	for macFails := 0; ; {
		res, err := j.run(ctx)
		if err == nil {
			return res, nil
		}
		if !errors.Is(err, errMACMismatch) {
			j.discardIfEmpty()
			return 0, err
		}
		macFails++
		if macFails >= macBudget {
			return 0, fmt.Errorf("MAC mismatch after %d download(s) — the file on MEGA may be corrupt", macFails)
		}
		warnf("%s failed verification (MAC mismatch) — deleting and downloading again (%d/%d)", j.shown, macFails+1, macBudget)
		j.spaceNeed.Store(n.size) // the .part was deleted: all of it is needed again
		j.spaceWritten.Store(0)
	}
}

var errMACMismatch = errors.New("MAC mismatch")

// run performs one complete download pass of the file.
func (j *job) run(ctx context.Context) (fileResult, error) {
	e, n := j.e, j.n
	if err := j.prepare(); err != nil {
		return 0, err
	}
	defer func() {
		if j.f != nil {
			j.f.Close()
			j.f = nil
		}
	}()

	done := j.st.doneBytes()
	j.row.begin(n.name, done, n.size)
	if done > 0 {
		infof("resume    %s  (%s of %s, %d%%)", j.shown, formatBytes(done), formatBytes(n.size), pct(done, n.size))
	}

	// Pending chunks, in file order.
	var pending []int
	for i, c := range j.st.Chunks {
		if !c.complete() {
			pending = append(pending, i)
		}
	}
	debugf("%s: %d byte(s), %d chunk(s), %d pending, mode %s, verify %v",
		j.shown, n.size, len(j.st.Chunks), len(pending), j.st.Mode, j.verify)

	if len(pending) > 0 {
		workers := 1
		if j.st.Mode == modeParallel {
			workers = min(e.cfg.connections, len(pending))
		}
		if err := j.runChunks(ctx, pending, workers); err != nil {
			return 0, err
		}
	}

	// Everything is on disk and every MAC folded.
	verified := false
	if j.verify && n.size > 0 {
		var got [8]byte
		j.lk.ctl.update(func() { got = condense(j.st.Meta) })
		if got != n.key.mac {
			debugf("%s: MAC mismatch, computed %x, expected %x", j.shown, got, n.key.mac)
			j.f.Close()
			j.f = nil
			os.Remove(j.part)
			j.lk.ctl.remove(n.handle)
			j.row.reset(0)
			return 0, errMACMismatch
		}
		debugf("%s: MAC ok (%x)", j.shown, got)
		verified = true
	}
	if err := j.f.Sync(); err != nil {
		return 0, err
	}
	if err := j.f.Close(); err != nil {
		return 0, err
	}
	j.f = nil
	if err := os.Rename(j.part, j.final); err != nil {
		return 0, fmt.Errorf("renaming: %w", err)
	}
	j.lk.ctl.remove(n.handle)
	// "verified" is reserved for --verify-existing; the colour says whether the
	// MAC was checked (green), couldn't be (yellow), or verification is off (plain).
	tag := "done    "
	if verified {
		tag = colour("32", "done    ")
	} else if j.e.cfg.verify && n.size > 0 {
		tag = colour("33", "done    ")
	}
	infof("%s  %s  (%s)", tag, j.shown, formatBytes(n.size))
	if verified {
		return resVerified, nil
	}
	return resDone, nil
}

// prepare reconciles the control file with the .part on disk and opens it.
func (j *job) prepare() error {
	e, n := j.e, j.n
	wantMode := modeParallel
	if e.cfg.sequential {
		wantMode = modeSequential
	}

	st := j.lk.ctl.get(n.handle)
	fi, statErr := os.Stat(j.part)
	partExists := statErr == nil

	if st != nil {
		reason := ""
		switch {
		case st.Size != n.size:
			reason = "size changed on MEGA"
		case !partExists && st.doneBytes() > 0:
			reason = ".part file is missing"
		case partExists && st.Mode == modeParallel && fi.Size() != n.size:
			reason = ".part file has the wrong size"
		case partExists && st.Mode == modeSequential && fi.Size() < st.contiguous():
			reason = ".part file is shorter than recorded"
		}
		if reason != "" {
			warnf("%s: cannot resume (%s) — starting over", j.shown, reason)
			st = nil
		}
	} else if partExists {
		warnf("%s: found a .part with no saved state — starting over", j.shown)
	}
	if st == nil {
		if partExists {
			if err := os.Remove(j.part); err != nil {
				return err
			}
		}
		st = &fileState{
			Path:   n.relPath,
			Size:   n.size,
			Mode:   wantMode,
			Verify: e.cfg.verify,
			Chunks: planChunks(n.size, e.cfg.chunkSize),
		}
		j.lk.ctl.put(n.handle, st)
	}

	switch {
	case st.Mode == modeParallel && wantMode == modeSequential && st.doneBytes() > 0:
		// Pre-checked for all files in checkModes; this is a backstop.
		return errModeMismatch
	case st.NoSparse:
		// Stays sequential: see the sparse check below.
	case st.Mode != wantMode:
		debugf("%s: switching %s → %s", j.shown, st.Mode, wantMode)
		j.lk.ctl.update(func() { st.Mode = wantMode })
	}

	j.verify = e.cfg.verify && st.Verify
	if e.cfg.verify && !st.Verify {
		warnf("%s: this download was started with --no-verify, so it cannot be verified", j.shown)
	}
	if !e.cfg.verify && st.Verify {
		j.lk.ctl.update(func() { st.Verify = false })
	}
	j.st = st
	j.fold, _ = aes.NewCipher(n.key.aes[:])

	f, err := os.OpenFile(j.part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", j.part, err)
	}
	j.f = f
	if st.Mode == modeParallel {
		// Size the file up front (sparsely) so every worker can write at its own
		// offset. On Windows the file must be flagged sparse first, or NTFS
		// allocates it all and zero-fills ahead of every out-of-order write.
		if err := markSparseFn(f); err != nil {
			if st.doneBytes() > 0 || len(st.Chunks) < 2 {
				// Already allocated by an earlier run, or a single chunk written from
				// the start (nothing out of order to zero-fill ahead of).
				// Already allocated by an earlier run; carry on as it was.
				debugf("%s: cannot mark the .part sparse (%v); continuing in parallel", j.shown, err)
			} else {
				// Without sparse support, sizing the file up front allocates all of it,
				// and Windows zero-fills ahead of every out-of-order write. Fetch this
				// file sequentially instead, and keep it that way on resume.
				debugf("%s: cannot mark the .part sparse (%v); switching to sequential", j.shown, err)
				e.noSparseOnce.Do(func() {
					noticef("%s can't hold sparse files; downloading one connection per file to avoid zero-filling (a different --output avoids this)",
						existingDir(j.lk.tree.baseDir(e.cfg.output)))
				})
				j.lk.ctl.update(func() { st.Mode, st.NoSparse = modeSequential, true })
			}
		}
	}
	if st.Mode == modeParallel {
		if err := f.Truncate(n.size); err != nil {
			return err
		}
	} else {
		// Sequential: the file only ever holds the checkpointed prefix.
		if err := f.Truncate(st.contiguous()); err != nil {
			return err
		}
	}
	return nil
}

// discardIfEmpty removes the .part and the file's state when it stopped (failed or
// was interrupted) before anything was checkpointed: there is nothing to resume,
// so they would only be clutter. j.f is already closed by run.
func (j *job) discardIfEmpty() {
	if j.st == nil {
		return
	}
	if done, _, ok := j.lk.ctl.progress(j.n.handle); !ok || done > 0 {
		return
	}
	if err := os.Remove(j.part); err != nil && !errors.Is(err, fs.ErrNotExist) {
		debugf("%s: removing empty .part: %v", j.shown, err)
		return
	}
	j.lk.ctl.remove(j.n.handle)
	debugf("%s: no progress made; removed the empty .part and its state", j.shown)
}

// runChunks downloads the pending chunks with the given number of workers. The
// first permanent failure cancels the others.
func (j *job) runChunks(ctx context.Context, pending []int, workers int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	queue := make(chan int, len(pending))
	for _, i := range pending {
		queue <- i
	}
	close(queue)

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for range workers {
		wg.Go(func() {
			for ci := range queue {
				if ctx.Err() != nil {
					return
				}
				if err := j.fetchChunk(ctx, ci); err != nil {
					once.Do(func() { firstErr = err; cancel() })
					return
				}
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// fetchChunk retries one chunk until it completes or the retry budget runs out.
func (j *job) fetchChunk(ctx context.Context, ci int) error {
	e := j.e
	c := j.st.Chunks[ci]
	label := fmt.Sprintf("%s chunk %d/%d", j.shown, ci+1, len(j.st.Chunks))
	attempts, throttled := 0, 0
	for {
		var before int64
		j.lk.ctl.update(func() { before = c.Done })
		if c.Start+before >= c.End {
			return nil
		}
		err := j.attemptChunk(ctx, ci, label)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ae apiError
		var le *localError
		if (errors.As(err, &ae) && ae.permanent()) || errors.As(err, &le) {
			return err
		}
		var after int64
		j.lk.ctl.update(func() { after = c.Done })

		var te *throttleError
		if errors.As(err, &ae) && ae.throttled() {
			// The link request was throttled: wait longer each time, like call does.
			te = &throttleError{what: fmt.Sprintf("MEGA API error %d", int(ae)), after: throttleDelay(throttled + 1)}
			err = te
		}
		if errors.As(err, &te) && throttled < maxThrottleWaits {
			throttled++
			debugf("%s: %v", label, err)
			j.row.setNote(fmt.Sprintf("rate limited, waiting %ds", int(te.after.Seconds())))
			if !sleepCtx(ctx, te.after) {
				return ctx.Err()
			}
			continue
		}

		if after-before >= progressReset {
			attempts = 0
		}
		attempts++
		if e.cfg.retries > 0 && attempts >= e.cfg.retries {
			return fmt.Errorf("chunk %d/%d: %w (gave up after %d attempts)", ci+1, len(j.st.Chunks), err, attempts)
		}
		backoff := min(time.Duration(attempts*attempts)*time.Second, 15*time.Second)
		budget := strconv.Itoa(e.cfg.retries)
		if e.cfg.retries == 0 {
			budget = "∞"
		}
		warnf("%s: %v — retry %d/%s in %s", label, err, attempts+1, budget, backoff)
		j.row.setNote(fmt.Sprintf("retry %d/%s in %ds", attempts+1, budget, int(backoff.Seconds())))
		if !sleepCtx(ctx, backoff) {
			return ctx.Err()
		}
	}
}

// attemptChunk makes one request for the rest of a chunk: fresh link from the API,
// fetch through a proxy, then decrypt + MAC + write as the bytes arrive.
func (j *job) attemptChunk(ctx context.Context, ci int, label string) error {
	e, n := j.e, j.n
	c := j.st.Chunks[ci]
	var done int64
	j.lk.ctl.update(func() { done = c.Done })
	off := c.Start + done

	// Download URLs are short-lived: a fresh one for every attempt.
	fi, err := e.api.getLink(ctx, j.lk.tree.folder, n.handle)
	if err != nil {
		return err
	}
	proxy := e.pickProxy()
	url := fi.URL + "/" + strconv.FormatInt(off, 10) + "-" + strconv.FormatInt(c.End-1, 10)
	if proxy != "" {
		url = proxy + "/" + url
	}
	debugf("%s: GET %s (bytes %d-%d) via %s", label, redactURL(fi.URL), off, c.End-1, hostOr(proxy, "direct"))

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", hostOr(proxy, "storage"), err)
	}
	defer resp.Body.Close()

	want := c.End - off
	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
		if resp.ContentLength >= 0 && resp.ContentLength != want {
			return fmt.Errorf("%s: server offered %d bytes, expected %d", hostOr(proxy, "storage"), resp.ContentLength, want)
		}
	case isThrottle(resp.StatusCode):
		return &throttleError{what: "HTTP " + strconv.Itoa(resp.StatusCode), after: retryAfter(resp, 20*time.Second)}
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("%s: HTTP %s %s", hostOr(proxy, "storage"), resp.Status, strings.TrimSpace(string(body)))
	}

	block, _ := aes.NewCipher(n.key.aes[:])
	stream := cipher.NewCTR(block, ctrIV(n.key.nonce, off))
	var mac *macStream
	if j.verify {
		mac = newMACStream(n.key.aes, n.key.nonce, off, n.size)
	}

	j.row.addConn(1)
	defer j.row.addConn(-1)

	// Stall watchdog: a silent connection becomes a retry instead of a hung worker.
	var lastRead atomic.Int64
	lastRead.Store(time.Now().UnixNano())
	var stalled atomic.Bool
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-reqCtx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastRead.Load())) > stallTimeout {
					stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	pos := off        // next byte to write
	checkpoint := off // last position recorded in the control file
	defer func() {
		// Bytes past the checkpoint will be fetched again; take them back off the bar.
		if lost := pos - checkpoint; lost > 0 {
			j.row.rewind(lost)
		}
	}()

	buf := make([]byte, readChunk)
	for pos < c.End {
		if err := e.lim.wait(ctx, len(buf)); err != nil {
			return err
		}
		room := min(int64(len(buf)), c.End-pos)
		nr, rerr := resp.Body.Read(buf[:room])
		if nr > 0 {
			lastRead.Store(time.Now().UnixNano())
			p := buf[:nr]
			stream.XORKeyStream(p, p)
			if _, werr := j.f.WriteAt(p, pos); werr != nil {
				return &localError{fmt.Errorf("writing %s: %w", j.part, werr)}
			}
			pos += int64(nr)
			j.row.advance(int64(nr))
			e.prog.addBytes(int64(nr))

			// Checkpoint at every MAC chunk boundary crossed. Without verification the
			// same boundaries are used, so the two modes resume identically.
			var macs [][16]byte
			newCk := checkpoint
			if mac != nil {
				macs = mac.Write(p)
				if len(macs) > 0 {
					newCk = mac.lastBoundary
				}
			} else if b := macBoundaryFloor(pos, c.End); b > checkpoint {
				newCk = b
			}
			if newCk > checkpoint {
				j.record(ci, newCk-c.Start, macs)
				checkpoint = newCk
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			if stalled.Load() {
				return fmt.Errorf("stalled: no data for %s", stallTimeout)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s: %w", hostOr(proxy, "storage"), rerr)
		}
	}
	if pos < c.End {
		return fmt.Errorf("connection closed early: got %s of %s", formatBytes(pos-off), formatBytes(want))
	}
	debugf("%s: complete", label)
	return nil
}

// macBoundaryFloor returns the largest MAC boundary <= pos, or end if pos == end.
func macBoundaryFloor(pos, end int64) int64 {
	if pos >= end {
		return end
	}
	b := macBoundaryAtOrAfter(pos)
	if b == pos {
		return b
	}
	// Step back one MAC chunk: the boundary before b.
	if b > macSteadyStart {
		return b - macSteadyStep
	}
	var prev, at int64
	for i := int64(1); at < b; i++ {
		prev = at
		at += i * macChunkBase
	}
	return prev
}

// record checkpoints a chunk's progress and folds whatever MACs can now be folded:
// the meta-MAC is a strict in-order CBC over chunk MACs, so it advances only through
// the leading run of chunks; MACs of chunks that finished early wait in the state.
func (j *job) record(ci int, done int64, macs [][16]byte) {
	j.lk.ctl.update(func() {
		st := j.st
		c := st.Chunks[ci]
		j.spaceWritten.Add(done - c.Done)
		c.Done = done
		for _, m := range macs {
			c.MACs = append(c.MACs, block16(m))
		}
		for st.Folded < len(st.Chunks) {
			front := st.Chunks[st.Folded]
			for _, m := range front.MACs {
				for i := range st.Meta {
					st.Meta[i] ^= m[i]
				}
				j.fold.Encrypt(st.Meta[:], st.Meta[:])
			}
			front.MACs = nil
			if !front.complete() {
				break
			}
			st.Folded++
		}
	})
}

// verifyOnDisk recomputes the MAC of a completed file (--verify-existing).
func (j *job) verifyOnDisk(ctx context.Context, path string) (bool, error) {
	n := j.n
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	j.row.begin(n.name, 0, n.size)
	j.row.setNote("verifying")
	mac := newMACStream(n.key.aes, n.key.nonce, 0, n.size)
	fold := newMetaFold(n.key.aes, [16]byte{})
	buf := make([]byte, 1<<20)
	for {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		nr, err := f.Read(buf)
		if nr > 0 {
			for _, m := range mac.Write(buf[:nr]) {
				fold.add(m)
			}
			j.row.advance(int64(nr))
			j.row.setNote("verifying")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}
	}
	return condense(fold.state) == n.key.mac, nil
}

// pickProxy chooses a proxy at random for each attempt ("" = direct).
func (e *engine) pickProxy() string {
	if e.cfg.direct || len(e.cfg.proxies) == 0 {
		return ""
	}
	return e.cfg.proxies[rand.Intn(len(e.cfg.proxies))]
}

// checkModes refuses to start when --sequential would resume a parallel partial
// download: its .part is preallocated with holes that sequential mode can't fill.
func checkModes(cfg *config, t *tree, ctl *controlFile, files []*node) error {
	if !cfg.sequential {
		return nil
	}
	var bad []string
	for _, n := range files {
		st := ctl.get(n.handle)
		if st == nil || st.Mode != modeParallel || st.doneBytes() == 0 {
			continue
		}
		if _, err := os.Stat(t.localPath(cfg.output, n) + ".part"); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		bad = append(bad, n.relPath)
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %d file(s) were partially downloaded in parallel mode and can't be resumed with --sequential:\n  %s\n"+
		"Resume them without --sequential, or delete their .part files and %s to restart sequentially",
		errModeMismatch, len(bad), strings.Join(bad, "\n  "), ctl.path)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// hostOr returns a URL's host, or def when the URL is empty.
func hostOr(u, def string) string {
	if u == "" {
		return def
	}
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	return s
}

// redactURL keeps a storage URL's host but hides the signed token, which is long
// and useless in logs.
func redactURL(u string) string {
	if i := strings.Index(u, "/dl/"); i >= 0 && len(u) > i+16 {
		return u[:i+16] + "…"
	}
	return u
}
