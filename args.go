package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

// defaultProxies are the Cloudflare Workers that relay userstorage downloads;
// one is picked at random per chunk attempt, as with -x.
var defaultProxies = []string{
	"https://flat-resonance-cfdb.ililli.workers.dev",
	"https://m.ndhqaroux.workers.dev",
	"https://a.qi5mfqlu7.workers.dev",
	"https://x.hit4zt0oj.workers.dev",
}

const (
	defaultChunk = 256 << 20
	minChunk     = 128 << 20
)

var errNoLinks = errors.New("no link given")

// config holds every setting resolved from the command line.
type config struct {
	links          []string
	output         string
	jobs           int   // files downloaded at once
	connections    int   // connections per file (parallel mode)
	chunkSize      int64 // bytes per download chunk
	retries        int   // attempts per chunk; 0 = forever
	proxies        []string
	direct         bool
	paths          []string
	list           bool
	selectTUI      bool
	dryRun         bool
	sequential     bool
	verify         bool
	verifyExisting bool
	limitRate      int64
	noSpaceCheck   bool
	verbose        bool
	logFile        string
}

// parseArgs implements the command line by hand (as dl does) so short and long forms
// are interchangeable, flags may appear anywhere, and -p can repeat.
func parseArgs(argv []string) (*config, error) {
	cfg := &config{
		output:      ".",
		jobs:        4,
		connections: 4,
		chunkSize:   defaultChunk,
		retries:     5,
		verify:      true,
	}
	var proxies, rate string
	var inputs []string

	takesValue := map[string]bool{
		"o": true, "output": true, "j": true, "jobs": true, "c": true, "connections": true,
		"C": true, "chunk-size": true, "r": true, "retries": true, "x": true, "proxy": true,
		"p": true, "path": true, "i": true, "input": true, "limit-rate": true, "log": true,
	}

	posInt := func(flag, val string, allowZero bool) (int, error) {
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 || (n == 0 && !allowZero) {
			kind := "a positive integer"
			if allowZero {
				kind = "a non-negative integer"
			}
			return 0, fmt.Errorf("--%s needs %s, got %q", flag, kind, val)
		}
		return n, nil
	}

	assign := func(name, val string) error {
		var err error
		switch name {
		case "o", "output":
			cfg.output = val
		case "j", "jobs":
			cfg.jobs, err = posInt("jobs", val, false)
		case "c", "connections":
			cfg.connections, err = posInt("connections", val, false)
		case "C", "chunk-size":
			if cfg.chunkSize, err = parseSize(val); err == nil && cfg.chunkSize < minChunk {
				err = fmt.Errorf("--chunk-size must be at least 128M")
			}
		case "r", "retries", "retry":
			cfg.retries, err = posInt("retries", val, true)
		case "x", "proxy":
			proxies = val
		case "direct":
			cfg.direct = true
		case "p", "path":
			cfg.paths = append(cfg.paths, val)
		case "l", "list":
			cfg.list = true
		case "s", "select":
			cfg.selectTUI = true
		case "n", "dry-run":
			cfg.dryRun = true
		case "sequential":
			cfg.sequential = true
		case "no-verify":
			cfg.verify = false
		case "verify-existing":
			cfg.verifyExisting = true
		case "no-space-check":
			cfg.noSpaceCheck = true
		case "limit-rate":
			rate = val
		case "i", "input":
			inputs = append(inputs, val)
		case "v", "verbose":
			cfg.verbose = true
		case "log":
			cfg.logFile = val
		case "V", "version":
			fmt.Println("mgdl", version)
			os.Exit(0)
		case "h", "help":
			usage(os.Stdout)
			os.Exit(0)
		default:
			return fmt.Errorf("unknown flag %q", name)
		}
		return err
	}

	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			cfg.links = append(cfg.links, argv[i+1:]...)
			i = len(argv)
		case strings.HasPrefix(arg, "--"):
			name, val, hasVal := strings.Cut(arg[2:], "=")
			if takesValue[name] && !hasVal {
				if i+1 >= len(argv) {
					return nil, fmt.Errorf("flag %q needs a value", arg)
				}
				i++
				val = argv[i]
			} else if !takesValue[name] && hasVal {
				return nil, fmt.Errorf("flag --%s does not take a value", name)
			}
			if err := assign(name, val); err != nil {
				return nil, err
			}
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			// Short flags: "-v", "-j8", "-j 8", "-nv" (bundled booleans).
			rest := arg[1:]
			for rest != "" {
				name := rest[:1]
				rest = rest[1:]
				if takesValue[name] {
					val := rest
					if val == "" {
						if i+1 >= len(argv) {
							return nil, fmt.Errorf("flag -%s needs a value", name)
						}
						i++
						val = argv[i]
					}
					if err := assign(name, strings.TrimPrefix(val, "=")); err != nil {
						return nil, err
					}
					break
				}
				if err := assign(name, ""); err != nil {
					return nil, err
				}
			}
		default:
			cfg.links = append(cfg.links, arg)
		}
	}

	for _, in := range inputs {
		links, err := readLinkFile(in)
		if err != nil {
			return nil, err
		}
		cfg.links = append(cfg.links, links...)
	}
	if len(cfg.links) == 0 {
		return nil, errNoLinks
	}
	if len(cfg.paths) > 0 && len(cfg.links) > 1 {
		return nil, errors.New("--path can only be used with a single link")
	}

	cfg.proxies = splitProxies(proxies)
	if len(cfg.proxies) == 0 {
		cfg.proxies = slices.Clone(defaultProxies)
	}
	if rate != "" {
		n, err := parseSize(rate)
		if err != nil {
			return nil, fmt.Errorf("--limit-rate: %w", err)
		}
		cfg.limitRate = n
	}
	// Compatibility with the shell script's debug switch.
	if os.Getenv("MGDL_DEBUG") == "1" {
		cfg.verbose = true
	}
	return cfg, nil
}

