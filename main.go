// mgdl downloads files and folders from MEGA.nz public links. Metadata comes from
// the MEGA API over the user's own connection; file data is fetched through a
// Cloudflare Worker proxy in chunks, each with a freshly requested download URL,
// and is decrypted (AES-128-CTR) and verified (CBC-MAC) on the fly as it is
// written. Interrupted downloads resume from a per-link control file.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "0.1.0"

func main() {
	enableVT()
	os.Exit(cli(os.Args[1:]))
}

// cli runs one invocation and returns the exit status: 0 success, 1 failures,
// 2 usage error, 130 interrupted.
func cli(argv []string) int {
	cfg, err := parseArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mgdl: %v\n\n", err)
		usage(os.Stderr)
		return 2
	}
	con.tty = isTerminal(os.Stdout)
	con.errTTY = isTerminal(os.Stderr)
	con.verbose = cfg.verbose
	if cfg.logFile != "" {
		if err := openLog(cfg.logFile); err != nil {
			fmt.Fprintf(os.Stderr, "mgdl: %v\n", err)
			return 2
		}
		defer con.logFile.Close()
	}
	if cfg.selectTUI && (!isTerminal(os.Stdin) || !con.tty) {
		fmt.Fprintln(os.Stderr, "mgdl: --select needs an interactive terminal; use --path or --list instead")
		return 2
	}
	debugf("config: %+v", *cfg)

	// The first Ctrl+C (or SIGTERM) cancels everything gracefully: workers stop, the
	// control file is saved, partial files stay for the next run. A second one kills
	// the process immediately.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		<-sigs
		first := time.Now()
		cancel()
		for range sigs {
			// Some tools (GNU timeout, process supervisors) deliver the same signal
			// twice in quick succession; only a deliberate second press forces out.
			if time.Since(first) > time.Second {
				if con.tty {
					os.Stdout.WriteString("\x1b[?25h") // the dashboard hid the cursor
				}
				fmt.Fprintln(os.Stderr, "\nforced exit — progress up to the last checkpoint (≤2s ago) is saved")
				os.Exit(130)
			}
		}
	}()

	api := newAPIClient(newAPIHTTPClient(), cfg.retries)
	dl := newDownloadHTTPClient()

	var total runStats
	failedLinks := 0
	for i, raw := range cfg.links {
		if ctx.Err() != nil {
			break
		}
		if len(cfg.links) > 1 {
			if i > 0 {
				fmt.Println()
			}
			infof("[%d/%d] %s", i+1, len(cfg.links), raw)
		}
		st, err := processLink(ctx, cfg, api, dl, raw)
		total.completed += st.completed
		total.verified += st.verified
		total.skipped += st.skipped
		total.failed += st.failed
		total.bytes += st.bytes
		total.elapsed += st.elapsed
		if err != nil {
			if errors.Is(err, context.Canceled) {
				break
			}
			failedLinks++
			fmt.Fprintf(os.Stderr, "mgdl: %v\n", err)
		}
	}

	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "\ninterrupted — progress saved, run the same command again to resume")
		return 130
	}
	if len(cfg.links) > 1 && !cfg.list && !cfg.dryRun {
		fmt.Printf("\nAll links: %d downloaded (%d verified), %d already present, %d failed; %s in %s\n",
			total.completed, total.verified, total.skipped, total.failed,
			formatBytes(total.bytes), formatDuration(total.elapsed))
	}
	if failedLinks > 0 || total.failed > 0 {
		return 1
	}
	return 0
}

// processLink resolves one link and lists, dry-runs, or downloads it.
func processLink(ctx context.Context, cfg *config, api *apiClient, hc *http.Client, raw string) (runStats, error) {
	l, err := parseLink(raw)
	if err != nil {
		return runStats{}, err
	}
	t, err := resolve(ctx, api, l)
	if err != nil {
		return runStats{}, fmt.Errorf("%s: %w", raw, err)
	}
	files, err := t.selectPaths(cfg.paths)
	if err != nil {
		return runStats{}, err
	}
	if cfg.selectTUI && t.root.isDir {
		files, err = runSelector(t, files)
		if errors.Is(err, errSelectCancelled) {
			infof("%s: selection cancelled, skipping", t.title())
			return runStats{}, nil
		}
		if err != nil {
			return runStats{}, err
		}
	}
	if cfg.list {
		printTree(os.Stdout, t, cfg.verbose)
		for _, note := range renameNotes(t, t.files) {
			noticef("%s", note)
		}
		return runStats{}, nil
	}
	for _, note := range renameNotes(t, files) {
		noticef("%s", note)
	}
	if len(files) == 0 {
		infof("%s: nothing selected", t.title())
		return runStats{}, nil
	}
	ctl, err := loadControl(t.baseDir(cfg.output), l.handle())
	if err != nil {
		return runStats{}, err
	}
	space := checkLinkSpace(cfg, t, ctl, files)
	if cfg.dryRun {
		dryRun(cfg, t, ctl, files, space)
		return runStats{}, nil
	}
	if err := checkModes(cfg, t, ctl, files); err != nil {
		return runStats{}, err
	}
	var guard *spaceGuard
	switch {
	case cfg.noSpaceCheck:
	case space.err != nil:
		warnf("cannot determine free space in %s (%v); not checking it", space.dir, space.err)
	case space.short():
		return runStats{}, space.asError()
	default:
		debugf("space: %s to download, %s free in %s", formatBytes(space.need), formatBytes(space.free), space.dir)
		guard = newSpaceGuard(space.dir)
	}
	return downloadLink(ctx, cfg, api, hc, t, ctl, files, guard)
}

