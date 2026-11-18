# CLAUDE.md

Guidance for working on `mgdl`, a Go MEGA.nz downloader (rewrite of
`../mg-download.sh`, UI modelled on `../dl`, verification from `../mg-verify`).

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
go test -race ./...
```

Tests are hermetic (`e2e_test.go` runs a fake API + storage + proxy with
`httptest`; keys and handles are synthetic). No test touches the real network.

## File map

| File | Responsibility |
|---|---|
| `main.go` | Entry, signal handling, exit codes, `dryRun`, HTTP clients. |
| `run.go` | A run over all links: `planLinks` (resolve, repeat links, `mergeTrees`, select, control files, checks, `checkSpace`), `downloadAll` (one queue + dashboard), `linkReport`. |
| `args.go` | Hand-rolled flag parsing (short/long, bundling, repeatable `-p`), usage text, `-i` link files. |
| `link.go` | Parsing of modern, subfolder/file-in-folder and legacy links. |
| `api.go` | MEGA API client: batching, seq ids, error codes, transient retries. |
| `nodes.go` | Folder listing → decrypted tree; key selection from multi-share `k`; name sanitising and de-duplication (within a link: `finishTree`; across links: `mergeTrees`); `-p` matching; `--list` output. |
| `crypto.go` | Key unpacking, attribute decryption, CTR IV for an offset, MAC chunk geometry, streaming `macStream`, `metaFold`, `condense`. |
| `state.go` | The control file (`.mgdl-HANDLE`), chunk planning. |
| `download.go` | Per-file job: prepare/reconcile state, parallel or sequential chunk workers, per-attempt fetch with decrypt+MAC+checkpoint, retry/throttle/stall logic, MAC check, `--verify-existing`. |
| `progress.go` | Dashboard (ported from dl) / plain-mode status lines, summary. |
| `log.go` | `infof`/`warnf`/`debugf`; erases and redraws the dashboard around each line. |
| `select.go` | `--select` tree picker (raw mode, alternate screen). |
| `term*.go` | TTY detection, width, Windows VT enablement. |
| `width.go` | Display width (wide CJK/emoji = 2, combining = 0), `truncate`, `padRight`. |
| `resize_*.go` | `watchResize`: SIGWINCH on Unix, width polling (250 ms) on Windows. |

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
  queue, `-j` workers, one dashboard and one summary, followed by `linkReport` (one
  line per group of same-named links) when there are several. A link that fails to
  plan is reported and dropped; the rest go ahead. A link whose handle an earlier,
  successfully resolved link already had is skipped as a repeat (it would share
  the control file). Same-named files in different links are **not** de-duplicated:
  both are downloaded, as below.
- **One control file per link**, all mutation through `controlFile.update` (one
  mutex), saved atomically (tmp + rename) every 2s and on exit, removed when empty.
- **Final name means verified.** Data goes to `NAME.part`; rename only after the
  MAC matches (or verification is off). Mismatch → delete + redownload, bounded by
  `-r` (3 when `-r 0`).
- **Free space is checked, sparse files are used** (`space.go`, `disk_*.go`). Up
  front: the selection's remaining need (`fileNeed`: 0 if complete, size − done if
  resumable) plus headroom (1 MiB + 0.5%) must fit, summed over all links landing on
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
  without spending a retry (up to 20; API throttles wait 5s, 10s, … up to 30s).
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
- **Piped stdout:** no cursor movement; plain timestamped lines, status every 10s.
- **Terminal output is measured in display columns** (`dispWidth`), never runes or
  bytes, and every drawn line stays narrower than the terminal so none wraps.
  Dashboard rows (`rowLayout`): the name column takes what's left after the bar,
  size, percentage and tail, up to the longest name; the bar shrinks (20 → 8) before
  names are cut, and is hidden when a minimum bar plus a minimum name won't fit. Redraws happen on every tick and on resize; after a shrink the
  previous frame's rows are recounted as the terminal re-wrapped them
  (`frameRows`) before moving up, and each frame ends with `ESC[J`.
