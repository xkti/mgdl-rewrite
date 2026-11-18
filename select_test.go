package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// rowNames lists the visible rows as the picker shows them, indented by depth.
func rowNames(s *selector) []string {
	var out []string
	for _, r := range s.rows {
		name := label(r)
		if r.isDir {
			name += "/"
		}
		if r.dup {
			name += " (dup)"
		}
		out = append(out, strings.Repeat("  ", s.depth[r])+name)
	}
	return out
}

func pickerTree(root string, files ...string) *tree {
	r := &node{handle: "RrRr" + root[:4], isDir: true, origName: root}
	for i, f := range files {
		c := &node{handle: root[:4] + "FfF" + string(rune('a'+i)), origName: f, size: 10, parent: r}
		r.children = append(r.children, c)
	}
	r.name, _ = sanitize(r.origName)
	t := &tree{root: r}
	finishTree(r, &t.files)
	return t
}

// Same-named folders share one section, duplicates sit together and are marked,
// links that failed show as message rows, and a link's choice maps back to it.
func TestPickerView(t *testing.T) {
	t1 := pickerTree("Pack", "b.txt", "a.txt")
	t2 := pickerTree("Pack", "a.txt", "c.txt")
	t3 := pickerTree("Else", "z.txt")
	m := newNameMerger()
	s := newSelector(4)
	for _, tr := range []*tree{t1, t2, t3} {
		m.add(tr)
		s.addTree(tr, tr.files)
	}
	if !s.loading() || !strings.HasPrefix(s.title(), "Loading links: 3 of 4") {
		t.Fatalf("title %q", s.title())
	}
	s.addBroken(&linkRun{idx: 4, note: "bad key"})

	want := []string{
		"Else/", "  z.txt",
		"Pack/", "  a.txt (dup)", "  a_PackFfFa.txt (dup)", "  b.txt", "  c.txt",
		"link 4 couldn't be opened: bad key",
	}
	if got := rowNames(s); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if s.title() != "4 links · 1 couldn't be opened" || !s.dups {
		t.Fatalf("title %q, dups %v", s.title(), s.dups)
	}

	// Untick the second link's a.txt (the renamed duplicate) and the Else folder.
	s.cursor = 4
	s.handleKey(" ")
	s.cursor = 0
	s.handleKey(" ")
	if got := s.chosenFiles(t2); len(got) != 1 || got[0].origName != "c.txt" {
		t.Fatalf("link 2 chose %v", got)
	}
	if got := s.chosenFiles(t3); len(got) != 0 {
		t.Fatalf("link 3 chose %v", got)
	}
	if s.mark(s.rows[2]) != "[-]" || s.mark(s.rows[7]) != "   " {
		t.Fatal("folder or message marks wrong")
	}

	// Collapsing and a rebuild keep the cursor on its row.
	s.cursor = 2
	s.handleKey("\x1b[D")
	if len(s.rows) != 4 || s.current().origName != "Pack" {
		t.Fatalf("after collapse: %v", rowNames(s))
	}
	s.refresh()
	if s.current().origName != "Pack" {
		t.Fatal("cursor moved on rebuild")
	}
}

// Enter while links are loading asks first; any other key keeps waiting.
func TestPickerEnterWhileLoading(t *testing.T) {
	s := newSelector(2)
	s.addTree(pickerTree("Pack", "a.txt"), nil)
	if _, done := s.handleKey("\r"); done || !s.confirm {
		t.Fatal("first enter should ask")
	}
	if _, done := s.handleKey("j"); done || s.confirm {
		t.Fatal("another key should cancel the question")
	}
	s.handleKey("\r")
	if ev, done := s.handleKey("\r"); !done || ev != pickerDone {
		t.Fatal("second enter should start")
	}
	s.addSkipped()
	if _, done := s.handleKey("\r"); !done {
		t.Fatal("with everything loaded, enter starts at once")
	}
}

// The run uses the picker's choice, and a link still loading when the user
// confirms is skipped.
func TestPickAcrossLinks(t *testing.T) {
	m := newFakeMega(t)
	a := m.addFile("FiLeAaAa", "a.bin", 100, 1, false)
	b := m.addFile("FiLeBbBb", "b.bin", 100, 2, false)
	c := m.addFile("FiLeCcCc", "c.bin", 100, 3, false)
	l1 := m.publishFolder("One", []*fakeFile{a, b}, nil)

	old := runPickerFn
	t.Cleanup(func() { runPickerFn = old })
	runPickerFn = func(ctx context.Context, s *selector, arrivals <-chan *linkRun, arrive func(*linkRun)) (pickerEvent, error) {
		arrive(<-arrivals) // the folder: untick b.bin, then confirm before link 2 arrives
		for _, f := range s.files {
			if f.origName == "b.bin" {
				s.toggle(f)
			}
		}
		return pickerDone, nil
	}
	out := t.TempDir()
	cfg := m.config(out)
	cfg.selectTUI = true
	st, runs, err := m.runAll(context.Background(), cfg, l1, m.fileLink(c))
	if err != nil {
		t.Fatal(err)
	}
	if st.completed != 1 || runs[1].status != statusSkipped || runs[1].note != "still loading when the selection was made" {
		t.Fatalf("stats %+v, link 2 %v %q", st, runs[1].status, runs[1].note)
	}
	mustEqualFile(t, filepath.Join(out, "One", "a.bin"), a.plain)
	if fileExists(filepath.Join(out, "One", "b.bin")) {
		t.Fatal("an unticked file was downloaded")
	}

	runPickerFn = func(context.Context, *selector, <-chan *linkRun, func(*linkRun)) (pickerEvent, error) {
		return pickerCancelled, nil
	}
	if _, runs, err = m.runAll(context.Background(), cfg, l1, m.fileLink(c)); !errors.Is(err, errSelectCancelled) || runs[0].status != statusSkipped {
		t.Fatalf("cancel: %v, %v", err, runs[0].status)
	}
}
