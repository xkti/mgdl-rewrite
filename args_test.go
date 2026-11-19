package main

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Placeholder links: syntactically valid, not real shares.
const (
	testFileLink   = "https://mega.nz/file/AbCdEfGh#" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testFolderLink = "https://mega.nz/folder/AbCdEfGh#" + "AAAAAAAAAAAAAAAAAAAAAA"
)

func TestParseSizeBinary(t *testing.T) {
	cases := map[string]int64{
		"1024": 1024, "1K": 1024, "1k": 1024, "256M": 256 << 20, "1G": 1 << 30,
		"1.5G": 3 << 29, "128MiB": 128 << 20, "64MB": 64 << 20, "2T": 2 << 40,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "M", "-1M", "abc", "0"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) should fail", bad)
		}
	}
}

func TestParseArgs(t *testing.T) {
	cfg, err := parseArgs([]string{"-j8", "-c", "2", "--chunk-size=128M", testFolderLink,
		"-p", "a/b", "--path", "c.txt", "-r", "0", "-nv", "--no-verify", "--sequential",
		"-x", "one.example.dev, https://two.example.dev/", "-o", "out"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.jobs != 8 || cfg.connections != 2 || cfg.chunkSize != 128<<20 || cfg.retries != 0 {
		t.Fatalf("numbers: %+v", cfg)
	}
	if !cfg.dryRun || !cfg.verbose || cfg.verify || !cfg.sequential || cfg.output != "out" {
		t.Fatalf("flags: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.paths, []string{"a/b", "c.txt"}) {
		t.Fatalf("paths: %v", cfg.paths)
	}
	if !reflect.DeepEqual(cfg.proxies, []string{"https://one.example.dev", "https://two.example.dev"}) {
		t.Fatalf("proxies: %v", cfg.proxies)
	}
	if !reflect.DeepEqual(cfg.links, []string{testFolderLink}) {
		t.Fatalf("links: %v", cfg.links)
	}
}

func TestParseArgsDefaults(t *testing.T) {
	cfg, err := parseArgs([]string{testFileLink})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.jobs != 4 || cfg.connections != 4 || cfg.chunkSize != 256<<20 || cfg.retries != 5 ||
		!cfg.verify || cfg.output != "." || !reflect.DeepEqual(cfg.proxies, defaultProxies) {
		t.Fatalf("defaults: %+v", cfg)
	}
}

func TestParseArgsErrors(t *testing.T) {
	for _, argv := range [][]string{
		{},
		{"-j", "0", testFileLink},
		{"-C", "127M", testFileLink},
		{"--bogus", testFileLink},
		{"--list=yes", testFileLink},
		{testFileLink, "-o"},
	} {
		if _, err := parseArgs(argv); err == nil {
			t.Errorf("parseArgs(%q) should fail", argv)
		}
	}
}

func TestInputFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "links.txt")
	os.WriteFile(p, []byte("# comment\r\n"+testFileLink+"\r\n\r\n  "+testFolderLink+"  \n"), 0o644)
	cfg, err := parseArgs([]string{"-i", p})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.links) != 2 || strings.ContainsAny(cfg.links[0], "\r ") {
		t.Fatalf("links: %q", cfg.links)
	}
}

