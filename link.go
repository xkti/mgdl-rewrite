package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// linkKind distinguishes a public file link from a public folder link.
type linkKind int

const (
	kindFile linkKind = iota
	kindFolder
)

// megaLink is a parsed public link.
type megaLink struct {
	raw    string
	kind   linkKind
	id     string // public handle of the file or folder share
	key    []byte // 32-byte file key, or 16-byte folder master key
	target string // for folder links: handle of a subfolder or file inside it ("" = root)
}

// handle is the name used for this link's control file: the node the link points
// at, which is the share itself unless a subfolder or file was singled out.
func (l *megaLink) handle() string {
	if l.target != "" {
		return l.target
	}
	return l.id
}

var (
	// Modern: https://mega.nz/file/ID#KEY, /folder/ID#KEY[/folder|file/HANDLE]
	reModern = regexp.MustCompile(`^/(file|folder)/([A-Za-z0-9_-]{8})$`)
	// Fragment of a modern folder link that singles out a node inside it.
	reTarget = regexp.MustCompile(`^([A-Za-z0-9_+=-]+)(?:/(?:folder|file)/([A-Za-z0-9_-]{8}))?$`)
	// Legacy: #!ID!KEY (file), #F!ID!KEY[!HANDLE] (folder).
	reLegacyFile   = regexp.MustCompile(`^!([A-Za-z0-9_-]{8})!([A-Za-z0-9_+/=-]+)$`)
	reLegacyFolder = regexp.MustCompile(`^F!([A-Za-z0-9_-]{8})!([A-Za-z0-9_+/=-]+?)(?:[!?]([A-Za-z0-9_-]{8}))?$`)
)

// parseLink accepts the link forms MEGA has handed out over the years. Whitespace
// and Windows line endings (links pasted from a .txt) are trimmed.
func parseLink(s string) (*megaLink, error) {
	s = strings.TrimSpace(strings.TrimRight(s, "\r\n"))
	if s == "" {
		return nil, errors.New("empty link")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("not a URL: %q", s)
	}
	host := strings.ToLower(strings.TrimPrefix(u.Host, "www."))
	if host != "mega.nz" && host != "mega.co.nz" && host != "mega.io" {
		return nil, fmt.Errorf("not a MEGA link: %q", s)
	}

	l := &megaLink{raw: s}
	frag := u.Fragment
	var keyStr string

	switch {
	case reModern.MatchString(u.Path):
		m := reModern.FindStringSubmatch(u.Path)
		l.id = m[2]
		t := reTarget.FindStringSubmatch(frag)
		if t == nil {
			return nil, fmt.Errorf("link is missing its key (the part after #): %q", s)
		}
		keyStr = t[1]
		if m[1] == "file" {
			l.kind = kindFile
			if t[2] != "" {
				return nil, fmt.Errorf("file links cannot point inside a folder: %q", s)
			}
		} else {
			l.kind = kindFolder
			l.target = t[2]
		}

	case (u.Path == "" || u.Path == "/") && reLegacyFile.MatchString(frag):
		m := reLegacyFile.FindStringSubmatch(frag)
		l.kind, l.id, keyStr = kindFile, m[1], m[2]

	case (u.Path == "" || u.Path == "/") && reLegacyFolder.MatchString(frag):
		m := reLegacyFolder.FindStringSubmatch(frag)
		l.kind, l.id, keyStr, l.target = kindFolder, m[1], m[2], m[3]

	default:
		return nil, fmt.Errorf("unrecognised MEGA link format: %q", s)
	}

	key, err := b64dec(keyStr)
	if err != nil {
		return nil, fmt.Errorf("link key is not valid base64: %q", s)
	}
	want := 32
	if l.kind == kindFolder {
		want = 16
	}
	if len(key) != want {
		return nil, fmt.Errorf("link key has the wrong length (%d bytes, want %d) — was the link copied in full?", len(key), want)
	}
	l.key = key
	return l, nil
}
