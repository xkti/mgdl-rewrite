package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// errSelectCancelled means the user quit the picker without choosing.
var errSelectCancelled = errors.New("selection cancelled")

// selector is the --select picker: one tree for every link in the run, laid out as
// it will land in the output directory. Each link's top folder (or file) is a
// section at the top level; links with the same folder name share one section, as
// their files share one folder on disk. Links are added as they load, in
// command-line order, so names already on screen never change.
//
// The view is rebuilt whenever a link arrives. Folders in it are made up for the
// view; files are the links' own nodes, so the choice (keyed by file) survives a
// rebuild, and so does which folders are open (keyed by path).
type selector struct {
	trees  []*tree
	files  []*node          // every loaded file, in link order
	broken []string         // one line per link that couldn't be opened
	root   *node            // the output directory
	parent map[*node]*node  // each row's folder in the view
	path   map[*node]string // each row's lower-cased path, the key for expanded
	info   map[*node]bool   // rows that are messages, not files or folders
	chosen map[*node]bool   // chosen files
	open   map[string]bool  // expanded folders, by path
	rows   []*node          // visible rows
	depth  map[*node]int
	cursor int
	top    int // first row on screen

	total   int  // links in the run
	loaded  int  // links that have arrived (opened or not)
	dups    bool // some names clash between links
	confirm bool // Enter pressed while links were still loading
}

func newSelector(total int) *selector {
	s := &selector{chosen: map[*node]bool{}, open: map[string]bool{}, total: total}
	s.refresh()
	return s
}

// addTree adds a loaded link with its preselected files (from --path, or all).
// Its top folder starts open, and so does everything in it if it's small enough
// to take in at once.
func (s *selector) addTree(t *tree, preselected []*node) {
	s.loaded++
	s.trees = append(s.trees, t)
	s.files = append(s.files, t.files...)
	for _, f := range preselected {
		s.chosen[f] = true
	}
	if t.root.isDir {
		s.open[strings.ToLower(t.root.name)] = true
		if len(t.files) <= 40 {
			var openAll func(n *node, p string)
			openAll = func(n *node, p string) {
				for _, c := range n.children {
					if c.isDir {
						cp := joinRel(p, strings.ToLower(c.name))
						s.open[cp] = true
						openAll(c, cp)
					}
				}
			}
			openAll(t.root, strings.ToLower(t.root.name))
		}
	}
	s.refresh()
}

// addBroken records a link that couldn't be opened; it shows as a message row.
func (s *selector) addBroken(lr *linkRun) {
	s.loaded++
	s.broken = append(s.broken, fmt.Sprintf("link %d couldn't be opened: %s", lr.idx, lr.note))
	s.refresh()
}

// addSkipped counts a link that arrived but adds nothing (a repeat).
func (s *selector) addSkipped() {
	s.loaded++
	s.refresh()
}

func (s *selector) loading() bool { return s.loaded < s.total }

// refresh rebuilds the view from the loaded links and keeps the cursor on the
// same row.
func (s *selector) refresh() {
	curPath := ""
	if n := s.current(); n != nil {
		curPath = s.path[n]
	}
	s.root = &node{isDir: true}
	s.parent, s.path, s.info, s.depth = map[*node]*node{}, map[*node]string{}, map[*node]bool{}, map[*node]int{}
	s.dups = false
	dirs := map[string]*node{}
	var add func(v *node, vp string, items []*node)
	add = func(v *node, vp string, items []*node) {
		for _, c := range items {
			p := joinRel(vp, strings.ToLower(c.name))
			s.dups = s.dups || c.dup
			if !c.isDir {
				v.children = append(v.children, c)
				s.parent[c], s.path[c] = v, p
				continue
			}
			d := dirs[p]
			if d == nil {
				d = &node{isDir: true, handle: c.handle, origName: c.origName, name: c.name, dup: c.dup}
				dirs[p] = d
				v.children = append(v.children, d)
				s.parent[d], s.path[d] = v, p
			}
			add(d, p, c.children)
		}
	}
	for _, t := range s.trees {
		add(s.root, "", []*node{t.root})
	}
	sortView(s.root)
	for i, msg := range s.broken {
		n := &node{origName: msg, name: msg}
		s.root.children = append(s.root.children, n)
		s.parent[n], s.path[n], s.info[n] = s.root, fmt.Sprintf("\x00%d", i), true
	}
	s.rebuild(curPath)
}

