package main

import (
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestDispWidth(t *testing.T) {
	for s, want := range map[string]int{
		"":              0,
		"backup.7z":     9,
		"café":          4,
		"café":         4, // e + combining acute
		"日本語.txt":       10,
		"한국어":           6,
		"ｆｕｌｌ":          8, // fullwidth Latin
		"🎉 party":       8,
		"❤️":            1, // variation selector adds nothing
		"a‍b":           2, // zero-width joiner
		"├── NUL/ 17 B": 13,
	} {
		if got := dispWidth(s); got != want {
			t.Errorf("dispWidth(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestTruncateByColumns(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"a long file name.mkv", 10, "a long fi…"},
		{"日本語のファイル名.txt", 9, "日本語の…"},  // 4 wide chars (8) + … — never 10
		{"日本語のファイル名.txt", 10, "日本語の…"}, // a wide char that would overshoot is left out
		{"anything", 1, "…"},
		{"anything", 0, ""},
	} {
		got := truncate(c.in, c.n)
		if got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
		if dispWidth(got) > c.n {
			t.Errorf("truncate(%q, %d) is %d columns wide", c.in, c.n, dispWidth(got))
		}
	}
	if got := padRight("日本", 6); got != "日本  " {
		t.Errorf("padRight = %q", got)
	}
	if got := padRight("abcdefgh", 5); got != "abcd…" {
		t.Errorf("padRight = %q", got)
	}
}

func TestRowLayout(t *testing.T) {
	// At 80 columns, name + bar get 80 - 1 - 23 - 24 = 32.
	for _, c := range []struct{ cols, longest, name, bar int }{
		{80, 9, 9, 20},                  // short name: full bar
		{80, 12, 12, 20},                // exactly fits
		{80, 19, 19, 13},                // bar gives up 7 columns, name shown in full
		{80, 30, 24, minBarWidth},       // bar at its minimum, name shortened
		{250, 120, 120, 20},             // wide terminal: whole name and full bar
		{64, 30, minNameW, minBarWidth}, // both at their minimum
		{63, 30, 16, 0},                 // too thin for both: bar hidden, name gets its room
		{63, 9, 9, 0},                   // ...even when the name is short
		{30, 120, minNameW, 0},          // very narrow: no bar, name at its minimum
	} {
		name, bar := rowLayout(c.cols, c.longest)
		if name != c.name || bar != c.bar {
			t.Errorf("rowLayout(%d, %d) = %d, %d; want %d, %d", c.cols, c.longest, name, bar, c.name, c.bar)
		}
	}
}

// After a shrink, lines wider than the new width occupy several rows.
func TestFrameRows(t *testing.T) {
	widths := []int{9, 79, 79, 0, 79, 45}
	if got := frameRows(widths, 80); got != 6 {
		t.Errorf("same width: %d rows", got)
	}
	if got := frameRows(widths, 40); got != 1+2+2+1+2+2 {
		t.Errorf("shrunk to 40: %d rows", got)
	}
	if got := frameRows(nil, 80); got != 0 {
		t.Errorf("empty frame: %d", got)
	}
}

// A rendered frame fits the terminal exactly, whatever the names.
func TestRenderFitsWidth(t *testing.T) {
	for _, cols := range []int{40, 80, 200} {
		name := strings.Repeat("長い名前", 20) + ".mkv"
		nw, bw := rowLayout(cols, dispWidth(name))
		barCol := ""
		if bw > 0 {
			barCol = " " + bar(5, 10, bw)
		}
		row := " " + padRight(name, nw) + "  100.0 MiB" + barCol + "  50% ×4  38.2 MiB/s  ETA 0:25"
		if w := dispWidth(truncate(row, cols-1)); w > cols-1 {
			t.Errorf("%d cols: row is %d wide", cols, w)
		}
	}
}

// renderFrame draws one dashboard frame at the given width and returns its lines.
func renderFrame(t *testing.T, cols int, names ...string) []string {
	t.Helper()
	t.Setenv("COLUMNS", strconv.Itoa(cols)) // stdout is a pipe here, so this sets the width
	var files []*node
	for _, n := range names {
		files = append(files, &node{name: n, size: 100 << 20})
	}
	p := newProgress("Share", files, len(files))
	p.tty = true
	for i, f := range files {
		p.slots[i].begin(f.name, 0, f.size)
		p.slots[i].advance(30 << 20)
	}
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	con.mu.Lock()
	p.renderLocked()
	con.mu.Unlock()
	os.Stdout = old
	w.Close()
	raw, _ := io.ReadAll(r)
	var lines []string
	for ln := range strings.SplitSeq(string(raw), "\n") {
		ln = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`).ReplaceAllString(ln, "")
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, strings.TrimRight(ln, "\r"))
		}
	}
	return lines
}

func TestDashboardUsesWidth(t *testing.T) {
	long := strings.Repeat("Very Long Episode Title ", 6) + "S01E01 1080p.mkv" // 160 columns
	cjk := strings.Repeat("日本語のファイル名", 4) + ".mp4"                             // 76 columns

	wide := strings.Join(renderFrame(t, 240, long, cjk), "\n")
	if !strings.Contains(wide, long) || !strings.Contains(wide, cjk) {
		t.Errorf("240 columns: names were cut:\n%s", wide)
	}
	if !strings.Contains(wide, " 100.0 MiB ") {
		t.Errorf("240 columns: expected each file's size:\n%s", wide)
	}
	for _, cols := range []int{240, 80, 50} {
		for _, ln := range renderFrame(t, cols, long, cjk) {
			if w := dispWidth(ln); w > cols-1 {
				t.Errorf("%d columns: line is %d wide: %q", cols, w, ln)
			}
		}
	}
	thin := strings.Join(renderFrame(t, 60, long, cjk), "\n")
	if strings.ContainsAny(thin, "█░") || !strings.Contains(thin, "ETA") {
		t.Errorf("60 columns: expected the bar hidden and the ETA kept:\n%s", thin)
	}
	narrow := strings.Join(renderFrame(t, 80, long, cjk), "\n")
	if !strings.Contains(narrow, "…") || !strings.Contains(narrow, "ETA") {
		t.Errorf("80 columns: expected shortened names with the ETA kept:\n%s", narrow)
	}
}
