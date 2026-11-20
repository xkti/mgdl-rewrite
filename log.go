package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// console serialises everything printed to the terminal. While the live dashboard is
// drawn, a line cannot simply be printed: the dashboard is erased, the line written
// above it, and the dashboard redrawn underneath. The dashboard registers itself here
// so every writer goes through that dance.
//
// Three streams:
//   - info:  permanent status lines (done, skipped, failed). stdout, like dl.
//   - warn:  problems the user must see. stderr.
//   - debug: -v detail. stderr when -v is set; always to the --log file if one is open.
//
// The --log file gets all of it, one "TIME LEVEL text" line per printed line
// (colours removed, blank lines dropped): INFO for stdout, NOTE/WARN/ERROR for
// stderr, DEBUG for -v detail, PROG for the progress line every 10s (dashboard or
// not), and CMD/EXIT for the command line and how the run ended.
type console struct {
	mu      sync.Mutex
	verbose bool
	logFile io.WriteCloser
	tty     bool // stdout is a terminal (dashboard mode)
	errTTY  bool // stderr is a terminal

	// redraw hooks, set while a dashboard is live. Both are called with mu held.
	erase func()
	paint func()
}

var con = &console{}

func (c *console) emit(w io.Writer, line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.erase != nil {
		c.erase()
	}
	if (w == os.Stdout && c.tty) || (w == os.Stderr && c.errTTY) {
		fmt.Fprintf(w, "\x1b[2K%s\n", line)
	} else {
		fmt.Fprintf(w, "[%s] %s\n", time.Now().Format("15:04:05"), line)
	}
	if c.paint != nil {
		c.paint()
	}
}

func (c *console) toFile(level, text string) {
	if c.logFile == nil {
		return
	}
	c.mu.Lock()
	c.toFileLocked(level, text)
	c.mu.Unlock()
}

// toFileLocked writes text to the log, a line per line; callers hold c.mu.
func (c *console) toFileLocked(level, text string) {
	if c.logFile == nil {
		return
	}
	now := time.Now().Format("2006-01-02 15:04:05.000")
	for line := range strings.SplitSeq(stripANSI(text), "\n") {
		if strings.TrimSpace(line) != "" {
			fmt.Fprintf(c.logFile, "%s %-5s %s\n", now, level, line)
		}
	}
}

// outf prints a line (or several) on stdout as it is, with no timestamp: the
// closing report, listings, the dry run. It also goes to the log.
func outf(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	con.mu.Lock()
	defer con.mu.Unlock()
	fmt.Println(text)
	con.toFileLocked("INFO", text)
}

// errorf prints an error on stderr as "mgdl: ...", and logs it.
func errorf(format string, args ...any) {
	stderrf("ERROR", "mgdl: "+format, args...)
}

// stderrf prints a line on stderr as it is, logged at level.
func stderrf(level, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	con.mu.Lock()
	defer con.mu.Unlock()
	fmt.Fprintln(os.Stderr, text)
	con.toFileLocked(level, strings.TrimPrefix(text, "mgdl: "))
}

// stripANSI removes the escape sequences colour adds.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			break
		}
		b.WriteString(s[:i])
		s = s[i+2:]
		j := strings.IndexFunc(s, func(r rune) bool { return r >= 0x40 && r <= 0x7e })
		if j < 0 {
			s = ""
			break
		}
		s = s[j+1:]
	}
	b.WriteString(s)
	return b.String()
}

// infof prints a permanent status line on stdout.
func infof(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	con.toFile("INFO", line)
	con.emit(os.Stdout, line)
}

// warnf prints a problem on stderr.
func warnf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	con.toFile("WARN", line)
	con.emit(os.Stderr, errColour("33", "warning: ")+line)
}

// noticef prints something the user should know but that isn't a problem, on
// stderr so it stays out of piped output.
func noticef(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	con.toFile("NOTE", line)
	con.emit(os.Stderr, errColour("36", line))
}

// debugf records verbose detail: stderr with -v, and the --log file regardless.
func debugf(format string, args ...any) {
	if !con.verbose && con.logFile == nil {
		return
	}
	line := fmt.Sprintf(format, args...)
	con.toFile("DEBUG", line)
	if con.verbose {
		con.emit(os.Stderr, errColour("2", line))
	}
}

// colour wraps s in an SGR sequence when stdout is a terminal, so redirected logs
// stay free of escape codes.
func colour(code, s string) string {
	if !con.tty {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// errColour is colour for stderr.
func errColour(code, s string) string {
	if !con.errTTY {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// openLog opens (appending) the --log file and records the command line.
func openLog(path string, argv []string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	con.logFile = f
	fmt.Fprintf(f, "\n=== mgdl %s started %s ===\n", version, time.Now().Format(time.RFC3339))
	con.toFile("CMD", commandLine(argv))
	return nil
}

// logExit records how the run ended and its exit status.
func logExit(code int, outcome string) {
	con.toFile("EXIT", fmt.Sprintf("%s (exit status %d)", outcome, code))
}

// commandLine is the command as typed, quoted so it can be pasted into a shell.
func commandLine(argv []string) string {
	name := "mgdl"
	if len(os.Args) > 0 && os.Args[0] != "" {
		name = os.Args[0]
	}
	parts := []string{shellQuote(name)}
	for _, a := range argv {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=,@%+", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
