package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A run takes every link given on the command line through two phases:
//
//  1. Plan: each link is parsed, resolved, selected and checked, in command-line
//     order, before anything is downloaded. Names are made unique across links
//     (mergeTrees), and free space is checked for everything at once.
//  2. Download: the selected files of all links that passed share one queue, one
//     pool of -j workers and one dashboard, so the next link's files start as soon
//     as a worker is free rather than after the previous link's slowest file.
//
// Each link keeps its own control file, so resuming works link by link as before.

// linkStatus is how a link's part of the run ended.
type linkStatus int

const (
	statusQueued      linkStatus = iota // planned and waiting for (or in) the download phase
	statusDone                          // every selected file finished or was already there
	statusPartial                       // some files failed
	statusInterrupted                   // Ctrl+C before it finished
	statusSkipped                       // nothing to do: nothing selected, cancelled, a repeat
	statusNotStarted                    // opened, but a check stopped it before downloading
	statusFailed                        // couldn't be opened at all
)

// linkRun is one link's journey through a run.
type linkRun struct {
	idx  int // position on the command line, from 1
	raw  string
	link *megaLink // nil if the link didn't parse

	tree  *tree
	ctl   *controlFile
	files []*node     // selected
	space *spaceGuard // nil with --no-space-check, or when free space is unknown

	matched bool // the link was opened and -p applied to it

	throttleNoted atomic.Bool // the "rate limited" notice was shown for its lookup

	status linkStatus
	note   string // short reason for skipped/not started/failed
	err    error  // the full error, if any

	mu       sync.Mutex
	done     int // files finished or already present
	failures []string
}

// stop ends the link's part of the run with a status and the error that caused it.
func (lr *linkRun) stop(s linkStatus, err error) {
	lr.status, lr.err = s, err
	if err != nil {
		lr.note = firstLine(err.Error())
	}
}

