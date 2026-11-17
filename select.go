package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// errSelectCancelled means the user quit the selector without choosing.
var errSelectCancelled = errors.New("selection cancelled")

// selector is the --select tree: a list of the visible rows (expanded folders show
// their children), a cursor, and the set of chosen files.
type selector struct {
	t        *tree
	expanded map[*node]bool
	chosen   map[*node]bool
	rows     []*node
	depth    map[*node]int
	cursor   int
	top      int // first row on screen
}

// runSelector shows the tree and returns the chosen files in tree order.
// preselected (from --path, or everything) starts ticked.
func runSelector(t *tree, preselected []*node) ([]*node, error) {
	s := &selector{t: t, expanded: map[*node]bool{t.root: true}, chosen: map[*node]bool{}, depth: map[*node]int{}}
	for _, f := range preselected {
		s.chosen[f] = true
	}
	// Open top-level folders when the tree is small enough to take in at once.
	if len(t.files) <= 40 {
		var openAll func(n *node)
		openAll = func(n *node) {
			for _, c := range n.children {
				if c.isDir {
					s.expanded[c] = true
					openAll(c)
				}
			}
		}
		openAll(t.root)
	}
	s.rebuild()

	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("--select: cannot switch the terminal to raw mode: %w", err)
	}
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor
	defer func() {
		os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")
		term.Restore(fd, old)
	}()

	// Keys are read in the background so a resize can redraw without waiting for
	// one. The reader is left blocked in Read when we return; the terminal is
	// restored either way, and the process carries on without it.
	type read struct {
		b   []byte
		err error
	}
	keys := make(chan read)
	go func() {
		for {
			buf := make([]byte, 64)
			n, err := os.Stdin.Read(buf)
			keys <- read{buf[:n], err}
			if err != nil {
				return
			}
		}
	}()
	resized, stopResize := watchResize()
	defer stopResize()

	for {
		s.draw()
		select {
		case <-resized:
			continue
		case r := <-keys:
			if r.err != nil {
				return nil, r.err
			}
			for _, key := range splitKeys(r.b) {
				out, done, err := s.handleKey(key)
				if done {
					return out, err
				}
			}
		}
	}
}

