# CLAUDE.md

Guidance for working on `mgdl`, a Go MEGA.nz downloader (rewrite of
`../mg-download.sh`, UI modelled on `../dl`, verification from `../mg-verify`).

## Constraints

- **Dependencies:** stdlib plus `golang.org/x/term` and `golang.org/x/sys` only.
  Pinned to versions that still support Go 1.24 (newer x/ releases require 1.25+).
- **Binary units everywhere.** K/M/G are powers of 1024 (`parseSize`, `formatBytes`).
- **Build with `-trimpath`** so local paths don't leak into the binary.
- **Cross-platform** (Linux/macOS/Windows), no Windows GUI/double-click mode.
- Metadata via the MEGA API **directly**; file data **through the proxy**. A fresh
  `g` download URL for every chunk attempt; never reuse one.

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
| `main.go` | Entry, signal handling, per-link orchestration (`processLink`, `downloadLink`, `dryRun`), HTTP clients. |
| `args.go` | Hand-rolled flag parsing (short/long, bundling, repeatable `-p`), usage text, `-i` link files. |
| `link.go` | Parsing of modern, subfolder/file-in-folder and legacy links. |
| `api.go` | MEGA API client: batching, seq ids, error codes, transient retries. |
| `nodes.go` | Folder listing → decrypted tree; key selection from multi-share `k`; name sanitising and de-duplication; `-p` matching; `--list` output. |
| `crypto.go` | Key unpacking, attribute decryption, CTR IV for an offset, MAC chunk geometry, streaming `macStream`, `metaFold`, `condense`. |
| `state.go` | The control file (`.mgdl-HANDLE`), chunk planning. |
| `download.go` | Per-file job: prepare/reconcile state, parallel or sequential chunk workers, per-attempt fetch with decrypt+MAC+checkpoint, retry/throttle/stall logic, MAC check, `--verify-existing`. |
| `progress.go` | Dashboard (ported from dl) / plain-mode status lines, summary. |
| `log.go` | `infof`/`warnf`/`debugf`; erases and redraws the dashboard around each line. |
| `select.go` | `--select` tree picker (raw mode, alternate screen). |
| `term*.go` | TTY detection, width, Windows VT enablement. |

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
- **One control file per link**, all mutation through `controlFile.update` (one
  mutex), saved atomically (tmp + rename) every 2s and on exit, removed when empty.
- **Final name means verified.** Data goes to `NAME.part`; rename only after the
  MAC matches (or verification is off). Mismatch → delete + redownload, bounded by
  `-r` (3 when `-r 0`).
- **Free space is checked, sparse files are used** (`space.go`, `disk_*.go`). Up
  front: the selection's remaining need (`fileNeed`: 0 if complete, size − done if
  resumable) plus headroom (1 MiB + 0.5%) must fit, or the link stops before any
  request. Per file at start (`spaceGuard.claim`): its need plus what in-flight files
  still have to write (`spaceNeed − spaceWritten`, updated in `record`). A shortfall
  is a `localError` (no retries). Unknown free space warns and skips the check;
  `--no-space-check` disables it. Real preallocation was benchmarked and rejected:
  same write speed on ext4, no fragmentation win on a full disk.
- **Parallel `.part` files are sized up front (sparse; `markSparse` on Windows so
  NTFS doesn't zero-fill ahead of out-of-order writes); sequential ones never are.** Resuming a
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
  Windows-reserved names/characters) and duplicate sibling names get ` (2)`.
- **Piped stdout:** no cursor movement; plain timestamped lines, status every 10s.