// label names the link in the report: its local name once resolved, else its
// position and ID (the full link was printed with its error).
func (lr *linkRun) label() string {
	switch {
	case lr.tree != nil:
		return lr.tree.root.name
	case lr.link != nil:
		return fmt.Sprintf("link %d (%s)", lr.idx, lr.link.handle())
	}
	return fmt.Sprintf("link %d", lr.idx)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// linkError prints a link's error as it happens. With several links, it says which.
func linkError(lr *linkRun, total int, err error) {
	if total > 1 {
		fmt.Fprintf(os.Stderr, "mgdl: link %d: %v\n", lr.idx, err)
		return
	}
	fmt.Fprintf(os.Stderr, "mgdl: %v\n", err)
}

// runLinks plans and downloads every link in cfg.links. The error is
// context.Canceled after Ctrl+C, errSelectCancelled if the picker was cancelled,
// otherwise all links' errors joined (nil if none).
func runLinks(ctx context.Context, cfg *config, api *apiClient, hc *http.Client) (runStats, []*linkRun, error) {
	runs, cancelled := planLinks(ctx, cfg, api)
	if cancelled {
		return runStats{}, runs, errSelectCancelled
	}
	var queued []*linkRun
	for _, lr := range runs {
		if lr.status == statusQueued {
			queued = append(queued, lr)
		}
	}
	var st runStats
	if len(queued) > 0 && !cfg.list && !cfg.dryRun {
		st = downloadAll(ctx, cfg, api, hc, queued)
	}
	if ctx.Err() != nil {
		for _, lr := range runs {
			if lr.status == statusQueued {
				lr.status = statusInterrupted
			}
		}
		return st, runs, context.Canceled
	}
	var errs []error
	for _, lr := range runs {
		if lr.err != nil {
			errs = append(errs, lr.err)
		}
	}
	return st, runs, errors.Join(errs...)
}

// planLinks is phase 1. Links that can't go ahead are marked and reported; the
// rest come back as statusQueued, with files selected and space checked.
// cancelled reports that the user cancelled the picker.
func planLinks(ctx context.Context, cfg *config, api *apiClient) (runs []*linkRun, cancelled bool) {
	total := len(cfg.links)
	runs = make([]*linkRun, total)
	for i, raw := range cfg.links {
		runs[i] = &linkRun{idx: i + 1, raw: raw}
	}

	// While the picker has the screen, messages wait until it closes.
	picking := cfg.selectTUI && !cfg.list
	var held []func()
	say := func(f func()) {
		if picking {
			held = append(held, f)
		} else {
			f()
		}
	}

	// Links are resolved a few at a time but dealt with strictly in command-line
	// order, so names given to clashes don't depend on which link loaded first.
	resCtx, stopResolving := context.WithCancel(ctx)
	defer stopResolving()
	// A rate limit while looking links up is shown once per link, or, while the
	// picker has the screen, in its header.
	var wait lookupWait
	note := func(lr *linkRun, code int, d time.Duration) {
		if picking {
			wait.set(code, d)
			return
		}
		if lr.throttleNoted.CompareAndSwap(false, true) {
			what := "looking up the link"
			if total > 1 {
				what = fmt.Sprintf("looking up link %d", lr.idx)
			}
			noticef("%s: rate limited (API %d), waiting %s", what, code, d.Round(time.Second))
		}
	}
	arrivals := resolveInOrder(resCtx, api, runs, note)
	merger := newNameMerger()
	seen := map[string]*linkRun{}
	pathHit := make([]bool, len(cfg.paths)) // each -p matched in at least one link
	accept := func(lr *linkRun) {
		switch lr.status {
		case statusInterrupted:
			return
		case statusFailed:
			say(func() { linkError(lr, total, lr.err) })
			return
		}
		// A link whose share or file an earlier link already resolved is the same
		// thing given twice and would share its control file: drop it. (Only links
		// that worked count, so a repeat with a working key still runs.)
		if first := seen[lr.link.handle()]; first != nil {
			lr.tree = first.tree // reported alongside the first
			lr.status, lr.note = statusSkipped, fmt.Sprintf("same as link %d", first.idx)
			say(func() { noticef("link %d is the same as link %d, skipping it", lr.idx, first.idx) })
			return
		}
		seen[lr.link.handle()] = lr
		lr.tree.showRoot = total > 1
		merger.add(lr.tree)
		if len(cfg.paths) > 0 && !lr.tree.root.isDir && total == 1 {
			err := errors.New("--path only applies to folder links")
			lr.stop(statusFailed, err)
			say(func() { linkError(lr, total, err) })
			return
		}
		files, hit := lr.tree.matchPaths(cfg.paths)
		for i, h := range hit {
			pathHit[i] = pathHit[i] || h
		}
		lr.files = files
		lr.matched = true
	}

	if picking {
		goOn, cancelled := pick(ctx, runs, arrivals, accept, stopResolving, &wait)
		for _, f := range held {
			f()
		}
		if !goOn {
			return runs, cancelled
		}
		// -p only preselected; what the user then chose stands.
		if err := unmatchedPaths(cfg.paths, pathHit, runs); err != nil {
			warnf("%v", err)
		}
	} else {
		for lr := range arrivals {
			accept(lr)
		}
		// A -p that matches nothing anywhere is most likely a typo: stop.
		if err := unmatchedPaths(cfg.paths, pathHit, runs); err != nil {
			fmt.Fprintf(os.Stderr, "mgdl: %v\n", err)
			for _, lr := range runs {
				if lr.status == statusQueued {
					lr.stop(statusNotStarted, err)
				}
			}
			return runs, false
		}
	}

	// List, report renames, and load each link's state.
	for _, lr := range runs {
		if lr.status != statusQueued || lr.tree == nil {
			continue
		}
		t := lr.tree
		if total > 1 && (cfg.list || cfg.dryRun) {
			if lr.idx > 1 {
				fmt.Println()
			}
			infof("[%d/%d] %s", lr.idx, total, lr.raw)
		}
		if cfg.list {
			printTree(os.Stdout, t, cfg.verbose)
			for _, note := range renameNotes(t, t.files) {
				noticef("%s", note)
			}
			continue
		}
		for _, note := range renameNotes(t, lr.files) {
			noticef("%s", note)
		}
		if len(lr.files) == 0 {
			lr.status, lr.note = statusSkipped, "nothing selected"
			if len(cfg.paths) > 0 && !cfg.selectTUI {
				lr.note = "nothing matches --path"
			}
			infof("%s: %s", t.title(), lr.note)
			continue
		}
		var err error
		if lr.ctl, err = loadControl(t.baseDir(cfg.output), lr.link.handle()); err != nil {
			lr.stop(statusFailed, err)
			linkError(lr, total, err)
			continue
		}
		if cfg.dryRun {
			// With several links, space is reported once for all of them, below.
			var space *spaceReport
			if total == 1 && !cfg.noSpaceCheck {
				r := spaceFor(cfg, []*linkRun{lr})
				space = &r
			}
			dryRun(cfg, t, lr.ctl, lr.files, space)
			continue
		}
		if err := checkModes(cfg, t, lr.ctl, lr.files); err != nil {
			lr.stop(statusNotStarted, err)
			linkError(lr, total, err)
		}
	}
	switch {
	case cfg.list || cfg.noSpaceCheck:
	case cfg.dryRun && total > 1:
		dryRunSpace(cfg, runs)
	case !cfg.dryRun:
		checkSpace(cfg, runs, total)
	}
	return runs, false
}

// lookupWait is the latest rate-limit wait while links are looked up, for the
// picker's header. Lookups run in the background, so it's atomic.
type lookupWait struct {
	until atomic.Int64 // UnixNano
	code  atomic.Int64
}

func (w *lookupWait) set(code int, d time.Duration) {
	w.code.Store(int64(code))
	w.until.Store(time.Now().Add(d).UnixNano())
}

// remaining is the time left in the current wait (0 if none) and its API code.
func (w *lookupWait) remaining() (time.Duration, int) {
	if w == nil || w.until.Load() == 0 {
		return 0, 0
	}
	return max(0, time.Until(time.Unix(0, w.until.Load()))), int(w.code.Load())
}

// unmatchedPaths reports the -p entries that matched nothing in any link that
// was opened (nil if all matched, or no link was opened to match them in).
func unmatchedPaths(paths []string, hit []bool, runs []*linkRun) error {
	var trees []*tree
	for _, lr := range runs {
		if lr.matched {
			trees = append(trees, lr.tree)
		}
	}
	var missing []string
	for i, p := range paths {
		if !hit[i] {
			missing = append(missing, fmt.Sprintf("%q", p))
		}
	}
	if len(missing) == 0 || len(trees) == 0 {
		return nil
	}
	where := "any link"
	if len(trees) == 1 {
		where = fmt.Sprintf("%q", trees[0].title())
	}
	return fmt.Errorf("--path %s matches nothing in %s (paths are relative to each link's folder, see --list)",
		strings.Join(missing, ", "), where)
}

// runPickerFn is the interactive picker; a variable so tests can play the user.
var runPickerFn = runPicker

// pick runs the --select picker while links arrive, then narrows each link's files
// to the choice. Links still loading when the picker closes are skipped. It
// reports whether planning should go on (false: cancelled, interrupted, or the
// terminal couldn't be used), and whether the user cancelled.
func pick(ctx context.Context, runs []*linkRun, arrivals <-chan *linkRun, accept func(*linkRun), stopResolving func(), wait *lookupWait) (goOn, cancelled bool) {
	s := newSelector(len(runs))
	s.lookup = wait
	arrived := map[*linkRun]bool{}
	ev, err := runPickerFn(ctx, s, arrivals, func(lr *linkRun) {
		arrived[lr] = true
		accept(lr)
		switch {
		case lr.status == statusFailed:
			s.addBroken(lr)
		case lr.status == statusQueued:
			s.addTree(lr.tree, lr.files)
		default:
			s.addSkipped()
		}
	})
	stopResolving()
	for range arrivals { // wait for the resolvers to finish before touching their links
	}

	interrupted := ctx.Err() != nil
	for _, lr := range runs {
		switch {
		case arrived[lr] && lr.status != statusQueued:
			// already settled: failed, a repeat
		case interrupted:
			lr.status = statusInterrupted
		case err != nil:
			lr.stop(statusFailed, err)
		case ev == pickerCancelled:
			lr.status, lr.note = statusSkipped, "selection cancelled"
		case !arrived[lr]:
			lr.status, lr.note = statusSkipped, "still loading when the selection was made"
		default:
			lr.files = s.chosenFiles(lr.tree)
		}
	}
	switch {
	case interrupted:
		return false, false
	case err != nil:
		fmt.Fprintf(os.Stderr, "mgdl: %v\n", err)
		return false, false
	case ev == pickerCancelled:
		infof("selection cancelled, nothing downloaded")
		return false, true
	}
	return true, false
}

// resolveInOrder resolves links a few at a time and delivers each, in order, once
// it and every link before it are done. The channel closes after the last one;
// the caller must drain it (cancel ctx to make the rest finish quickly).
// note hears about rate-limit waits during each link's lookup.
func resolveInOrder(ctx context.Context, api *apiClient, runs []*linkRun, note func(*linkRun, int, time.Duration)) <-chan *linkRun {
	const parallel = 4
	out := make(chan *linkRun)
	done := make([]chan struct{}, len(runs))
	for i := range done {
		done[i] = make(chan struct{})
	}
	go func() {
		sem := make(chan struct{}, parallel)
		for i, lr := range runs {
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; close(done[i]) }()
				resolveOne(withThrottleNote(ctx, func(code int, d time.Duration) { note(lr, code, d) }), api, lr)
			}()
		}
	}()
	go func() {
		defer close(out)
		for i, lr := range runs {
			<-done[i]
			out <- lr
		}
	}()
	return out
}

