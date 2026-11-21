package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rateLimiter is a token bucket shared by every worker, so --limit-rate caps the
// combined transfer rather than each download individually. A nil limiter is
// unlimited, which keeps the hot path free of branches at the call site.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64 // maximum tokens held, so short pauses can be spent later
	tokens float64
	last   time.Time
}

// newRateLimiter returns nil (meaning unlimited) when rate is zero or negative.
func newRateLimiter(bytesPerSec int64) *rateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	burst := float64(bytesPerSec) // one second of slack
	return &rateLimiter{
		rate:   float64(bytesPerSec),
		burst:  burst,
		tokens: burst,
		last:   time.Now(),
	}
}

// wait blocks until n bytes' worth of budget is available, or the context ends.
func (r *rateLimiter) wait(ctx context.Context, n int) error {
	if r == nil {
		return nil
	}
	for {
		r.mu.Lock()
		now := time.Now()
		// Refill for the time elapsed since the last request.
		r.tokens += now.Sub(r.last).Seconds() * r.rate
		r.last = now
		if r.tokens > r.burst {
			r.tokens = r.burst
		}
		need := float64(n)
		if need > r.burst {
			need = r.burst // never ask for more than the bucket can ever hold
		}
		if r.tokens >= need {
			r.tokens -= need
			r.mu.Unlock()
			return nil
		}
		// Sleep exactly long enough for the shortfall to accrue.
		deficit := need - r.tokens
		delay := time.Duration(deficit / r.rate * float64(time.Second))
		r.mu.Unlock()

		if delay < time.Millisecond {
			delay = time.Millisecond
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// bar renders a proportional progress bar width columns wide; an unknown total
// shows as empty.
func bar(done, total int64, width int) string {
	filled := 0
	if total > 0 {
		filled = min(width, max(0, int(float64(done)/float64(total)*float64(width))))
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// pct returns a 0-100 completion percentage, guarding against an unknown total.
func pct(done, total int64) int {
	if total <= 0 {
		return 0
	}
	return min(int(float64(done)/float64(total)*100), 100)
}

// formatBytes prints a binary (IEC) size: KiB/MiB/GiB are powers of 1024, matching
// what ls -h, du -h and the file manager report for the same file.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// formatSizeArg prints a size as it would be given on the command line ("149M",
// "1G"), or as formatBytes does when it isn't a whole number of units.
func formatSizeArg(n int64) string {
	for _, u := range []struct {
		shift  uint
		suffix string
	}{{40, "T"}, {30, "G"}, {20, "M"}, {10, "K"}} {
		if n >= 1<<u.shift && n%(1<<u.shift) == 0 {
			return strconv.FormatInt(n>>u.shift, 10) + u.suffix
		}
	}
	return formatBytes(n)
}

// formatSpeed prints a transfer rate, or a placeholder before any bytes have moved.
func formatSpeed(bps float64) string {
	if bps <= 0 {
		return "-- MiB/s"
	}
	return formatBytes(int64(bps)) + "/s"
}

// etaSeconds is how long the remaining bytes take at the given rate, or 0 when there is
// nothing to go on yet.
func etaSeconds(remaining, bps float64) float64 {
	if bps <= 0 || remaining <= 0 {
		return 0
	}
	return remaining / bps
}

// formatETA converts remaining bytes and current speed into a countdown.
func formatETA(remaining, bps float64) string {
	secs := etaSeconds(remaining, bps)
	if secs <= 0 {
		return "--:--"
	}
	return formatDuration(time.Duration(secs * float64(time.Second)))
}

// formatDuration prints m:ss, or h:mm:ss once the span passes an hour.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	h, m, s := total/3600, total/60%60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// parseSize reads a byte count like "256M", "1.5G", "512K" or a plain number.
// Suffixes are binary: 1K = 1024 bytes. An optional trailing "B" or "iB" is allowed.
func parseSize(s string) (int64, error) {
	orig := s
	s = strings.TrimSpace(s)
	u := strings.ToUpper(s)
	u = strings.TrimSuffix(strings.TrimSuffix(u, "B"), "I")
	mult := float64(1)
	if u != "" {
		switch u[len(u)-1] {
		case 'K':
			mult, u = 1<<10, u[:len(u)-1]
		case 'M':
			mult, u = 1<<20, u[:len(u)-1]
		case 'G':
			mult, u = 1<<30, u[:len(u)-1]
		case 'T':
			mult, u = 1<<40, u[:len(u)-1]
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(u), 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("cannot parse size %q (try 256M, 1G, 512K, or a byte count)", orig)
	}
	return int64(v * mult), nil
}

// plural formats n with a noun, adding "s" unless n is 1: "1 file", "2 files".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
