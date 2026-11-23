# CLAUDE.md

Guidance for working on `mgdl`, a Go MEGA.nz downloader (rewrite of
`../mg-download.sh`, UI modelled on `../dl`, verification from `../mg-verify`).
It decrypts **and verifies** files while they download, resumes interrupted
downloads without re-reading what is on disk, and fetches large files in parallel
chunks through a Cloudflare Worker proxy. `README.md` is the user's own, written
for the public; this file is the reference for how mgdl actually behaves.

## What it does (user's view)

```sh
mgdl https://mega.nz/file/ID#KEY                     # a file
mgdl https://mega.nz/folder/ID#KEY                   # a folder, into ./FolderName/
mgdl https://mega.nz/folder/ID#KEY/folder/HANDLE     # a subfolder, in its place in ./FolderName/
mgdl https://mega.nz/folder/ID#KEY/file/HANDLE       # one file from a folder, likewise
mgdl -l LINK                                         # list the tree and exit
mgdl -p 'Season 1' -p extras/notes.txt LINK          # only these paths
mgdl -s LINK                                         # pick files interactively
mgdl -n LINK                                         # dry run
mgdl -C 128M -c 8 -o /mnt/media LINK                 # smaller chunks, more connections
mgdl -i links.txt                                    # many links, one per line
```

Legacy links (`https://mega.nz/#!ID!KEY`, `https://mega.nz/#F!ID!KEY`) work too.
Running the same command again resumes. The usage text in `args.go` (`usage`) is
the authority; keep this table in step with it.

| Flag | Default | Meaning |
|---|---|---|
| `-o, --output DIR` | `.` | Download destination. |
| `-j, --jobs N` | `4` | Files downloaded at once (shared by all links). |
| `-c, --connections N` | `4` | Connections per file (chunks fetched in parallel). |
| `-C, --chunk-size SIZE` | `256M` | Bytes per request; binary suffixes. Min `128M`. Fixed per file once planned. |
| `--sequential` | off | One chunk at a time per file, no preallocation. Refused for a partial started in parallel. |
| `-r, --retries N` | `5` | Attempts per chunk (`--retry` also accepted); `0` = forever. Resets after ≥ 8 MiB progress. |
| `-x, --proxy LIST` | 5 built in (`defaultProxies`) | Comma-separated proxy domains (`https://` added if missing), one picked at random per chunk attempt. Repeatable. |
| `--direct` | off | Skip the proxy. |
| `-p, --path PATH` | | File or folder relative to the link's top folder. Repeatable; applies to every link. |
| `-l, --list` | | Print the tree with sizes, then exit. |
| `-s, --select` | | Interactive picker over all links (`-p` entries and subfolder/file links preselected). Needs a TTY on stdin and stdout (exit 2 otherwise). |
| `-n, --dry-run` | | Report what would be downloaded/resumed/skipped. Writes nothing. |
| `--no-verify` | | Decrypt only; skip the MAC check. Also disables `--verify-existing`. |
| `--verify-existing` | | Re-check finished files on disk; one that fails is deleted and downloaded again (the usage text warns). |
| `--limit-rate RATE` | | Total bandwidth cap, e.g. `40M`. |
| `-i, --input FILE` | | Links from a file (`-` = stdin), one per line, `#` comments, CRLF ok. Repeatable; added after command-line links. |
| `--no-space-check` | | Skip the free-space check. |
| `-v, --verbose` | | `-v` detail to stderr. `MGDL_DEBUG=1` does the same. |
| `--log FILE` | | Append everything printed plus `-v` detail to FILE (see invariants). |
| `-V, --version` / `-h, --help` | | Version / full usage. A usage error prints only `shortUsage`. |