// resolveOne parses and resolves a link, setting its link and tree, or its failure.
func resolveOne(ctx context.Context, api *apiClient, lr *linkRun) {
	if ctx.Err() != nil {
		lr.status = statusInterrupted
		return
	}
	l, err := parseLink(lr.raw)
	if err != nil {
		lr.stop(statusFailed, err)
		return
	}
	lr.link = l
	t, err := resolve(ctx, api, l)
	switch {
	case ctx.Err() != nil:
		lr.status = statusInterrupted
	case err != nil:
		lr.stop(statusFailed, err)
		lr.err = fmt.Errorf("%s: %w", lr.raw, err)
	default:
		lr.tree = t
	}
}

// queuedRuns is the links still going ahead, with files selected.
func queuedRuns(runs []*linkRun) []*linkRun {
	var out []*linkRun
	for _, lr := range runs {
		if lr.status == statusQueued && lr.files != nil {
			out = append(out, lr)
		}
	}
	return out
}

// checkSpace is the up-front free-space check for the whole run: if the queued
// links' combined need doesn't fit, none of them start.
func checkSpace(cfg *config, runs []*linkRun, total int) {
	queued := queuedRuns(runs)
	if len(queued) == 0 {
		return
	}
	r := spaceFor(cfg, queued)
	switch {
	case r.err != nil:
		warnf("cannot determine free space in %s (%v); not checking it", r.dir, r.err)
	case r.short():
		err := r.asError()
		if total > 1 {
			err = fmt.Errorf("%d link(s) not started: %w", len(queued), err)
		}
		fmt.Fprintf(os.Stderr, "mgdl: %v\n", err)
		for _, lr := range queued {
			lr.stop(statusNotStarted, err)
			lr.note = errNoSpace.Error()
		}
	default:
		debugf("space: %s to download, %s free in %s", formatBytes(r.need), formatBytes(r.free), r.dir)
		guard := newSpaceGuard(r.dir)
		for _, lr := range queued {
			lr.space = guard
		}
	}
}

