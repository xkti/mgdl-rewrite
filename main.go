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

	st, runs, err := runLinks(ctx, cfg, api, dl)
	if errors.Is(err, errSelectCancelled) {
		return 0 // the picker said so; there is nothing to report
	}
	if len(runs) > 1 && !cfg.list && !cfg.dryRun {
		fmt.Println()
		for _, line := range linkReport(runs) {
			fmt.Println(line)
		}
	}
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "\ninterrupted — progress saved, run the same command again to resume")
		return 130
	}
	// Errors were printed as they happened, except files that failed during the
	// download: for a single link, list them here (with several, the report says).
	if len(runs) == 1 && runs[0].status == statusPartial {
		fmt.Fprintf(os.Stderr, "mgdl: %v\n", runs[0].err)
	}
	if err != nil || st.failed > 0 {
		return 1
	}
	return 0
}

// dryRun reports what a download would do, without transferring or writing anything.
// space is nil when it isn't to be reported here (--no-space-check, or several
// links, which dryRunSpace reports together).
func dryRun(cfg *config, t *tree, ctl *controlFile, files []*node, space *spaceReport) {
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
	if space != nil {
		printSpace(*space, "a real run would stop here")
	}
}

// printSpace is the dry run's free-space line; short says what a real run would do
// if the space isn't there.
func printSpace(r spaceReport, short string) {
	switch {
	case r.err != nil:
		fmt.Printf("free space in %s is unknown (%v)\n", r.dir, r.err)
	case r.short():
		fmt.Printf("%s\n", colour("31", fmt.Sprintf("NOT ENOUGH SPACE: %s needed on disk, %s free in %s — %s",
			formatBytes(r.need+spaceMargin(r.need)), formatBytes(r.free), r.dir, short)))
	default:
		fmt.Printf("%s needed on disk, %s free in %s\n", formatBytes(r.need), formatBytes(r.free), r.dir)
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