Short flags bundle (`-nv`, `-j8`), flags may go anywhere, `--` ends flags.
Exit status: 0 success (or picker cancelled), 1 any failure, 2 usage error (or
`--select` without a terminal, or `--log` can't be opened), 130 interrupted.
First Ctrl+C/SIGTERM stops gracefully and saves the control file; a second one
more than 1s later exits at once (progress up to the last 2s checkpoint is kept).

Data path, briefly: metadata from the MEGA API over the user's own connection;
data via `PROXY/http://gfs…/dl/…/START-END`; each chunk AES-128-CTR decrypted from
its offset with CBC-MACs computed on the way to disk; the folded meta-MAC is
compared with the file key's when the file completes. Everything else is in the
invariants below.

## Constraints

- **Dependencies:** stdlib plus `golang.org/x/term` and `golang.org/x/sys` only.
  Kept at their latest releases; those set the minimum Go version (1.26 for
  x/sys v0.50.0 and x/term v0.48.0). Run `go fix ./...` after raising it.
- **Binary units everywhere.** K/M/G are powers of 1024 (`parseSize`, `formatBytes`).
- **Build with `-trimpath`** so local paths don't leak into the binary.
- **Cross-platform** (Linux/macOS/Windows), no Windows GUI/double-click mode.
- Metadata via the MEGA API **directly**; file data **through the proxy**. A fresh
  `g` download URL for every chunk attempt; never reuse one.
- **Plain words in normal output, detail in warnings and errors.** Status lines,
  the dashboard, summaries and usage text use everyday words a non-technical user
  understands ("done", "verified", not "MAC-checked"). Warnings and errors keep the
  technical detail (e.g. `failed verification (MAC mismatch)`) so problems can be
  diagnosed; don't simplify them. Deeper detail goes in `-v` debug output.
- **"verified" is reserved for `--verify-existing`** (a finished file re-checked on
  disk). A fresh download prints `done`, coloured green (checked), yellow (verify on
  but the partial was started or resumed with `--no-verify`, so it can't be) or
  plain (`--no-verify`). The summary's `(N verified)` count stays as is.

## Build / test

```sh
go build -trimpath -o mgdl .
GOOS=windows GOARCH=amd64 go build -trimpath -o mgdl.exe .
go test -race ./...
./build.sh    # release: windows, linux, freebsd, darwin (amd64+arm64) into dist/ + SHA256SUMS
```

`build.sh` builds static (`CGO_ENABLED=0`), stripped (`-s -w`) binaries and sets
`main.version` from `git describe --tags --always --dirty` (the source default is
`0.1.0`).

Tests are hermetic (`e2e_test.go` runs a fake API + storage + proxy with
`httptest`; keys and handles are synthetic). No test touches the real network.

## File map

| File | Responsibility |
|---|---|
| `main.go` | Entry, signal handling, exit codes, the closing report and totals, `dryRun`, HTTP clients. |
| `run.go` | A run over all links: `planLinks` (resolve, repeat links, `mergeTrees`, select, control files, checks, `checkSpace`), `downloadAll` (one queue + dashboard), `linkReport`. |
| `args.go` | Hand-rolled flag parsing (short/long, bundling, repeatable `-p`), usage text, `-i` link files. |
| `link.go` | Parsing of modern, subfolder/file-in-folder and legacy links. |
| `api.go` | MEGA API client: batching, seq ids, error codes, transient retries. |
| `nodes.go` | Folder listing → decrypted tree; key selection from multi-share `k`; name sanitising and de-duplication (within a link: `finishTree`; across links: `mergeTrees`); `-p` matching; `--list` output. |
| `crypto.go` | Key unpacking, attribute decryption, CTR IV for an offset, MAC chunk geometry, streaming `macStream`, parallel whole-file `fileMeta` (`--verify-existing`), `metaFold`, `condense`. |
| `state.go` | The control file (`.mgdl-HANDLE`, in the tree's base dir: beside a file link's file, at the top of a folder's download), chunk planning. |
| `space.go` | Free-space check: `fileNeed`, `spaceFor` (up front, per filesystem directory), `spaceGuard` (per file at start). |
| `disk_*.go` | Free space and `markSparse`, per platform. |
| `download.go` | Per-file job: prepare/reconcile state, parallel or sequential chunk workers, per-attempt fetch with decrypt+MAC+checkpoint, retry/throttle/stall logic, MAC check, `--verify-existing`. |
| `progress.go` | Dashboard (ported from dl) / plain-mode status lines; `summary` collects the closing figures (`runStats`, printed by `main` via `lines`). |
| `log.go` | `infof`/`warnf`/`noticef`/`debugf`, `outf`/`errorf` (stdout/stderr as is); erases and redraws the dashboard around each line; the `--log` file (`CMD`, every printed line, `PROG`, `EXIT`). |
| `select.go` | `--select` picker for all links at once (raw mode, alternate screen): sections fill in as links load, same-named folders merged, duplicates yellow. |
| `term*.go` | TTY detection, width, Windows VT enablement. |
| `util.go` | `--limit-rate` token bucket, `parseSize`/`formatBytes`/speed/ETA/duration formatting, `bar`, `plural`. |
| `width.go` | Display width (wide CJK/emoji = 2, combining = 0), `truncate`, `padRight`, `fitNames` (name + text listings). |
| `resize_*.go` | `watchResize`: SIGWINCH on Unix, width polling (250 ms) on Windows. |
| `worker/index.js` | The Cloudflare Worker that relays `*.userstorage.mega.co.nz` requests; deploy your own and pass it with `-x`. |
| `worker/wrangler.toml` | Wrangler config for it (worker `mgdl`, entry `index.js`): `cd worker && npx wrangler deploy`, then `-x mgdl.SUBDOMAIN.workers.dev`. |
| `build.sh` | Release cross-compile into `dist/`. |
| `*_test.go` | `e2e_test.go` (fake API/storage/proxy, full runs), `multi_test.go` (several links), `rename_sparse_test.go`, plus unit tests per file. |

## Key invariants — don't break these

- **MAC chunk geometry.** Chunks are 1..8 × 128 KiB, then 1 MiB forever; the
  steady stride starts at 36 × 128 KiB = **4.5 MiB**. Tested against a naive
  port of mg-verify (`refMeta`).
- **Download-chunk boundaries sit on MAC boundaries** (`planChunks` rounds up), so
  each chunk's MACs are computed independently by its own worker.
- **Checkpoints sit on MAC boundaries.** `chunkState.Done` only advances when a MAC
  chunk completes (or, with `--no-verify`, to the MAC boundary floor). Resume thus
  restarts MAC and CTR cleanly at that offset; nothing is re-read from disk.
- **Meta-MAC fold is strictly in order.** `job.record` folds only through the
  leading run of chunks; MACs of chunks that finished early wait in `chunkState.MACs`
  (persisted). Tested with a randomised completion order.
- **All links run as one.** Every link is planned (resolved, selected, checked) in
  command-line order before any file data moves; then all selected files share one
  queue, `-j` workers and one dashboard. At the end, a single link prints its name
  over the totals; several print `linkReport` (one line per group of same-named
  links) and then the totals for all of them, with no list of names on top. A link that fails to
  plan is reported and dropped; the rest go ahead. A link whose share (or file)
  an earlier, successfully resolved link already had shares its tree and control
  file: the very same link is skipped as a repeat; a link to another part of the
  share **joins** the earlier link's run (`linkRun.joined`): its files are added to
  that run's selection (preselected in the picker), and it ends with that run's
  status (`settleJoined`). Same-named files in different links are **not** de-duplicated:
  both are downloaded, as below. Links are resolved four at a time
  (`resolveInOrder`) but accepted (repeat check, `nameMerger.add`, selection)
  strictly in order on one goroutine — with `--select`, the picker's — so names
  never depend on load order and nothing on screen changes under the user.
  `-p` applies to every link (`matchPaths`, relative to each link's top folder, within
  what the link points at); a link with
  no match is skipped, and an entry matching in no link stops the run before any
  download (with `--select` it only preselects, so that's just a warning).
- **Subfolder and file-in-folder links keep the share's whole tree.** `resolve`
  always roots the tree at the share; the link's target only narrows the selection
  (`selectIn` → `tree.under`), so its files land where a link to the whole share
  would put them, and the control file is the share's (`megaLink.handle` = share ID).
- **One control file per link** (per share), all mutation through `controlFile.update` (one
  mutex), saved atomically (tmp + rename) every 2s and on exit, removed when empty.
- **A file's chunk layout is fixed once planned.** It's saved with the file's state
  (`fileState.Chunk` records the `-C` used; `plannedChunk` reads older files from the
  layout), and a resume keeps it whatever `-C` says now. A `-C` given that differs
  warns once (`chunkChange`): after the first affected `resume` line in a run (not up
  front, where `skip` lines would scroll it away) and after the dry run's totals.
- **Final name means verified.** Data goes to `NAME.part`; rename only after the
  MAC matches (or verification is off). Mismatch → delete + redownload, bounded by
  `-r` (3 when `-r 0`).
- **Never delete a file mgdl didn't write.** A folder or wrong-size file at the
  final name can't be this file's finished download, so it's left alone and the
  file goes beside it as `NAME_HANDLE` (`finalPath`, notice on stderr). The first
  candidate already holding the file or its `.part` wins, so it's found again on
  later runs. Only `--verify-existing` deletes a final-name file (on a failed check).
- **Free space is checked, sparse files are used** (`space.go`, `disk_*.go`). Up
  front: the selection's remaining need (`fileNeed`: 0 if complete, size − done if
  resumable) plus headroom (`spaceMargin`: the worst case for the control file and filesystem overhead, about 0.008% of the data (82 bytes per MiB) plus 1 MiB plus ~130 KiB per file) must fit, summed over all links landing on
  the same filesystem directory, or those links stop before any request. Per file at start (`spaceGuard.claim`): its need plus what in-flight files
  still have to write (`spaceNeed − spaceWritten`, updated in `record`). A shortfall
  is a `localError` (no retries). Unknown free space warns and skips the check;
  `--no-space-check` disables it. Real preallocation was benchmarked and rejected:
  same write speed on ext4, no fragmentation win on a full disk.
- **Parallel `.part` files are sized up front (sparse; `markSparse` on Windows so
  NTFS doesn't zero-fill ahead of out-of-order writes); sequential ones never are.**
  If `markSparse` fails (exFAT/FAT32) on a fresh multi-chunk file, that file
  switches to sequential and `fileState.NoSparse` keeps it sequential on resume, so
  it's never fully allocated. A file that stops before any checkpoint
  (`discardIfEmpty`) loses its `.part` and state: there is nothing to resume. Resuming a
  parallel partial with `--sequential` is refused up front (`checkModes`).
- **Retries:** per chunk, counter reset after ≥ 8 MiB of progress; stalls (60s
  silent) are retried. Throttling — HTTP 429/503/509 and API -4/-6 — is waited out
  without spending a retry (up to 20; API throttles and HTTP 509 wait 5s, 10s, …
  up to 30s, ignoring `Retry-After`; 429/503 honour `Retry-After`, else 20s).
  The first wait per file prints one notice with the reason (`rate limited (HTTP
  509), waiting 5s`); the rest only show on the dashboard row and in `-v`. Link
  lookups do the same once per link (`withThrottleNote` → `call`), except under
  the picker, whose header shows a countdown instead (printing would garble it).
  Permanent API errors (-2, -8, -9, -11, -14, -16, -17) and local disk errors fail
  immediately.
- **No nested retries.** Metadata calls (`call`) retry on their own; download-link
  requests (`getLink` → `callOnce`) make one attempt and leave retrying to the
  chunk loop, so a failing link request costs one chunk attempt, not -r × -r.
- **Links are validated part by part before any request** (`parseLink`): ID and
  handles must be 8 characters, keys 43 (file) / 22 (folder) characters, checked
  *before* base64 decoding so every truncation says "copied in full?".
- **Any decryption failure halts the link before anything is downloaded**
  (`resolve`/`decryptNode`): bad node key, missing key for the share, bad key
  length, attributes that don't decrypt or carry no name. The share root (or the
  file of a file link) is decrypted first so a wrong link key is reported as such.
  Only the `k` pair under the share root's handle is ever used. (A MAC mismatch
  after download is verification, not decryption: delete + redownload, as above.)
- **Paths:** names are sanitised to a single component (no separators, no `..`,
  Windows-reserved names/characters). Duplicate sibling names (case-insensitive)
  get `_HANDLE` before the extension (`withHandle`); the first in sort order keeps
  its name, and generated names count as taken so nothing can collide.
  Across links (`mergeTrees`, over whole trees so names don't depend on the
  selection): same-named folders merge; any other clash gets `_HANDLE` on the later
  link's item, both are marked `node.dup`, and the rename is a notice, not a warning.
  `sanitize` returns the reason; every rename among the selected files (all files
  with `--list`) is reported on **stderr** via `noticef`, never stdout.
- **The `--log` file gets everything printed.** One `TIME LEVEL text` line per
  printed line (colours stripped, blank lines dropped), starting (after a `=== mgdl VERSION started TIME ===` header, the file being appended to) with `CMD` (the
  command line, shell-quoted) and ending with `EXIT` (`runOutcome` + exit status);
  the progress line goes in as `PROG` every 10s, dashboard or not. Print through
  `infof`/`outf`/`errorf`/… rather than `fmt` so nothing is missed.
- **Piped stdout:** no cursor movement; plain timestamped lines, status every 10s.
- **Terminal output is measured in display columns** (`dispWidth`), never runes or
  bytes, and every drawn line stays narrower than the terminal so none wraps.
  Dashboard rows (`rowLayout`): the name column takes what's left after the bar,
  size, percentage and tail, up to the longest name; the bar shrinks (20 → 8) before
  names are cut, and is hidden when a minimum bar plus a minimum name won't fit. Redraws happen on every tick and on resize; after a shrink the
  previous frame's rows are recounted as the terminal re-wrapped them
  (`frameRows`) before moving up, and each frame ends with `ESC[J`.
  One-off listings of a name and a text (the multi-link report, the dry run's
  file list) are sized by `fitNames`: full names when the line fits, otherwise
  names shrink to leave the longest text its room but keep at least half the line
  (and `minNameW`), and the text is cut instead. Piped (`outputCols` = 0), nothing
  is cut; there is no fixed name cap.
