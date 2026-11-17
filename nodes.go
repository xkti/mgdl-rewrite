package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"runtime"
	"slices"
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
	renamed  string // why name differs from origName ("" if it doesn't)
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
		fi, err := api.getFile(ctx, "", l.id)
		if err != nil {
			return nil, err
		}
		n := &node{handle: l.id, size: fi.Size, key: fk}
		if n.origName, err = decryptName(fk.aes[:], fi.Attr); err != nil {
			return nil, wrongKey(err)
		}
		n.name, n.renamed = sanitize(n.origName)
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

	// Any decryption failure stops here, before anything is downloaded. The share
	// root goes first: if the master key can't decrypt it, the link's key is wrong,
	// which deserves a clearer message than whichever node happened to fail first.
	nodes := make(map[string]*node, len(raw))
	for _, r := range raw {
		if r.Handle == shareRoot {
			n, err := decryptNode(l.key, r, shareRoot)
			if err != nil {
				return nil, wrongKey(err)
			}
			nodes[r.Handle] = n
		}
	}
	for _, r := range raw {
		if r.Handle == shareRoot || (r.Type != 0 && r.Type != 1) {
			continue // other types never appear in public shares, but be safe
		}
		n, err := decryptNode(l.key, r, shareRoot)
		if err != nil {
			kind := "file"
			if r.Type == 1 {
				kind = "folder"
			}
			return nil, fmt.Errorf("cannot decrypt %s %s: %w — stopping", kind, r.Handle, err)
		}
		nodes[r.Handle] = n
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
	root.name, root.renamed = sanitize(root.origName)
	if !root.isDir {
		root.relPath, root.origPath = root.name, root.origName
		t.files = []*node{root}
		return t, nil
	}
	finishTree(root, &t.files)
	return t, nil
}

// wrongKey reports that the link's own key could not decrypt what it points at.
func wrongKey(err error) error {
	return fmt.Errorf("the link's key is wrong (%v) — was the link copied correctly?", err)
}

// decryptNode unwraps a listed node's key and decrypts its name. Any failure is an
// error: a node that can't be decrypted can't be downloaded or even named.
func decryptNode(master []byte, r rawNode, shareRoot string) (*node, error) {
	n := &node{handle: r.Handle, isDir: r.Type == 1, size: r.Size}
	key, err := nodeKey(master, r.Key, shareRoot)
	if err != nil {
		return nil, err
	}
	attrKey := key
	if n.isDir {
		if len(key) != 16 {
			return nil, fmt.Errorf("folder key is %d bytes, expected 16", len(key))
		}
	} else {
		if n.key, err = unpackFileKey(key); err != nil {
			return nil, err
		}
		attrKey = n.key.aes[:]
	}
	if n.origName, err = decryptName(attrKey, r.Attr); err != nil {
		return nil, err
	}
	return n, nil
}

// nodeKey finds and decrypts a node's key. "k" holds one or more "handle:key" pairs
// separated by "/"; only the one under the share root's handle is encrypted with
// the link's master key. The others belong to other shares and can't be decrypted
// with it (AES-ECB would "succeed" and produce garbage), so they are never tried.
func nodeKey(master []byte, k, shareRoot string) ([]byte, error) {
	for pair := range strings.SplitSeq(k, "/") {
		h, enc, ok := strings.Cut(pair, ":")
		if !ok || h != shareRoot {
			continue
		}
		raw, err := b64dec(enc)
		if err != nil {
			return nil, fmt.Errorf("node key is not valid base64: %w", err)
		}
		return decryptNodeKey(master, raw)
	}
	return nil, fmt.Errorf("no key for this share (%s) among %d key(s)", shareRoot, strings.Count(k, ":"))
}

