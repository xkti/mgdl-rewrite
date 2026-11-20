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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
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
func cli(argv []string) (code int) {
	cfg, err := parseArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mgdl: %v\n\n", err)
		usage(os.Stderr)
		return 2
	}
	con.tty = isTerminal(os.Stdout)
	con.errTTY = isTerminal(os.Stderr)
	con.verbose = cfg.verbose
	outcome := "stopped"
	if cfg.logFile != "" {
		if err := openLog(cfg.logFile, argv); err != nil {
			fmt.Fprintf(os.Stderr, "mgdl: %v\n", err)
			return 2
		}
		defer con.logFile.Close()
		defer func() { logExit(code, outcome) }()
	}
	if cfg.selectTUI && (!isTerminal(os.Stdin) || !con.tty) {
		errorf("--select needs an interactive terminal; use --path or --list instead")
		outcome = "not started: --select needs an interactive terminal"
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
				logExit(130, "forced exit, progress up to the last checkpoint saved")
				os.Exit(130)
			}
		}
	}()

	api := newAPIClient(newAPIHTTPClient(), cfg.retries)
	dl := newDownloadHTTPClient()

	st, runs, err := runLinks(ctx, cfg, api, dl)
	outcome = runOutcome(cfg, runs, err)
	if errors.Is(err, errSelectCancelled) {
		return 0 // the picker said so; there is nothing to report
	}
	// One link: its name over the totals. Several: a line per link, then the
	// totals for all of them.
	if len(runs) > 1 && !cfg.list && !cfg.dryRun {
		outf("\n%s", strings.Join(linkReport(runs, outputCols()), "\n"))
		if st.ran {
			outf("\n%s", strings.Join(st.lines(""), "\n"))
		}
	} else if st.ran {
		outf("\n%s\n%s", runTitle(runs), strings.Join(st.lines("  "), "\n"))
	}
	if errors.Is(err, context.Canceled) {
		stderrf("NOTE", "\ninterrupted — progress saved, run the same command again to resume")
		return 130
	}
	// Errors were printed as they happened, except files that failed during the
	// download: for a single link, list them here (with several, the report says).
	if len(runs) == 1 && runs[0].status == statusPartial {
		errorf("%v", runs[0].err)
	}
	if err != nil || st.failed > 0 {
		return 1
	}
	return 0
}

// runOutcome sums up how the run ended, in the report's words, for the log's
// closing line.
func runOutcome(cfg *config, runs []*linkRun, err error) string {
	switch {
	case errors.Is(err, errSelectCancelled):
		return "selection cancelled, nothing downloaded"
	case errors.Is(err, context.Canceled):
		return "interrupted, progress saved"
	case len(runs) == 0:
		return "nothing to do"
	}
	// --list and --dry-run leave the links that got through planning queued.
	for _, lr := range runs {
		if lr.status == statusQueued {
			lr.status = statusDone
			defer func() { lr.status = statusQueued }()
		}
	}
	_, out := groupOutcome(runs)
	if len(runs) > 1 {
		out = plural(len(runs), "link") + ": " + out
	}
	switch {
	case cfg.list:
		out = "listed; " + out
	case cfg.dryRun:
		out = "dry run, nothing downloaded; " + out
	}
	return out
}

// dryRun reports what a download would do, without transferring or writing anything.
// space is nil when it isn't to be reported here (--no-space-check, or several
// links, which dryRunSpace reports together).
func dryRun(cfg *config, t *tree, ctl *controlFile, files []*node, space *spaceReport) {
	outf("%s", t.title())
	outf("  output:  %s", t.baseDir(cfg.output))
	outf("  control: %s", ctl.path)
	if cfg.direct {
		outf("  proxy:   none (direct)")
	} else {
		outf("  proxy:   %s", strings.Join(cfg.proxies, ", "))
	}
	mode := fmt.Sprintf("parallel, %s × %s", plural(cfg.jobs, "file"), plural(cfg.connections, "connection"))
	if cfg.sequential {
		mode = fmt.Sprintf("sequential, %s at once", plural(cfg.jobs, "file"))
	}
	outf("  mode:    %s, %s chunks, verify %v\n", mode, formatBytes(cfg.chunkSize), cfg.verify)

	// Work out every line first, so the columns can be fitted to the terminal.
	var remain int64
	whats := make([]string, len(files))
	nameW, whatW := 0, 0
	for i, n := range files {
		final, blocker, err := finalPath(cfg, t, n)
		var what string
		if err != nil {
			what = "can't be saved: " + err.Error()
		} else if _, err := os.Stat(final); err == nil {
			what = "skip (already complete)"
			if cfg.verifyExisting && cfg.verify {
				what = "verify existing file"
			}
		} else if have, ok := resumable(ctl, n, final); ok {
			st := ctl.get(n.handle)
			what = fmt.Sprintf("resume from %s (%d%%, %s)", formatBytes(have), pct(have, n.size), st.Mode)
			if cfg.sequential && st.Mode == modeParallel && have > 0 {
				what += " — can't resume with --sequential"
			}
			remain += n.size - have
		} else {
			chunks := len(planChunks(n.size, cfg.chunkSize))
			what = fmt.Sprintf("download %s in %s", formatBytes(n.size), plural(chunks, "chunk"))
			remain += n.size
		}
		if blocker != nil && err == nil {
			what += fmt.Sprintf(", saved as %s (a different file has its name)", filepath.Base(final))
		}
		whats[i] = what
		nameW = max(nameW, dispWidth(n.relPath))
		whatW = max(whatW, dispWidth(what))
	}
	nameW, whatRoom := fitNames(outputCols(), 4, nameW, whatW)
	for i, n := range files {
		what := whats[i]
		if whatRoom > 0 {
			what = truncate(what, whatRoom)
		}
		outf("  %s  %s", padRight(n.relPath, nameW), what)
	}
	outf("\n%s selected, %s to transfer", plural(len(files), "file"), formatBytes(remain))
	if space != nil {
		printSpace(*space, "a real run would stop here")
	}
}

// printSpace is the dry run's free-space line; short says what a real run would do
// if the space isn't there.
func printSpace(r spaceReport, short string) {
	switch {
	case r.err != nil:
		outf("free space in %s is unknown (%v)", r.dir, r.err)
	case r.short():
		outf("%v %s — %s", errNoSpace, r.shortfall(), short)
	default:
		outf("%s to download, %s free in %s", formatBytes(r.need), formatBytes(r.free), r.dir)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// hideURL drops the request URL that net/http puts in its errors (`Get "URL":
// unexpected EOF`). A download URL carries a signed token and is long, so it is
// only shown with -v (logged here); warnings get the bare network error.
func hideURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		debugf("http: %v", ue)
		return ue.Err
	}
	return err
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
