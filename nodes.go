package main

import (
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// node is one file or folder of a resolved link, with its name decrypted.
type node struct {
	handle   string
	isDir    bool
	origName string   // name exactly as stored on MEGA
	name     string   // name made safe for the local filesystem
	size     int64    // files only
	key      *fileKey // files only
	parent   *node
	children []*node // folders only, sorted folders-first then by name

	relPath  string // sanitised path from the link root, '/'-separated ("" for the root)
	origPath string // same, with original names, for matching -p arguments
}

// tree is a resolved link: what to download and where it belongs.
type tree struct {
	link   *megaLink
	folder string // share handle for API calls ("" for a file link)
	root   *node  // a folder, or a lone file
	files  []*node
}

// title is what the dashboard and listings call this link.
func (t *tree) title() string { return t.root.origName }

// totalSize sums the sizes of the given files.
func totalSize(files []*node) int64 {
	var n int64
	for _, f := range files {
		n += f.size
	}
	return n
}

// resolve fetches and decrypts a link's metadata.
func resolve(ctx context.Context, api *apiClient, l *megaLink) (*tree, error) {
	if l.kind == kindFile {
		fk, _ := unpackFileKey(l.key)
		fi, err := api.getFile(ctx, "", l.id, false)
		if err != nil {
			return nil, err
		}
		n := &node{handle: l.id, size: fi.Size, key: fk}
		n.origName = decryptName(fk.aes[:], fi.Attr, l.id)
		n.name = sanitizeName(n.origName)
		n.relPath, n.origPath = n.name, n.origName
		debugf("resolve: file %s %q, %d bytes", l.id, n.origName, n.size)
		return &tree{link: l, root: n, files: []*node{n}}, nil
	}

	raw, err := api.listFolder(ctx, l.id)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("folder listing is empty")
	}
	debugf("resolve: folder %s lists %d nodes", l.id, len(raw))

	// The share root is the folder whose parent is not part of the listing.
	known := make(map[string]bool, len(raw))
	for _, r := range raw {
		known[r.Handle] = true
	}
	shareRoot := ""
	for _, r := range raw {
		if r.Type == 1 && !known[r.Parent] {
			shareRoot = r.Handle
			break
		}
	}
	if shareRoot == "" {
		return nil, fmt.Errorf("cannot find the root of the folder listing")
	}

	nodes := make(map[string]*node, len(raw))
	var order []*node
	for _, r := range raw {
		if r.Type != 0 && r.Type != 1 {
			continue // trash/inbox/etc. never appear in public shares, but be safe
		}
		n := &node{handle: r.Handle, isDir: r.Type == 1, size: r.Size}
		key, err := nodeKey(l.key, r.Key, shareRoot)
		if err != nil {
			warnf("%s: %v — skipping", r.Handle, err)
			continue
		}
		attrKey := key
		if !n.isDir {
			if n.key, err = unpackFileKey(key); err != nil {
				warnf("%s: %v — skipping", r.Handle, err)
				continue
			}
			attrKey = n.key.aes[:]
		} else if len(key) != 16 {
			warnf("%s: folder key has bad length — skipping", r.Handle)
			continue
		}
		n.origName = decryptName(attrKey, r.Attr, r.Handle)
		nodes[r.Handle] = n
		order = append(order, n)
	}
	for _, r := range raw {
		n := nodes[r.Handle]
		if n == nil {
			continue
		}
		if p := nodes[r.Parent]; p != nil && p.isDir && r.Handle != shareRoot {
			n.parent = p
			p.children = append(p.children, n)
		}
	}

	rootHandle := shareRoot
	if l.target != "" {
		rootHandle = l.target
	}
	root := nodes[rootHandle]
	if root == nil {
		return nil, fmt.Errorf("node %s is not in this folder", rootHandle)
	}
	root.parent = nil // a subfolder link starts its own tree

	t := &tree{link: l, folder: l.id, root: root}
	root.name = sanitizeName(root.origName)
	if !root.isDir {
		root.relPath, root.origPath = root.name, root.origName
		t.files = []*node{root}
		return t, nil
	}
	finishTree(root, &t.files)
	return t, nil
}

// nodeKey finds and decrypts a node's key. "k" holds one or more "handle:key" pairs
// separated by "/"; the one under the share root's handle is encrypted with the
// link's master key. Other pairs belong to other shares and cannot be used, but are
// tried as a fallback in case MEGA changes the arrangement again.
func nodeKey(master []byte, k, shareRoot string) ([]byte, error) {
	var fallback []string
	for _, pair := range strings.Split(k, "/") {
		h, enc, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		if h == shareRoot {
			fallback = append([]string{enc}, fallback...)
		} else {
			fallback = append(fallback, enc)
		}
	}
	var lastErr error = fmt.Errorf("no usable key in %q", k)
	for _, enc := range fallback {
		raw, err := b64dec(enc)
		if err != nil {
			lastErr = err
			continue
		}
		key, err := decryptNodeKey(master, raw)
		if err != nil {
			lastErr = err
			continue
		}
		return key, nil
	}
	return nil, lastErr
}

