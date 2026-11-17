package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// barWidth is the width of each per-file progress bar; it narrows to minBarWidth
// when the terminal is too narrow for the file names.
const (
	barWidth    = 20
	minBarWidth = 8
)

// refreshInterval is how often the dashboard redraws.
const refreshInterval = 200 * time.Millisecond

// plainInterval is how often a status line is printed when stdout is not a terminal.
const plainInterval = 10 * time.Second

// speedWindow is how much recent history the live aggregate rate averages over: long
// enough to smooth MEGA's swings, short enough to reflect a slow tail promptly.
const speedWindow = 15 * time.Second

// minSpeedSpan is the least history needed before a rate is worth showing.
const minSpeedSpan = 2 * time.Second

type speedSample struct {
	t     time.Time
	bytes int64
}

// slot is one row of the dashboard, owned by a single file job at a time.
type slot struct {
	mu     sync.Mutex
	active bool
	name   string
	base   int64 // bytes already on disk when this attempt started
	done   int64 // bytes present now
	total  int64
	conns  int    // connections currently transferring
	note   string // transient status such as "retry 2/5 in 4s"
	speed  float64

	lastBytes int64
	lastTime  time.Time
	start     time.Time // start of this attempt; with base, gives the average for the ETA
}

func (s *slot) begin(name string, offset, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active, s.name, s.base, s.done, s.total = true, name, offset, offset, total
	s.note, s.speed, s.conns = "", 0, 0
	now := time.Now()
	s.lastBytes, s.lastTime, s.start = offset, now, now
}

// advance records n freshly written bytes.
func (s *slot) advance(n int64) {
	s.mu.Lock()
	s.done += n
	s.note = ""
	s.mu.Unlock()
}

// rewind takes back n bytes that were written but must be fetched again (a chunk
// attempt failed past its last checkpoint).
func (s *slot) rewind(n int64) {
	s.mu.Lock()
	s.done -= n
	if s.done < s.base {
		s.base = s.done
	}
	s.lastBytes -= n
	s.mu.Unlock()
}

// reset restarts the row's counters from offset (e.g. after a MAC mismatch).
func (s *slot) reset(offset int64) {
	s.mu.Lock()
	s.done, s.base, s.lastBytes = offset, offset, offset
	s.start, s.speed = time.Now(), 0
	s.mu.Unlock()
}

func (s *slot) addConn(d int) {
	s.mu.Lock()
	s.conns += d
	s.mu.Unlock()
}

func (s *slot) setNote(msg string) {
	s.mu.Lock()
	s.note = msg
	s.mu.Unlock()
}

func (s *slot) clear() {
	s.mu.Lock()
	s.active, s.name, s.note = false, "", ""
	s.done, s.total, s.base, s.speed, s.conns = 0, 0, 0, 0, 0
	s.mu.Unlock()
}

type slotSnap struct {
	active      bool
	name        string
	done, total int64
	speed, avg  float64
	note        string
	conns       int
}

// sample refreshes the smoothed speed and returns a consistent snapshot.
func (s *slot) sample(now time.Time) slotSnap {
	s.mu.Lock()
	defer s.mu.Unlock()
	var avg float64
	if s.active {
		if el := now.Sub(s.start).Seconds(); el >= minSpeedSpan.Seconds() && s.done > s.base {
			avg = float64(s.done-s.base) / el
		}
		if dt := now.Sub(s.lastTime).Seconds(); dt >= 0.05 {
			inst := float64(s.done-s.lastBytes) / dt
			if inst < 0 {
				inst = 0
			}
			if s.speed == 0 {
				s.speed = inst
			} else {
				s.speed = 0.7*s.speed + 0.3*inst
			}
			s.lastBytes, s.lastTime = s.done, now
		}
	}
	return slotSnap{s.active, s.name, s.done, s.total, s.speed, avg, s.note, s.conns}
}

