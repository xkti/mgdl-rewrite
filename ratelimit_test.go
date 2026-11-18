package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// --limit-rate holds the real transfer rate over TLS, where every read returns at
// most one record (≤ 16 KiB), far less than the read buffer. Charging the whole
// buffer per read ran about 16× too slow.
func TestLimitRateHoldsWithSmallReads(t *testing.T) {
	m := newFakeMega(t)
	m.storage.Close()
	m.storage = httptest.NewTLSServer(http.HandlerFunc(m.serveStorage))
	const rate = 1 << 20
	f := m.addFile("FiLe0001", "x.bin", 2*rate, 60, false)
	out := t.TempDir()
	cfg := m.config(out)
	cfg.direct = true
	cfg.limitRate = rate
	cfg.links = []string{m.fileLink(f)}

	start := time.Now()
	if _, _, err := runLinks(context.Background(), cfg, m.apiClient(), m.storage.Client()); err != nil {
		t.Fatal(err)
	}
	// One second's worth is the bucket's burst, so 2 s of data takes about 1 s.
	if took := time.Since(start); took < 700*time.Millisecond || took > 2*time.Second {
		t.Fatalf("2 MiB at 1 MiB/s took %s, want about 1s", took)
	}
	mustEqualFile(t, filepath.Join(out, "x.bin"), f.plain)
}

// Waiting on the rate limiter is not a stall, however long each wait is.
func TestLimitRateIsNotAStall(t *testing.T) {
	old := stallTimeout
	stallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { stallTimeout = old })

	m := newFakeMega(t)
	const rate = 128 << 10 // each 32 KiB piece waits 250 ms, past the stall timeout
	f := m.addFile("FiLe0001", "x.bin", 2*rate, 61, false)
	m.hooks(nil, time.Millisecond)
	out := t.TempDir()
	cfg := m.config(out)
	cfg.direct = true
	cfg.connections = 1
	cfg.limitRate = rate

	if _, err := m.run(context.Background(), cfg, m.fileLink(f)); err != nil {
		t.Fatal(err)
	}
	if got := m.requests.Load(); got != 1 {
		t.Fatalf("%d storage requests: a rate-limit wait was taken for a stall", got)
	}
	mustEqualFile(t, filepath.Join(out, "x.bin"), f.plain)
}