// decryptName returns a node's name, falling back to its handle when the attributes
// cannot be decrypted, so one bad entry does not sink the whole folder.
func decryptName(key []byte, attr, handle string) string {
	a, err := decryptAttrs(key, attr)
	if err != nil || a.Name == "" {
		warnf("%s: cannot decrypt name (%v); using the handle instead", handle, err)
		return handle
	}
	return a.Name
}

// finishTree sorts children, de-duplicates sibling names, fills in paths, and
// collects files in display order.
func finishTree(dir *node, files *[]*node) {
	sort.SliceStable(dir.children, func(i, j int) bool {
		a, b := dir.children[i], dir.children[j]
		if a.isDir != b.isDir {
			return a.isDir
		}
		if la, lb := strings.ToLower(a.origName), strings.ToLower(b.origName); la != lb {
			return la < lb
		}
		return a.handle < b.handle
	})
	// MEGA allows siblings with identical names; the filesystem does not (and on
	// Windows/macOS, names differing only in case collide too).
	seen := map[string]int{}
	for _, c := range dir.children {
		name := sanitizeName(c.origName)
		k := strings.ToLower(name)
		if n := seen[k]; n > 0 {
			ext := ""
			if !c.isDir {
				ext = path.Ext(name)
			}
			name = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), n+1, ext)
			debugf("resolve: duplicate name %q, saving as %q", c.origName, name)
		}
		seen[k]++
		c.name = name
		c.relPath = joinRel(dir.relPath, c.name)
		c.origPath = joinRel(dir.origPath, c.origName)
		if c.isDir {
			finishTree(c, files)
		} else {
			*files = append(*files, c)
		}
	}
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// sanitizeName makes a MEGA name safe to use as a single path component: no
// separators, no control characters, never "." or "..", and on Windows none of the
// reserved characters or device names.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r < 0x20 || r == 0x7f:
			b.WriteRune('_')
		case runtime.GOOS == "windows" && strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if runtime.GOOS == "windows" {
		out = strings.TrimRight(out, ". ")
		base := strings.ToUpper(strings.SplitN(out, ".", 2)[0])
		switch base {
		case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6",
			"COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			out = "_" + out
		}
	}
	if out == "" || out == "." || out == ".." {
		out = "_" + out
	}
	return out
}

// localPath is where a file belongs on disk under the output directory. A folder
// link's files live inside a directory named after the folder.
func (t *tree) localPath(output string, n *node) string {
	if t.root.isDir {
		return filepath.Join(output, t.root.name, filepath.FromSlash(n.relPath))
	}
	return filepath.Join(output, n.name)
}

// baseDir is where the link's control file lives: beside a lone file, or at the
// top of a downloaded folder.
func (t *tree) baseDir(output string) string {
	if t.root.isDir {
		return filepath.Join(output, t.root.name)
	}
	return output
}

// selectPaths narrows the files to those under any of the given paths. Paths are
// relative to the link's root folder, use '/' separators, and match either a file or
// a whole folder. Both the original MEGA names and the sanitised local names match.
func (t *tree) selectPaths(paths []string) ([]*node, error) {
	if len(paths) == 0 {
		return t.files, nil
	}
	if !t.root.isDir {
		return nil, fmt.Errorf("--path only applies to folder links")
	}
	picked := map[*node]bool{}
	for _, p := range paths {
		want := strings.Trim(strings.ReplaceAll(p, "\\", "/"), "/")
		if want == "" {
			for _, f := range t.files {
				picked[f] = true
			}
			continue
		}
		matched := false
		for _, f := range t.files {
			for _, have := range []string{f.origPath, f.relPath} {
				if have == want || strings.HasPrefix(have, want+"/") {
					picked[f] = true
					matched = true
				}
			}
		}
		if !matched {
			return nil, fmt.Errorf("--path %q matches nothing in %q (paths are relative to the folder, see --list)", p, t.title())
		}
	}
	var out []*node
	for _, f := range t.files { // keep tree order
		if picked[f] {
			out = append(out, f)
		}
	}
	return out, nil
}

// printTree writes a `tree`-style listing with sizes.
func printTree(w io.Writer, t *tree, showHandles bool) {
	label := func(n *node) string {
		s := n.origName
		if n.isDir {
			s += "/"
		} else {
			s += "  " + formatBytes(n.size)
		}
		if showHandles {
			s += "  [" + n.handle + "]"
		}
		return s
	}
	fmt.Fprintln(w, label(t.root))
	var walk func(n *node, prefix string)
	walk = func(n *node, prefix string) {
		for i, c := range n.children {
			branch, next := "├── ", "│   "
			if i == len(n.children)-1 {
				branch, next = "└── ", "    "
			}
			fmt.Fprintln(w, prefix+branch+label(c))
			if c.isDir {
				walk(c, prefix+next)
			}
		}
	}
	if t.root.isDir {
		walk(t.root, "")
	}
	dirs := 0
	var count func(n *node)
	count = func(n *node) {
		for _, c := range n.children {
			if c.isDir {
				dirs++
				count(c)
			}
		}
	}
	count(t.root)
	fmt.Fprintf(w, "\n%d folder(s), %d file(s), %s\n", dirs, len(t.files), formatBytes(totalSize(t.files)))
}