// readLinkFile reads one link per line from a file ("-" = stdin). Blank lines and
// lines starting with # are ignored; Windows line endings are fine.
func readLinkFile(path string) ([]string, error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("--input: %w", err)
		}
		defer f.Close()
		r = f
	}
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// splitProxies turns "a.workers.dev, https://b.dev/" into normalised base URLs.
func splitProxies(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "://") {
			part = "https://" + part
		}
		out = append(out, strings.TrimRight(part, "/"))
	}
	return out
}

// defaultProxyList formats defaultProxies for the usage text, one per line.
func defaultProxyList() string {
	hosts := make([]string, len(defaultProxies))
	for i, p := range defaultProxies {
		hosts[i] = strings.TrimPrefix(p, "https://")
	}
	return strings.Join(hosts, ",\n                          ")
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `mgdl %s — MEGA.nz downloader with on-the-fly decryption and verification

Usage:
  mgdl [flags] LINK [LINK...]
  mgdl [flags] -i links.txt

Links:
  https://mega.nz/file/ID#KEY                    a file
  https://mega.nz/folder/ID#KEY                  a whole folder
  https://mega.nz/folder/ID#KEY/folder/HANDLE    a subfolder of it
  https://mega.nz/folder/ID#KEY/file/HANDLE      a single file in it
  https://mega.nz/#!ID!KEY, https://mega.nz/#F!ID!KEY   legacy forms

Selection:
  -l, --list              print the file tree with sizes and exit
  -p, --path PATH         download only this file or folder (relative to the link's
                          folder, e.g. "Season 1/ep01.mkv"); repeatable
  -s, --select            choose files in an interactive tree (-p entries preselected)
  -n, --dry-run           show what would be downloaded or resumed; transfer nothing

Transfer:
  -o, --output DIR        where to download (default: current directory)
  -j, --jobs N            files downloaded at once (default 4)
  -c, --connections N     connections per file (default 4)
  -C, --chunk-size SIZE   bytes per request, e.g. 128M, 1G; suffixes are binary
                          (default 256M, minimum 128M)
      --sequential        fetch each file's chunks one at a time, without
                          preallocating (can't resume a parallel partial download)
  -r, --retries N         attempts per chunk; 0 = retry forever (default 5)
      --limit-rate RATE   total bandwidth cap, e.g. 40M, 500K (default unlimited)
  -x, --proxy LIST        comma-separated proxy domains, one picked at random per
                          request (default:
                          %s)
      --direct            don't use a proxy; fetch from MEGA directly
  -i, --input FILE        read links from FILE, one per line ("-" = stdin)
      --no-space-check    don't check for free disk space before downloading

Verification:
      --no-verify         decrypt only; skip the MAC check
      --verify-existing   re-verify already-downloaded files instead of skipping them

Output:
  -v, --verbose           detailed logging of every stage (to stderr)
      --log FILE          also write detailed logs to FILE
  -V, --version           print the version
  -h, --help              show this help

Files are decrypted and verified while they download, into NAME.part, and get
their final name only once complete and verified. Progress is saved in a control
file (.mgdl-HANDLE, beside the file or at the top of the folder) so an interrupted
run resumes where it stopped: just run the same command again.

Several links download together, sharing the -j file slots. Folders with the
same name are merged; if two links have a file with the same name, both are kept
and the later one gets its MEGA handle added to its name.

When stdout is not a terminal, the live dashboard is replaced by periodic status
lines. Sizes use binary units (1 MiB = 1024 KiB).
`, version, defaultProxyList())
}
