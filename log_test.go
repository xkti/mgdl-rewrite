package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// openTestLog sends the log to a temporary file for one test and returns a
// function that closes it and returns what was written.
func openTestLog(t *testing.T, argv []string) func() string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mgdl.log")
	if err := openLog(path, argv); err != nil {
		t.Fatal(err)
	}
	return func() string {
		con.logFile.Close()
		con.logFile = nil
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
}

// Every logged line has the same "DATE TIME LEVEL text" shape: what was printed on
// stdout and stderr, the command line first and how the run ended last.
func TestLogRecordsOutputAndOutcome(t *testing.T) {
	m := newFakeMega(t)
	a := m.addFile("FiLeAaAa", "a.bin", 3<<20, 12, false)
	b := m.addFile("FiLeBbBb", "b.bin", 3<<20, 13, false)
	fakeFree(t, fixedFree(5<<20)) // enough for either, not both
	argv := []string{"-o", "my dir", m.fileLink(a), m.fileLink(b)}
	read := openTestLog(t, argv)

	cfg := m.config(t.TempDir())
	stdout := captureStdout(t, func() {
		_, runs, err := m.runAll(context.Background(), cfg, m.fileLink(a), m.fileLink(b))
		outf("\n%s", strings.Join(linkReport(runs, 0), "\n"))
		logExit(1, runOutcome(cfg, runs, err))
	})
	log := read()

	line := regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d{3} (CMD  |INFO |NOTE |WARN |ERROR|DEBUG|PROG |EXIT ) +\S`)
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if !strings.HasPrefix(lines[0], "=== mgdl ") {
		t.Fatalf("no header:\n%s", log)
	}
	for _, l := range lines[1:] {
		if !line.MatchString(l) || strings.Contains(l, "\x1b") {
			t.Fatalf("badly formed log line %q in:\n%s", l, log)
		}
	}
	for _, want := range []string{
		" CMD   " + shellQuote(os.Args[0]) + " -o 'my dir' " + shellQuote(m.fileLink(a)) + " " + shellQuote(m.fileLink(b)),
		" ERROR 2 links not started: not enough disk space",
		" INFO  Links: 2 not started",
		" EXIT  2 links: not started: not enough disk space (exit status 1)",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if !strings.HasPrefix(lines[1][24:], "CMD ") || !strings.HasPrefix(lines[len(lines)-1][24:], "EXIT ") {
		t.Errorf("CMD should come first and EXIT last:\n%s", log)
	}
	// stdout itself is unchanged: no timestamps or levels.
	if !strings.Contains(stdout, "\nLinks: 2 not started\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"-j":                        "-j",
		"https://mega.nz/file/AbCd": "https://mega.nz/file/AbCd",
		"https://mega.nz/#!a!b":     "'https://mega.nz/#!a!b'",
		"my dir":                    "'my dir'",
		"it's":                      `'it'\''s'`,
		"":                          "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestStripANSI(t *testing.T) {
	if got := stripANSI("a \x1b[32mdone\x1b[0m b \x1b[2K"); got != "a done b " {
		t.Fatalf("%q", got)
	}
}