// decryptName decrypts a node's attributes and returns its name. Attributes that
// don't decrypt, aren't valid JSON, or carry no name are all errors.
func decryptName(key []byte, attr string) (string, error) {
	a, err := decryptAttrs(key, attr)
	if err != nil {
		return "", err
	}
	if a.Name == "" {
		return "", errors.New("attributes decrypted but contain no name")
	}
	return a.Name, nil
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
	// The first item (in sort order) keeps the name; later ones get "_HANDLE"
	// before the extension. Generated names count as taken too, so a later item
	// that happens to be called that is renamed rather than colliding.
	taken := map[string]string{} // lower-cased name → original name that claimed it
	for _, c := range dir.children {
		name, why := sanitize(c.origName)
		if first, dup := taken[strings.ToLower(name)]; dup {
			msg := "another item in this folder has the same name"
			if first != c.origName {
				msg = "another item in this folder has the same name, ignoring case"
			}
			why = joinReasons(why, msg)
			for dup {
				name = withHandle(name, c.handle, c.isDir)
				_, dup = taken[strings.ToLower(name)]
			}
		}
		taken[strings.ToLower(name)] = c.origName
		c.name, c.renamed = name, why
		c.relPath = joinRel(dir.relPath, c.name)
		c.origPath = joinRel(dir.origPath, c.origName)
		if c.isDir {
			finishTree(c, files)
		} else {
			*files = append(*files, c)
		}
	}
}

// withHandle tags a name with a node handle to make it unique: "x.txt" becomes
// "x_HANDLE.txt", a folder "sub" becomes "sub_HANDLE", and a dotfile ".bashrc"
// becomes ".bashrc_HANDLE".
func withHandle(name, handle string, isDir bool) string {
	ext := ""
	if !isDir {
		if ext = path.Ext(name); ext == name {
			ext = ""
		}
	}
	return strings.TrimSuffix(name, ext) + "_" + handle + ext
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// sanitizeName makes a MEGA name safe to use as a single path component.
func sanitizeName(s string) string {
	out, _ := sanitize(s)
	return out
}

// sanitize makes a MEGA name safe to use as a single path component — no
// separators, no control characters, never "." or "..", and on Windows none of the
// reserved characters or device names — and says why it changed it ("" if not).
func sanitize(s string) (string, string) {
	var why []string
	var b strings.Builder
	var sep, ctrl, winChar bool
	for _, r := range s {
		switch {
		case r == '/' || r == '\\':
			b.WriteRune('_')
			sep = true
		case r < 0x20 || r == 0x7f:
			b.WriteRune('_')
			ctrl = true
		case runtime.GOOS == "windows" && strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
			winChar = true
		default:
			b.WriteRune(r)
		}
	}
	if sep {
		why = append(why, "contains a path separator")
	}
	if ctrl {
		why = append(why, "contains control characters")
	}
	if winChar {
		why = append(why, `contains characters Windows doesn't allow (<>:"|?*)`)
	}
	out := b.String()
	if runtime.GOOS == "windows" {
		if t := strings.TrimRight(out, ". "); t != out {
			out = t
			why = append(why, "ends in a dot or space, which Windows doesn't allow")
		}
		base := strings.ToUpper(strings.SplitN(out, ".", 2)[0])
		switch base {
		case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6",
			"COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			out = "_" + out
			why = append(why, base+" is a reserved name on Windows")
		}
	}
	if out == "" || out == "." || out == ".." {
		if s == "" {
			why = append(why, "the name is empty")
		} else if len(why) == 0 {
			why = append(why, `"." and ".." can't be used as names`)
		}
		out = "_" + out
	}
	return out, strings.Join(why, "; ")
}

func joinReasons(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// renameNotes describes every renamed item among the given files and the folders
// leading to them (the root included), in tree order, one line each.
func renameNotes(t *tree, files []*node) []string {
	seen := map[*node]bool{}
	var notes []string
	add := func(n *node) {
		if n.renamed == "" || seen[n] {
			return
		}
		seen[n] = true
		orig := n.origPath
		if n == t.root {
			orig = n.origName
		}
		notes = append(notes, fmt.Sprintf("renamed   %s → %s  (%s)", orig, n.name, n.renamed))
	}
	add(t.root)
	for _, f := range files {
		var chain []*node
		for p := f; p != nil && p != t.root; p = p.parent {
			chain = append(chain, p)
		}
		for _, c := range slices.Backward(chain) {
			add(c)
		}
	}
	return notes
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