// progress owns the dashboard for one link: a title, one row per concurrent file,
// and an aggregate line. It renders in place on a TTY, or prints plain periodic
// status lines when stdout is piped.
type progress struct {
	title       string
	totalFiles  int
	totalBytes  int64 // every selected file's size is known up front from the API
	longestName int   // display width of the longest selected file name
	slots       []*slot
	started     time.Time

	mu        sync.Mutex
	completed int
	verified  int
	skipped   int
	failed    int
	finishedB int64 // bytes belonging to finished/skipped/failed files

	sessionB  atomic.Int64 // bytes received this run
	firstByte atomic.Int64

	samples      []speedSample // guarded by con.mu, like all drawing
	speedWindow  time.Duration
	minSpeedSpan time.Duration

	lastLines int   // lines in the frame on screen
	lastWidth []int // display width of each of those lines, to tell how a resize re-wrapped them
	tty       bool

	stopOnce sync.Once
	quit     chan struct{}
	wg       sync.WaitGroup
}

func newProgress(title string, files []*node, rows int) *progress {
	longest := 8
	for _, f := range files {
		longest = max(longest, dispWidth(f.name))
	}
	if rows > len(files) {
		rows = len(files)
	}
	slots := make([]*slot, rows)
	for i := range slots {
		slots[i] = &slot{}
	}
	return &progress{
		title:        title,
		totalFiles:   len(files),
		totalBytes:   totalSize(files),
		longestName:  longest,
		slots:        slots,
		started:      time.Now(),
		tty:          con.tty,
		quit:         make(chan struct{}),
		speedWindow:  speedWindow,
		minSpeedSpan: minSpeedSpan,
	}
}