// dryRunSpace closes a dry run of several links with what they need together,
// checked the way a real run would.
func dryRunSpace(cfg *config, runs []*linkRun) {
	queued := queuedRuns(runs)
	if len(queued) == 0 {
		return
	}
	files := 0
	for _, lr := range queued {
		files += len(lr.files)
	}
	fmt.Printf("\nAll links: %d file(s) from %d link(s)\n", files, len(queued))
	printSpace(spaceFor(cfg, queued), fmt.Sprintf("a real run wouldn't start %d link(s)", len(queued)))
}

// downloadAll is phase 2: one queue of every queued link's files, in link order,
// worked through by -j workers under one dashboard.
func downloadAll(ctx context.Context, cfg *config, api *apiClient, hc *http.Client, runs []*linkRun) runStats {
	type item struct {
		lr *linkRun
		n  *node
	}
	var items []item
	var files []*node
	for _, lr := range runs {
		for _, n := range lr.files {
			items = append(items, item{lr, n})
			files = append(files, n)
		}
	}

	prog := newProgress(runTitle(runs), files, cfg.jobs)
	e := &engine{cfg: cfg, api: api, http: hc, lim: newRateLimiter(cfg.limitRate), prog: prog, multi: len(runs) > 1}

	// The saver outlives ctx so the final checkpoints are written after Ctrl+C.
	saveCtx, stopSaver := context.WithCancel(context.Background())
	saverDone := make(chan struct{})
	ctls := make([]*controlFile, len(runs))
	for i, lr := range runs {
		ctls[i] = lr.ctl
	}
	go func() { saveLoop(saveCtx, ctls); close(saverDone) }()

	prog.start(ctx)

	queue := make(chan item)
	go func() {
		defer close(queue)
		for _, it := range items {
			select {
			case queue <- it:
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for _, s := range prog.slots {
		wg.Go(func() {
			for it := range queue {
				res, err := e.fetchFile(ctx, it.lr, it.n, s)
				s.clear()
				switch {
				case err == nil && res == resSkipped:
					prog.fileSkipped(it.n.size)
				case err == nil:
					prog.fileDone(it.n.size, res == resVerified)
				case errors.Is(err, context.Canceled):
					return
				default:
					prog.fileFailed(it.n.size)
					infof("%s  %s: %v", colour("31", "FAILED  "), displayPath(it.lr, it.n, e.multi), err)
					it.lr.mu.Lock()
					it.lr.failures = append(it.lr.failures, it.n.relPath)
					it.lr.mu.Unlock()
					continue
				}
				it.lr.mu.Lock()
				it.lr.done++
				it.lr.mu.Unlock()
			}
		})
	}
	wg.Wait()
	prog.stop()

	stopSaver()
	<-saverDone
	for _, c := range ctls {
		if err := c.save(); err != nil {
			warnf("saving control file: %v", err)
		}
	}
	if ctx.Err() != nil {
		return runStats{}
	}
	for _, lr := range runs {
		if len(lr.failures) == 0 {
			lr.status = statusDone
			continue
		}
		lr.status = statusPartial
		lr.note = fmt.Sprintf("%d of %d files failed", len(lr.failures), len(lr.files))
		lr.err = fmt.Errorf("%d file(s) failed: %s", len(lr.failures), strings.Join(lr.failures, ", "))
		if len(runs) > 1 {
			lr.err = fmt.Errorf("%s: %w", lr.label(), lr.err)
		}
	}
	return prog.summary()
}

// displayPath names a file in status lines: with several links, from the link's
// folder down, so files from different links can be told apart.
func displayPath(lr *linkRun, n *node, multi bool) string {
	if multi && lr.tree.root.isDir {
		return lr.tree.root.name + "/" + n.relPath
	}
	return n.relPath
}

// runTitle heads the dashboard and summary. Links with the same local name are one
// group, as their files land in the same place.
func runTitle(runs []*linkRun) string {
	names := groupNames(runs)
	if len(names) == 1 {
		title := runs[0].tree.title()
		if len(runs) > 1 {
			title += fmt.Sprintf("  (%d links)", len(runs))
		}
		return title
	}
	return fmt.Sprintf("%d links: %s", len(runs), strings.Join(names, ", "))
}

// groupNames lists the distinct local names (ignoring case) in order of appearance.
func groupNames(runs []*linkRun) []string {
	seen := map[string]bool{}
	var names []string
	for _, lr := range runs {
		k := strings.ToLower(lr.label())
		if !seen[k] {
			seen[k] = true
			names = append(names, lr.label())
		}
	}
	return names
}

// statusWords are the report's plain words for each status, singular.
var statusWords = map[linkStatus]string{
	statusDone:        "done",
	statusPartial:     "partly failed",
	statusInterrupted: "interrupted",
	statusSkipped:     "skipped",
	statusNotStarted:  "not started",
	statusFailed:      "couldn't be opened",
}

// reportOrder is the order counts appear in the report's header.
var reportOrder = []linkStatus{statusDone, statusPartial, statusInterrupted, statusNotStarted, statusFailed, statusSkipped}

// linkReport is the closing per-link report for a run with several links: a count
// of links by outcome, then one line per group of links sharing a local name.
func linkReport(runs []*linkRun) []string {
	counts := map[linkStatus]int{}
	type group struct {
		label string
		runs  []*linkRun
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, lr := range runs {
		counts[lr.status]++
		k := strings.ToLower(lr.label())
		if lr.tree == nil {
			k = fmt.Sprintf("\x00%d", lr.idx) // unresolved links are never grouped
		}
		g := byKey[k]
		if g == nil {
			g = &group{label: lr.label()}
			byKey[k] = g
			groups = append(groups, g)
		}
		g.runs = append(g.runs, lr)
	}

	var head []string
	for _, s := range reportOrder {
		if counts[s] > 0 {
			head = append(head, fmt.Sprintf("%d %s", counts[s], statusWords[s]))
		}
	}
	lines := []string{"Links: " + strings.Join(head, ", ")}

	labelW := 0
	labels := make([]string, len(groups))
	for i, g := range groups {
		labels[i] = g.label
		if len(g.runs) > 1 {
			labels[i] += fmt.Sprintf(" (%d links)", len(g.runs))
		}
		labelW = max(labelW, min(dispWidth(labels[i]), 40))
	}
	for i, g := range groups {
		lines = append(lines, fmt.Sprintf("  %s  %s", padRight(truncate(labels[i], 40), labelW), groupOutcome(g.runs)))
	}
	return lines
}

// groupOutcome describes how a group of links ended: the shared status with its
// detail, or a count per status when they differ.
func groupOutcome(runs []*linkRun) string {
	first := runs[0].status
	same := true
	for _, lr := range runs {
		same = same && lr.status == first
	}
	if !same {
		counts := map[linkStatus]int{}
		for _, lr := range runs {
			counts[lr.status]++
		}
		var parts []string
		for _, s := range reportOrder {
			if counts[s] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", counts[s], statusWords[s]))
			}
		}
		return colourStatus(worstStatus(runs), strings.Join(parts, ", "))
	}
	word := statusWords[first]
	switch first {
	case statusPartial:
		failed, files := 0, 0
		for _, lr := range runs {
			failed += len(lr.failures)
			files += len(lr.files)
		}
		word = fmt.Sprintf("%d of %d files failed", failed, files)
	case statusSkipped, statusNotStarted, statusFailed:
		// The reason, if every link in the group shares it.
		note := runs[0].note
		for _, lr := range runs {
			if lr.note != note {
				note = ""
			}
		}
		switch {
		case note == "":
		case first == statusSkipped:
			word = fmt.Sprintf("%s (%s)", word, note)
		default:
			word += ": " + note
		}
	}
	return colourStatus(first, word)
}

func worstStatus(runs []*linkRun) linkStatus {
	worst := statusDone
	rank := func(s linkStatus) int {
		switch s {
		case statusFailed:
			return 5
		case statusNotStarted:
			return 4
		case statusPartial:
			return 3
		case statusInterrupted:
			return 2
		case statusDone:
			return 1
		}
		return 0 // skipped
	}
	for i, lr := range runs {
		if i == 0 || rank(lr.status) > rank(worst) {
			worst = lr.status
		}
	}
	return worst
}

func colourStatus(s linkStatus, text string) string {
	switch s {
	case statusDone:
		return colour("32", text)
	case statusPartial, statusFailed:
		return colour("31", text)
	case statusInterrupted, statusNotStarted:
		return colour("33", text)
	}
	return text
}
