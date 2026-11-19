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

// handle is the name used for this link's control file: the file or the folder
// share. A link to a subfolder or file inside a share downloads into the share's
// own tree, so it shares the share's control file.
func (l *megaLink) handle() string { return l.id }

var (
	// Path of a modern link: /file/ID or /folder/ID. The ID is validated separately
	// so a truncated one gets a helpful message.
	reModern = regexp.MustCompile(`^/(file|folder)/([^/]*)$`)
	// Legacy fragments: !ID!KEY (file), F!ID!KEY[!HANDLE] (folder).
	reLegacyFile   = regexp.MustCompile(`^!([^!]*)!([^!]*)$`)
	reLegacyFolder = regexp.MustCompile(`^F!([^!]*)!([^!?]*)(?:[!?](.*))?$`)
	reHandle       = regexp.MustCompile(`^[A-Za-z0-9_-]{8}$`)
	reKeyChars     = regexp.MustCompile(`^[A-Za-z0-9_+/-]*$`)
)

// Key lengths in characters of unpadded base64: 32 bytes for a file, 16 for a folder.
const (
	fileKeyChars   = 43
	folderKeyChars = 22
)

// copiedInFull ends every message about a link that looks cut short.
const copiedInFull = " — was the link copied in full?"

// parseLink accepts the link forms MEGA has handed out over the years. Whitespace
// and Windows line endings (links pasted from a .txt) are trimmed. Every part is
// checked before any request is made, so a partially pasted link is reported as
// such rather than failing later with a confusing error.
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
	legacyPath := u.Path == "" || u.Path == "/"

	switch {
	case reModern.MatchString(u.Path):
		m := reModern.FindStringSubmatch(u.Path)
		l.id = m[2]
		l.kind = kindFolder
		if m[1] == "file" {
			l.kind = kindFile
		}
		// Fragment: KEY, or for folders KEY/folder/HANDLE or KEY/file/HANDLE.
		var rest string
		var hasRest bool
		keyStr, rest, hasRest = strings.Cut(frag, "/")
		if hasRest {
			what, h, ok := strings.Cut(rest, "/")
			if !ok || (what != "folder" && what != "file") {
				return nil, fmt.Errorf("unrecognised text after the key: %q", "/"+rest)
			}
			if l.kind == kindFile {
				return nil, fmt.Errorf("file links cannot point inside a folder: %q", s)
			}
			if !reHandle.MatchString(h) {
				return nil, fmt.Errorf("the %s handle %q after the key should be 8 characters%s", what, h, copiedInFull)
			}
			l.target = h
		}

	case legacyPath && reLegacyFile.MatchString(frag):
		m := reLegacyFile.FindStringSubmatch(frag)
		l.kind, l.id, keyStr = kindFile, m[1], m[2]

	case legacyPath && reLegacyFolder.MatchString(frag):
		m := reLegacyFolder.FindStringSubmatch(frag)
		l.kind, l.id, keyStr = kindFolder, m[1], m[2]
		if strings.ContainsAny(frag[len("F!")+len(m[1])+1:], "!?") {
			if !reHandle.MatchString(m[3]) {
				return nil, fmt.Errorf("the handle %q after the key should be 8 characters%s", m[3], copiedInFull)
			}
			l.target = m[3]
		}

	default:
		return nil, fmt.Errorf("unrecognised MEGA link format: %q", s)
	}

	if !reHandle.MatchString(l.id) {
		return nil, fmt.Errorf("the link ID %q should be 8 characters%s", l.id, copiedInFull)
	}
	key, err := parseKey(keyStr, l.kind)
	if err != nil {
		return nil, err
	}
	l.key = key
	return l, nil
}

// parseKey validates and decodes a link key. The length is checked in characters
// before decoding: some truncated lengths are not valid base64 at all, and the
// useful message for those is still "copied in full?", not "invalid base64".
func parseKey(keyStr string, kind linkKind) ([]byte, error) {
	k := strings.TrimRight(keyStr, "=")
	if k == "" {
		return nil, errors.New("link is missing its key (the part after #)" + copiedInFull)
	}
	if !reKeyChars.MatchString(k) {
		return nil, fmt.Errorf("link key %q contains characters that can't appear in a MEGA key", keyStr)
	}
	want := fileKeyChars
	if kind == kindFolder {
		want = folderKeyChars
	}
	if len(k) != want {
		return nil, fmt.Errorf("link key is %d characters long, expected %d%s", len(k), want, copiedInFull)
	}
	key, err := b64dec(k)
	if err != nil {
		return nil, fmt.Errorf("link key is not valid base64: %w", err)
	}
	return key, nil
}
