package main

import (
	"strings"
	"unicode"
)

// Terminal display width. CJK ideographs, Hangul, fullwidth forms and most emoji
// take two columns; combining marks, variation selectors and zero-width
// characters take none. Measuring in runes instead would let such names overflow
// a line, which wraps it and throws off the dashboard's in-place redraw.

// wideRanges are the double-width blocks (East Asian Wide/Fullwidth, and the
// emoji blocks terminals render two columns wide).
var wideRanges = [][2]rune{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x2329, 0x232A}, {0x23E9, 0x23EC},
	{0x23F0, 0x23F0}, {0x23F3, 0x23F3}, {0x25FD, 0x25FE}, {0x2614, 0x2615},
	{0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1},
	{0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE},
	{0x26D4, 0x26D4}, {0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5},
	{0x26FA, 0x26FA}, {0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B},
	{0x2728, 0x2728}, {0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755},
	{0x2757, 0x2757}, {0x2795, 0x2797}, {0x27B0, 0x27B0}, {0x27BF, 0x27BF},
	{0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55},
	{0x2E80, 0x303E}, {0x3041, 0x33FF}, {0x3400, 0x4DBF}, {0x4E00, 0x9FFF},
	{0xA000, 0xA4CF}, {0xA960, 0xA97F}, {0xAC00, 0xD7A3}, {0xF900, 0xFAFF},
	{0xFE10, 0xFE19}, {0xFE30, 0xFE6F}, {0xFF00, 0xFF60}, {0xFFE0, 0xFFE6},
	{0x16FE0, 0x16FE4}, {0x17000, 0x18CFF}, {0x1B000, 0x1B2FF},
	{0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A},
	{0x1F200, 0x1F2FF}, {0x1F300, 0x1F64F}, {0x1F680, 0x1F6FF}, {0x1F7E0, 0x1F7EB},
	{0x1F90C, 0x1F9FF}, {0x1FA70, 0x1FAFF}, {0x20000, 0x2FFFD}, {0x30000, 0x3FFFD},
}

// runeWidth is how many terminal columns r occupies.
func runeWidth(r rune) int {
	switch {
	case r == 0x200B || r == 0x200C || r == 0x200D || r == 0x2060 || r == 0xFEFF:
		return 0 // zero-width space/joiners, word joiner, BOM
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF:
		return 0 // variation selectors
	case r < 0x300:
		return 1 // fast path: ASCII and Latin-1/Extended
	case unicode.In(r, unicode.Mn, unicode.Me):
		return 0 // combining marks
	}
	for _, w := range wideRanges {
		if r < w[0] {
			break
		}
		if r <= w[1] {
			return 2
		}
	}
	return 1
}

// dispWidth is how many terminal columns s occupies (no escape sequences).
func dispWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

// truncate shortens s to at most n columns, ending in "…" when cut.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if dispWidth(s) <= n {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > n-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

// padRight truncates or pads s with spaces to exactly n columns.
func padRight(s string, n int) string {
	s = truncate(s, n)
	return s + strings.Repeat(" ", max(0, n-dispWidth(s)))
}