// sortView orders every folder: folders first, then by original name, ignoring
// case. The sort is stable, so items with the same name — duplicates from
// different links — sit together in link order.
func sortView(d *node) {
	sort.SliceStable(d.children, func(i, j int) bool {
		a, b := d.children[i], d.children[j]
		if a.isDir != b.isDir {
			return a.isDir
		}
		return strings.ToLower(a.origName) < strings.ToLower(b.origName)
	})
	for _, c := range d.children {
		if c.isDir {
			sortView(c)
		}
	}
}

// rebuild recomputes the visible rows after a change and puts the cursor back on
// the row at curPath, if it's still visible.
func (s *selector) rebuild(curPath string) {
	s.rows = s.rows[:0]
	var walk func(n *node, d int)
	walk = func(n *node, d int) {
		for _, c := range n.children {
			s.rows = append(s.rows, c)
			s.depth[c] = d
			if c.isDir && s.open[s.path[c]] {
				walk(c, d+1)
			}
		}
	}
	walk(s.root, 0)
	for i, r := range s.rows {
		if curPath != "" && s.path[r] == curPath {
			s.cursor = i
		}
	}
	s.cursor = max(0, min(s.cursor, len(s.rows)-1))
}

// chosenFiles lists a tree's chosen files in tree order.
func (s *selector) chosenFiles(t *tree) []*node {
	var out []*node
	for _, f := range t.files {
		if s.chosen[f] {
			out = append(out, f)
		}
	}
	return out
}

// pickerEvent is what the picker reports when it closes.
type pickerEvent int

const (
	pickerDone      pickerEvent = iota // Enter: download what's chosen
	pickerCancelled                    // q, Esc or Ctrl+C
)