// downloadLink runs the file worker pool for one link.
func downloadLink(ctx context.Context, cfg *config, api *apiClient, hc *http.Client,
	t *tree, ctl *controlFile, files []*node, space *spaceGuard) (runStats, error) {

	prog := newProgress(t.title(), files, cfg.jobs)
	e := &engine{cfg: cfg, api: api, http: hc, lim: newRateLimiter(cfg.limitRate), tree: t, ctl: ctl, prog: prog, space: space}

	// The saver outlives ctx so the final checkpoint is written after Ctrl+C.
	saveCtx, stopSaver := context.WithCancel(context.Background())
	saverDone := make(chan struct{})
	go func() { ctl.run(saveCtx); close(saverDone) }()

	prog.start(ctx)

	jobs := make(chan *node)
	go func() {
		defer close(jobs)
		for _, n := range files {
			select {
			case jobs <- n:
			case <-ctx.Done():
				return
			}
		}
	}()

	var mu sync.Mutex
	var failures []string
	var wg sync.WaitGroup
	for i := range prog.slots {
		wg.Add(1)
		go func(s *slot) {
			defer wg.Done()
			for n := range jobs {
				res, err := e.fetchFile(ctx, n, s)
				s.clear()
				switch {
				case err == nil && res == resSkipped:
					prog.fileSkipped(n.size)
				case err == nil:
					prog.fileDone(n.size, res == resVerified)
				case errors.Is(err, context.Canceled):
					return
				default:
					prog.fileFailed(n.size)
					infof("%s  %s: %v", colour("31", "FAILED  "), n.relPath, err)
					mu.Lock()
					failures = append(failures, n.relPath)
					mu.Unlock()
				}
			}
		}(prog.slots[i])
	}
	wg.Wait()
	prog.stop()

	stopSaver()
	<-saverDone
	if err := ctl.save(); err != nil {
		warnf("saving control file: %v", err)
	}

	if ctx.Err() != nil {
		return runStats{}, context.Canceled
	}
	st := prog.summary()
	if len(failures) > 0 {
		return st, fmt.Errorf("%d file(s) failed: %s", len(failures), strings.Join(failures, ", "))
	}
	return st, nil
}

// dryRun reports what a download would do, without transferring or writing anything.
func dryRun(cfg *config, t *tree, ctl *controlFile, files []*node, space spaceReport) {
	fmt.Printf("%s\n", t.title())
	fmt.Printf("  output:  %s\n", t.baseDir(cfg.output))
	fmt.Printf("  control: %s\n", ctl.path)
	if cfg.direct {
		fmt.Printf("  proxy:   none (direct)\n")
	} else {
		fmt.Printf("  proxy:   %s\n", strings.Join(cfg.proxies, ", "))
	}
	mode := fmt.Sprintf("parallel, %d file(s) × %d connection(s)", cfg.jobs, cfg.connections)
	if cfg.sequential {
		mode = fmt.Sprintf("sequential, %d file(s) at once", cfg.jobs)
	}
	fmt.Printf("  mode:    %s, %s chunks, verify %v\n\n", mode, formatBytes(cfg.chunkSize), cfg.verify)

	var remain int64
	nameW := 0
	for _, n := range files {
		nameW = max(nameW, min(len([]rune(n.relPath)), 50))
	}
	for _, n := range files {
		final := t.localPath(cfg.output, n)
		var what string
		if fi, err := os.Stat(final); err == nil && fi.Size() == n.size {
			what = "skip (already complete)"
			if cfg.verifyExisting && cfg.verify {
				what = "verify existing file"
			}
		} else if err == nil {
			what = fmt.Sprintf("re-download (local size %s is wrong)", formatBytes(fi.Size()))
			remain += n.size
		} else if st := ctl.get(n.handle); st != nil && st.Size == n.size && fileExists(final+".part") {
			have := st.doneBytes()
			what = fmt.Sprintf("resume from %s (%d%%, %s)", formatBytes(have), pct(have, n.size), st.Mode)
			if cfg.sequential && st.Mode == modeParallel && have > 0 {
				what += " — can't resume with --sequential"
			}
			remain += n.size - have
		} else {
			chunks := len(planChunks(n.size, cfg.chunkSize))
			what = fmt.Sprintf("download %s in %d chunk(s)", formatBytes(n.size), chunks)
			remain += n.size
		}
		fmt.Printf("  %-*s  %s\n", nameW, truncate(n.relPath, 50), what)
	}
	fmt.Printf("\n%d file(s) selected, %s to transfer\n", len(files), formatBytes(remain))
	switch {
	case cfg.noSpaceCheck:
	case space.err != nil:
		fmt.Printf("free space in %s is unknown (%v)\n", space.dir, space.err)
	case space.short():
		fmt.Printf("%s\n", colour("31", fmt.Sprintf("NOT ENOUGH SPACE: %s needed on disk, %s free in %s — a real run would stop here",
			formatBytes(space.need+spaceMargin(space.need)), formatBytes(space.free), space.dir)))
	default:
		fmt.Printf("%s needed on disk, %s free in %s\n", formatBytes(space.need), formatBytes(space.free), space.dir)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// newAPIHTTPClient is for MEGA API calls: direct, compressed, quick to time out.
func newAPIHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{Transport: tr, Timeout: 5 * time.Minute}
}

// newDownloadHTTPClient is tuned for long binary transfers: no compression, plenty
// of connection reuse, and a header timeout so a dead proxy fails fast.
func newDownloadHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = true
	tr.MaxIdleConnsPerHost = 64
	tr.ResponseHeaderTimeout = 60 * time.Second
	tr.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: tr}
}