// splitKeys breaks one read into individual keys: key-repeat and fast typing can
// deliver several escape sequences in a single read.
func splitKeys(b []byte) []string {
	var keys []string
	for len(b) > 0 {
		n := 1
		if b[0] == 0x1b && len(b) >= 3 && (b[1] == '[' || b[1] == 'O') {
			// CSI/SS3: parameters, then a final byte in 0x40–0x7e.
			n = 2
			for n < len(b) {
				c := b[n]
				n++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
		} else if b[0] >= 0x80 {
			_, n = utf8.DecodeRune(b)
		}
		keys = append(keys, string(b[:n]))
		b = b[n:]
	}
	return keys
}

// handleKey applies one keypress; done reports that the selector should close.
func (s *selector) handleKey(key string) (out []*node, done bool, err error) {
	{
		switch key {
		case "\x1b[A", "k", "\x1bOA":
			s.move(-1)
		case "\x1b[B", "j", "\x1bOB":
			s.move(1)
		case "\x1b[5~":
			s.move(-s.pageSize())
		case "\x1b[6~":
			s.move(s.pageSize())
		case "\x1b[H", "\x1b[1~", "g", "\x1bOH":
			s.cursor = 0
		case "\x1b[F", "\x1b[4~", "G", "\x1bOF":
			s.cursor = len(s.rows) - 1
		case "\x1b[C", "l", "\x1bOC":
			if n := s.current(); n != nil && n.isDir {
				s.expanded[n] = true
				s.rebuild()
			}
		case "\x1b[D", "h", "\x1bOD":
			s.collapseOrParent()
		case " ":
			if n := s.current(); n != nil {
				s.toggle(n)
			}
			s.move(1)
		case "a":
			s.toggle(s.t.root)
		case "\r", "\n":
			for _, f := range s.t.files {
				if s.chosen[f] {
					out = append(out, f)
				}
			}
			return out, true, nil
		case "q", "\x1b", "\x03":
			return nil, true, errSelectCancelled
		}
	}
	return nil, false, nil
}

// rebuild recomputes the visible rows after an expand or collapse.
func (s *selector) rebuild() {
	cur := s.current()
	s.rows = s.rows[:0]
	var walk func(n *node, d int)
	walk = func(n *node, d int) {
		for _, c := range n.children {
			s.rows = append(s.rows, c)
			s.depth[c] = d
			if c.isDir && s.expanded[c] {
				walk(c, d+1)
			}
		}
	}
	walk(s.t.root, 0)
	for i, r := range s.rows {
		if r == cur {
			s.cursor = i
		}
	}
	s.cursor = max(0, min(s.cursor, len(s.rows)-1))
}

func (s *selector) current() *node {
	if s.cursor < 0 || s.cursor >= len(s.rows) {
		return nil
	}
	return s.rows[s.cursor]
}

func (s *selector) move(d int) {
	s.cursor = max(0, min(s.cursor+d, len(s.rows)-1))
}

func (s *selector) collapseOrParent() {
	n := s.current()
	if n == nil {
		return
	}
	if n.isDir && s.expanded[n] {
		s.expanded[n] = false
		s.rebuild()
		return
	}
	if n.parent != nil && n.parent != s.t.root {
		s.expanded[n.parent] = false
		for i, r := range s.rows {
			if r == n.parent {
				s.cursor = i
			}
		}
		s.rebuild()
	}
}

// filesUnder lists every file beneath (or equal to) n.
func filesUnder(n *node) []*node {
	if !n.isDir {
		return []*node{n}
	}
	var out []*node
	for _, c := range n.children {
		out = append(out, filesUnder(c)...)
	}
	return out
}

// toggle flips a file, or a whole folder: if everything under it is chosen it is
// cleared, otherwise everything is chosen.
func (s *selector) toggle(n *node) {
	files := filesUnder(n)
	all := true
	for _, f := range files {
		if !s.chosen[f] {
			all = false
			break
		}
	}
	for _, f := range files {
		if all {
			delete(s.chosen, f)
		} else {
			s.chosen[f] = true
		}
	}
}

// mark is a row's checkbox: [x] all, [-] some, [ ] none.
func (s *selector) mark(n *node) string {
	files := filesUnder(n)
	got := 0
	for _, f := range files {
		if s.chosen[f] {
			got++
		}
	}
	switch {
	case got == 0:
		return "[ ]"
	case got == len(files):
		return "[x]"
	default:
		return "[-]"
	}
}

func (s *selector) pageSize() int {
	_, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || h < 8 {
		h = 24
	}
	return h - 5
}

func (s *selector) draw() {
	w := terminalWidth()
	page := s.pageSize()
	if s.cursor < s.top {
		s.top = s.cursor
	}
	if s.cursor >= s.top+page {
		s.top = s.cursor - page + 1
	}

	var count int
	var size int64
	for f := range s.chosen {
		count++
		size += f.size
	}

	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	fmt.Fprintf(&b, "\x1b[1m%s\x1b[0m\r\n", truncate(s.t.title(), w-1))
	fmt.Fprintf(&b, "%s\r\n", truncate(fmt.Sprintf("%d of %d files selected, %s of %s",
		count, len(s.t.files), formatBytes(size), formatBytes(totalSize(s.t.files))), w-1))
	b.WriteString(strings.Repeat("─", w-1) + "\r\n")

	for i := s.top; i < len(s.rows) && i < s.top+page; i++ {
		n := s.rows[i]
		icon := "  "
		if n.isDir {
			icon = "▸ "
			if s.expanded[n] {
				icon = "▾ "
			}
		}
		right := ""
		if n.isDir {
			right = formatBytes(totalSize(filesUnder(n)))
		} else {
			right = formatBytes(n.size)
		}
		left := fmt.Sprintf("%s %s%s%s", s.mark(n), strings.Repeat("  ", s.depth[n]), icon, n.origName)
		if n.isDir {
			left += "/"
		}
		room := w - 1 - dispWidth(right) - 2
		line := padRight(left, room) + "  " + right
		if i == s.cursor {
			line = "\x1b[7m" + line + "\x1b[0m"
		}
		b.WriteString(line + "\r\n")
	}
	for i := len(s.rows) - s.top; i < page; i++ {
		b.WriteString("\r\n")
	}
	b.WriteString("\x1b[2m" + truncate("↑↓ move  space toggle  →← open/close  a all  enter download  q quit", w-1) + "\x1b[0m")
	os.Stdout.WriteString(b.String())
}