// runPicker opens the picker straight away and calls arrive for each link as it
// comes in, on this goroutine, so arrive may update the selector and trees freely.
// It returns when the user confirms or cancels, or ctx ends; arrivals may still be
// open then.
func runPicker(ctx context.Context, s *selector, arrivals <-chan *linkRun, arrive func(*linkRun)) (pickerEvent, error) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return 0, fmt.Errorf("--select: cannot switch the terminal to raw mode: %w", err)
	}
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor
	defer func() {
		os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")
		term.Restore(fd, old)
	}()

	// Keys are read in the background so a resize or an arriving link can redraw
	// without waiting for one. The reader is left blocked in Read when we return;
	// the terminal is restored either way, and the process carries on without it.
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
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-resized:
		case lr, ok := <-arrivals:
			if !ok {
				arrivals = nil
				continue
			}
			arrive(lr)
			if !s.loading() {
				s.confirm = false
			}
		case r := <-keys:
			if r.err != nil {
				return 0, r.err
			}
			for _, key := range splitKeys(r.b) {
				if ev, done := s.handleKey(key); done {
					return ev, nil
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

// handleKey applies one keypress; done reports that the picker should close.
func (s *selector) handleKey(key string) (ev pickerEvent, done bool) {
	enter := key == "\r" || key == "\n"
	if s.confirm && !enter {
		s.confirm = false // any other key: keep waiting for the rest to load
		return 0, false
	}
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
		s.cursor = max(0, len(s.rows)-1)
	case "\x1b[C", "l", "\x1bOC":
		if n := s.current(); n != nil && n.isDir {
			s.open[s.path[n]] = true
			s.rebuild(s.path[n])
		}
	case "\x1b[D", "h", "\x1bOD":
		s.collapseOrParent()
	case " ":
		if n := s.current(); n != nil {
			s.toggle(n)
		}
		s.move(1)
	case "a":
		s.toggle(s.root)
	case "\r", "\n":
		if s.loading() && !s.confirm {
			s.confirm = true
			return 0, false
		}
		return pickerDone, true
	case "q", "\x1b", "\x03":
		return pickerCancelled, true
	}
	return 0, false
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
	if n.isDir && s.open[s.path[n]] {
		s.open[s.path[n]] = false
		s.rebuild(s.path[n])
		return
	}
	if p := s.parent[n]; p != nil && p != s.root {
		s.open[s.path[p]] = false
		s.rebuild(s.path[p])
	}
}

// filesOf lists every file beneath (or equal to) a row of the view.
func (s *selector) filesOf(n *node) []*node {
	if s.info[n] {
		return nil
	}
	if !n.isDir {
		return []*node{n}
	}
	var out []*node
	for _, c := range n.children {
		out = append(out, s.filesOf(c)...)
	}
	return out
}

// toggle flips a file, or a whole folder: if everything under it is chosen it is
// cleared, otherwise everything is chosen.
func (s *selector) toggle(n *node) {
	files := s.filesOf(n)
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
	if s.info[n] {
		return "   "
	}
	files := s.filesOf(n)
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

// title heads the picker: the link's name for a single link, otherwise how many
// links there are, with loading progress until they're all in.
func (s *selector) title() string {
	var parts []string
	switch {
	case s.loading():
		parts = append(parts, fmt.Sprintf("Loading links: %d of %d", s.loaded, s.total))
	case s.total == 1 && len(s.trees) == 1:
		return s.trees[0].title()
	default:
		parts = append(parts, fmt.Sprintf("%d links", s.total))
	}
	if len(s.broken) > 0 {
		parts = append(parts, fmt.Sprintf("%d couldn't be opened", len(s.broken)))
	}
	return strings.Join(parts, " · ")
}

// label is what a row shows: the name on MEGA, or for a duplicate, the name it
// will be saved under, so the copies can be told apart.
func label(n *node) string {
	if n.dup {
		return n.name
	}
	return n.origName
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
	s.top = max(0, s.top)

	var count int
	var size int64
	for f := range s.chosen {
		count++
		size += f.size
	}

	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	fmt.Fprintf(&b, "\x1b[1m%s\x1b[0m\r\n", truncate(s.title(), w-1))
	status := fmt.Sprintf("%d of %d files selected, %s of %s",
		count, len(s.files), formatBytes(size), formatBytes(totalSize(s.files)))
	if s.dups {
		status += " · yellow: same name as in another link"
	}
	fmt.Fprintf(&b, "%s\r\n", truncate(status, w-1))
	b.WriteString(strings.Repeat("─", w-1) + "\r\n")

	for i := s.top; i < len(s.rows) && i < s.top+page; i++ {
		n := s.rows[i]
		icon := "  "
		if n.isDir {
			icon = "▸ "
			if s.open[s.path[n]] {
				icon = "▾ "
			}
		}
		right := ""
		switch {
		case s.info[n]:
		case n.isDir:
			right = formatBytes(totalSize(s.filesOf(n)))
		default:
			right = formatBytes(n.size)
		}
		left := fmt.Sprintf("%s %s%s%s", s.mark(n), strings.Repeat("  ", s.depth[n]), icon, label(n))
		if n.isDir {
			left += "/"
		}
		room := w - 1 - dispWidth(right) - 2
		line := padRight(left, room) + "  " + right
		var style []string
		switch {
		case s.info[n]:
			style = append(style, "2") // dim
		case n.dup:
			style = append(style, "33") // yellow
		}
		if i == s.cursor {
			style = append(style, "7") // reverse
		}
		if len(style) > 0 {
			line = "\x1b[" + strings.Join(style, ";") + "m" + line + "\x1b[0m"
		}
		b.WriteString(line + "\r\n")
	}
	for i := len(s.rows) - s.top; i < page; i++ {
		b.WriteString("\r\n")
	}
	footer := "\x1b[2m" + truncate("↑↓ move  space toggle  →← open/close  a all  enter download  q quit", w-1) + "\x1b[0m"
	if s.confirm {
		msg := "1 link still loading"
		if waiting := s.total - s.loaded; waiting > 1 {
			msg = fmt.Sprintf("%d links still loading", waiting)
		}
		msg += " — enter: start anyway, other keys: wait"
		footer = "\x1b[1;33m" + truncate(msg, w-1) + "\x1b[0m"
	}
	b.WriteString(footer)
	os.Stdout.WriteString(b.String())
}