func TestParseLink(t *testing.T) {
	key43 := strings.Repeat("A", 43)
	key22 := strings.Repeat("B", 22)
	cases := []struct {
		in             string
		kind           linkKind
		id, target     string
		handleForState string
	}{
		{"https://mega.nz/file/AbCdEfGh#" + key43, kindFile, "AbCdEfGh", "", "AbCdEfGh"},
		{"https://mega.nz/folder/AbCdEfGh#" + key22, kindFolder, "AbCdEfGh", "", "AbCdEfGh"},
		{"https://mega.nz/folder/AbCdEfGh#" + key22 + "/folder/Zz9_-xYw", kindFolder, "AbCdEfGh", "Zz9_-xYw", "AbCdEfGh"},
		{"https://mega.nz/folder/AbCdEfGh#" + key22 + "/file/Zz9_-xYw", kindFolder, "AbCdEfGh", "Zz9_-xYw", "AbCdEfGh"},
		{"https://mega.nz/#!AbCdEfGh!" + key43, kindFile, "AbCdEfGh", "", "AbCdEfGh"},
		{"https://mega.co.nz/#F!AbCdEfGh!" + key22, kindFolder, "AbCdEfGh", "", "AbCdEfGh"},
		{"https://mega.nz/#F!AbCdEfGh!" + key22 + "!Zz9_-xYw", kindFolder, "AbCdEfGh", "Zz9_-xYw", "AbCdEfGh"},
		{"  https://www.mega.nz/file/AbCdEfGh#" + key43 + "\r\n", kindFile, "AbCdEfGh", "", "AbCdEfGh"},
	}
	for _, c := range cases {
		l, err := parseLink(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if l.kind != c.kind || l.id != c.id || l.target != c.target || l.handle() != c.handleForState {
			t.Errorf("%q: got %+v", c.in, l)
		}
	}
	for _, bad := range []string{
		"https://example.com/file/AbCdEfGh#" + key43,
		"https://mega.nz/file/AbCdEfGh",
		"https://mega.nz/file/AbCdEfGh#" + key22,   // folder-length key on a file link
		"https://mega.nz/folder/AbCdEfGh#" + key43, // and vice versa
		"https://mega.nz/file/AbCdEfGh#" + key43 + "/file/Zz9_-xYw",
		"not a url",
	} {
		if _, err := parseLink(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"normal.txt": "normal.txt", "a/b": "a_b", `a\b`: "a_b",
		"": "_", "tab\there": "tab_here",
	}
	if runtime.GOOS == "windows" {
		// Trailing dots and spaces are invalid on Windows and are stripped, and
		// device names are reserved even with an extension.
		maps.Copy(cases, map[string]string{
			"..": "_", ".": "_", "name.": "name", "name . ": "name",
			"CON": "_CON", "nul": "_nul", "NUL.txt": "_NUL.txt", "con.tar.gz": "_con.tar.gz",
			"COM1": "_COM1", "LPT9.log": "_LPT9.log", "CONSOLE": "CONSOLE", "a:b?": "a_b_",
		})
	} else {
		maps.Copy(cases, map[string]string{
			"..": "_..", ".": "_.", "CON": "CON", "name.": "name.", "a:b?": "a:b?",
		})
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every way a link can be cut short says so, rather than a misleading error.
func TestParseLinkTruncated(t *testing.T) {
	key43 := strings.Repeat("A", 43)
	key22 := strings.Repeat("B", 22)
	file := "https://mega.nz/file/AbCdEfGh#"
	folder := "https://mega.nz/folder/AbCdEfGh#"

	var cut []string
	for n := 1; n <= 5; n++ { // covers every length mod 4, incl. invalid base64 ones
		cut = append(cut, file+key43[:43-n], folder+key22[:22-n])
	}
	cut = append(cut,
		file+key43+"A", // one too many
		file, folder,   // nothing after #
		"https://mega.nz/file/AbCdEfGh",
		"https://mega.nz/file/AbCdE#"+key43, // short ID
		folder+key22+"/folder/Zz9_",         // short handle
		folder+key22+"/folder/",             // handle missing
		folder+key22+"/file/Zz9_-xY",
		"https://mega.nz/#!AbCdEfG!"+key43,
		"https://mega.nz/#F!AbCdEfGh!"+key22+"!Zz9",
		"https://mega.nz/#F!AbCdEfGh!"+key22[:20],
	)
	for _, l := range cut {
		_, err := parseLink(l)
		if err == nil || !strings.Contains(err.Error(), "copied in full") {
			t.Errorf("%q: want a 'copied in full' error, got %v", l, err)
		}
	}

	// Right length, wrong characters: a different message.
	if _, err := parseLink(file + strings.Repeat("A", 42) + "*"); err == nil || strings.Contains(err.Error(), "copied in full") {
		t.Errorf("bad characters: got %v", err)
	}
	// Junk after the key.
	if _, err := parseLink(folder + key22 + "/bogus/Zz9_-xYw"); err == nil || !strings.Contains(err.Error(), "unrecognised") {
		t.Errorf("junk after key: got %v", err)
	}
}