// start registers with the console and launches the refresh loop.
func (p *progress) start(ctx context.Context) {
	con.mu.Lock()
	// Seed the speed window, so the first plain-mode report (10s in) has a span to
	// average over rather than showing a placeholder.
	p.recordSample(time.Now())
	if p.tty {
		os.Stdout.WriteString("\x1b[?25l") // hide the cursor while we redraw
		con.erase, con.paint = p.eraseLocked, p.renderLocked
	} else {
		fmt.Printf("%s\n%d file(s), %s\n\n", p.title, p.totalFiles, formatBytes(p.totalBytes))
	}
	con.mu.Unlock()

	p.wg.Go(func() {
		interval := refreshInterval
		if !p.tty {
			interval = plainInterval
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		var resized <-chan struct{}
		if p.tty {
			ch, stop := watchResize()
			defer stop()
			resized = ch
		}
		for {
			select {
			case <-p.quit:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				p.render()
			case <-resized:
				p.render() // redraw at the new width straight away
			}
		}
	})
}

// stop halts the refresh loop, erases the dashboard and restores the cursor.
func (p *progress) stop() {
	p.stopOnce.Do(func() {
		close(p.quit)
		p.wg.Wait()
		con.mu.Lock()
		if p.tty {
			p.eraseLocked()
			os.Stdout.WriteString("\x1b[?25h")
		}
		con.erase, con.paint = nil, nil
		con.mu.Unlock()
	})
}

func (p *progress) addBytes(n int64) {
	if n > 0 && p.firstByte.Load() == 0 {
		p.firstByte.CompareAndSwap(0, time.Now().UnixNano())
	}
	p.sessionB.Add(n)
}

// avgSpeed is the mean rate since the first byte: the closing report, and the basis
// for the ETA (stable on a jittery connection).
func (p *progress) avgSpeed() float64 {
	first := p.firstByte.Load()
	if first == 0 {
		return 0
	}
	// At least a second, so a handful of bytes arriving in a millisecond doesn't
	// report a meaningless rate.
	elapsed := max(time.Since(time.Unix(0, first)).Seconds(), 1)
	return float64(p.sessionB.Load()) / elapsed
}

func (p *progress) etaSpeed() float64 {
	first := p.firstByte.Load()
	if first == 0 || time.Since(time.Unix(0, first)) < p.minSpeedSpan {
		return 0
	}
	return p.avgSpeed()
}

func (p *progress) recordSample(now time.Time) {
	p.samples = append(p.samples, speedSample{t: now, bytes: p.sessionB.Load()})
	cutoff := now.Add(-p.speedWindow)
	drop := 0
	for drop+1 < len(p.samples) && !p.samples[drop+1].t.After(cutoff) {
		drop++
	}
	p.samples = p.samples[drop:]
}

// windowSpeed is the live aggregate rate over the sliding window.
func (p *progress) windowSpeed() float64 {
	if len(p.samples) < 2 {
		return 0
	}
	first, last := p.samples[0], p.samples[len(p.samples)-1]
	span := last.t.Sub(first.t).Seconds()
	if span < p.minSpeedSpan.Seconds() {
		return 0
	}
	return float64(last.bytes-first.bytes) / span
}

func (p *progress) fileDone(size int64, verified bool) {
	p.mu.Lock()
	p.completed++
	if verified {
		p.verified++
	}
	p.finishedB += size
	p.mu.Unlock()
}

func (p *progress) fileSkipped(size int64) {
	p.mu.Lock()
	p.skipped++
	p.finishedB += size
	p.mu.Unlock()
}

func (p *progress) fileFailed(size int64) {
	p.mu.Lock()
	p.failed++
	p.finishedB += size // nothing more will arrive for it; keeps "left" honest
	p.mu.Unlock()
}

func (p *progress) render() {
	con.mu.Lock()
	defer con.mu.Unlock()
	p.renderLocked()
}

// eraseLocked removes the drawn dashboard, leaving the cursor at its top.
func (p *progress) eraseLocked() {
	if up := p.rowsOnScreen(terminalWidth()); up > 0 {
		os.Stdout.WriteString(fmt.Sprintf("\x1b[%dA\r\x1b[J", up))
	}
	p.lastLines, p.lastWidth = 0, nil
}

// rowsOnScreen is how many terminal rows the frame on screen occupies at the
// current width. Each line was drawn narrower than the terminal, but if the window
// has since shrunk, the terminal has re-wrapped the longer ones onto several rows.
func (p *progress) rowsOnScreen(cols int) int {
	return frameRows(p.lastWidth, cols)
}

func frameRows(widths []int, cols int) int {
	rows := 0
	for _, w := range widths {
		rows += max(1, (w+cols-1)/cols)
	}
	return rows
}

// Layout of a file row: " NAME BAR 100% ×4 TAIL". The name column gets whatever
// the rest leaves, so a wide window shows long names in full; when names don't
// fit, the bar gives up columns first, then names are shortened.
const (
	rowFixedW = 1 + 1 + 1 + 4 + 1 + 3 + 1 // margins, percentage, connections (bar excluded)
	rowTailW  = 24                        // "  38.2 MiB/s  ETA 0:25" with room to spare
	minNameW  = 8
)

// rowLayout returns the name column and bar widths for a terminal cols wide.
func rowLayout(cols, longest int) (nameW, barW int) {
	room := cols - 1 - rowFixedW - rowTailW // for name + bar
	barW = barWidth
	if short := longest - (room - barW); short > 0 {
		barW = max(minBarWidth, barW-short)
	}
	return max(minNameW, min(longest, room-barW)), barW
}

// renderLocked paints a frame (TTY) or prints one status line (plain). Callers
// hold con.mu.
func (p *progress) renderLocked() {
	now := time.Now()
	p.recordSample(now)
	agg := p.windowSpeed()
	pace := p.etaSpeed()

	var rows []slotSnap
	var activeDone int64
	var slowestFile float64
	for _, s := range p.slots {
		r := s.sample(now)
		rows = append(rows, r)
		if !r.active {
			continue
		}
		activeDone += r.done
		// A file in flight cannot finish sooner than its own connections allow.
		if r.avg > 0 && r.total > 0 {
			if eta := float64(r.total-r.done) / r.avg; eta > slowestFile {
				slowestFile = eta
			}
		}
	}

	p.mu.Lock()
	completed, skipped, failed, finishedB := p.completed, p.skipped, p.failed, p.finishedB
	p.mu.Unlock()

	// Sizes are all known up front, so "left" is exact: total minus finished files
	// minus the bytes on disk for files in flight.
	remaining := float64(p.totalBytes - finishedB - activeDone)
	if remaining < 0 {
		remaining = 0
	}
	eta := formatETA(remaining, pace)
	if bulk := etaSeconds(remaining, pace); slowestFile > bulk {
		eta = formatDuration(time.Duration(slowestFile * float64(time.Second)))
	}

	if !p.tty {
		fmt.Printf("[%s] %d/%d done%s  %s left  %s  ETA %s\n",
			now.Format("15:04:05"), completed+skipped, p.totalFiles, failedSuffix(failed),
			formatBytes(int64(remaining)), formatSpeed(agg), eta)
		return
	}

	width := terminalWidth()
	nameW, barW := rowLayout(width, p.longestName)
	rule := strings.Repeat("─", width-1)
	lines := []string{p.title, rule}
	for _, r := range rows {
		if !r.active {
			lines = append(lines, "")
			continue
		}
		var tail string
		switch {
		case r.note != "":
			tail = r.note
		default:
			tail = fmt.Sprintf("%10s  ETA %s", formatSpeed(r.speed), formatETA(float64(r.total-r.done), r.avg))
		}
		conns := "   "
		if r.conns > 1 {
			conns = fmt.Sprintf("×%-2d", r.conns)
		}
		lines = append(lines, fmt.Sprintf(" %s %s %3d%% %s %s",
			padRight(r.name, nameW), bar(r.done, r.total, barW), pct(r.done, r.total), conns, tail))
	}
	lines = append(lines, rule)
	lines = append(lines, fmt.Sprintf(" %d/%d done%s  %s left  %s  ETA %s",
		completed+skipped, p.totalFiles, failedSuffix(failed),
		formatBytes(int64(remaining)), formatSpeed(agg), eta))

	// Go back to the top of the previous frame (counting rows a shrink re-wrapped),
	// overwrite it line by line, and clear whatever is left below. Every line stays
	// narrower than the terminal so none of them wraps.
	var b strings.Builder
	if up := p.rowsOnScreen(width); up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA\r", up)
	}
	p.lastWidth = p.lastWidth[:0]
	for _, ln := range lines {
		ln = truncate(ln, width-1)
		p.lastWidth = append(p.lastWidth, dispWidth(ln))
		b.WriteString("\x1b[2K")
		b.WriteString(ln)
		b.WriteString("\n")
	}
	b.WriteString("\x1b[J")
	os.Stdout.WriteString(b.String())
	p.lastLines = len(lines)
}

// runStats is a link's outcome, accumulated into the multi-link total.
type runStats struct {
	completed, verified, skipped, failed int
	bytes                                int64
	elapsed                              time.Duration
}

// summary prints the closing report and returns the figures.
func (p *progress) summary() runStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := runStats{p.completed, p.verified, p.skipped, p.failed, p.sessionB.Load(), time.Since(p.started)}
	fmt.Printf("\n%s\n", p.title)
	fmt.Printf("  %d downloaded", p.completed)
	if p.verified > 0 {
		fmt.Printf(" (%d verified)", p.verified)
	}
	fmt.Printf(", %d already present", p.skipped)
	if p.failed > 0 {
		fmt.Printf(", %s", colour("31", fmt.Sprintf("%d failed", p.failed)))
	}
	fmt.Printf("\n  %s transferred in %s (%s average)\n",
		formatBytes(st.bytes), formatDuration(st.elapsed), formatSpeed(p.avgSpeed()))
	return st
}

func failedSuffix(failed int) string {
	if failed == 0 {
		return ""
	}
	return fmt.Sprintf("  %d failed", failed)
}
