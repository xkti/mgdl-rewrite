# mgdl

A MEGA.nz downloader, rewritten in Go from `mg-download.sh`. Decrypts **and
verifies** files while they download, resumes interrupted downloads without
re-reading what is already on disk, and downloads large files in parallel chunks
through a Cloudflare Worker proxy.

## Build

```sh
go build -trimpath -o mgdl .
GOOS=windows GOARCH=amd64 go build -trimpath -o mgdl.exe .
```

Needs Go 1.26+. Dependencies: `golang.org/x/term` and `golang.org/x/sys` only.

## Use

```sh
mgdl https://mega.nz/file/ID#KEY                     # a file
mgdl https://mega.nz/folder/ID#KEY                   # a folder, into ./FolderName/
mgdl https://mega.nz/folder/ID#KEY/folder/HANDLE     # just a subfolder, in its place in ./FolderName/
mgdl https://mega.nz/folder/ID#KEY/file/HANDLE       # one file from a folder, likewise
mgdl -l https://mega.nz/folder/ID#KEY                # list the tree and exit
mgdl -p 'Season 1' -p extras/notes.txt LINK          # only these paths
mgdl -s LINK                                         # pick files interactively
mgdl -n LINK                                         # dry run: what would happen
mgdl -C 128M -c 8 -o /mnt/media LINK                 # smaller chunks, more connections
mgdl -i links.txt                                    # many links, one per line
```

Legacy links (`https://mega.nz/#!ID!KEY`, `https://mega.nz/#F!ID!KEY`) work too.

A link to a subfolder or a file inside a folder keeps the whole folder's layout:
only what it points at is downloaded, but into the same place a link to the whole
folder would put it. Several such links into the same folder are downloaded
together into one copy of it, and with `-s` the picker shows that folder with each
link's part already ticked.

Interrupted? Run the same command again and it picks up where it stopped.

| Flag | Default | Meaning |
|---|---|---|
| `-o, --output DIR` | `.` | Download destination. |
| `-j, --jobs N` | `4` | Files downloaded at once. |
| `-c, --connections N` | `4` | Connections per file (chunks fetched in parallel). |
| `-C, --chunk-size SIZE` | `256M` | Bytes per request; `K/M/G` are binary (1024). Min `128M`. |
| `--sequential` | off | One chunk at a time per file, no preallocation. Can't resume a partial started in parallel. |
| `-r, --retries N` | `5` | Attempts per chunk; `0` = forever. Resets after real progress. |
| `-x, --proxy LIST` | 4 built-in Workers | Comma-separated proxy domains, picked at random per request. |
| `--direct` | off | Skip the proxy. |
| `-p, --path PATH` | | File or folder to download, relative to the link's top folder (for a subfolder link too, which it narrows further). Repeatable. With several links it applies to each; links with no match are skipped, and a path that matches in none of them stops the run. |
| `-l, --list` | | Print the file tree with sizes, then exit. |
| `-s, --select` | | Interactive file picker (`-p` entries come preselected). |
| `-n, --dry-run` | | Report what would be downloaded/resumed/skipped. Writes nothing. |
| `--no-verify` | | Decrypt only; skip the MAC check. |
| `--verify-existing` | | Re-verify completed files on disk instead of trusting them. |
| `--limit-rate RATE` | | Total bandwidth cap, e.g. `40M`. |
| `-i, --input FILE` | | Read links from a file (`-` = stdin). `#` comments allowed. |
| `--no-space-check` | | Don't check free disk space before downloading. |
| `-v, --verbose` | | Log every stage to stderr. `MGDL_DEBUG=1` does the same. |
| `--log FILE` | | Also write detailed logs to a file. |
| `-V, --version` | | Print the version. |
| `-h, --help` | | Show the help. |

## How it works

- **Metadata** (names, sizes, keys, download links) comes from the MEGA API over
  your own connection.
- **Data** is fetched through the proxy (`PROXY/http://gfs…/dl/…/START-END`),
  in chunks. Download links are short-lived, so a fresh one is requested for every
  chunk and every retry.
- **Decryption and verification happen as the bytes arrive.** Each chunk is
  AES-128-CTR decrypted from its offset, and its CBC-MACs are computed on the way
  to disk. When the file is complete, the folded meta-MAC is compared with the one
  in the file key. A mismatch deletes the file and downloads it again (within the
  retry budget). A file only gets its real name once it has been verified, so a
  file with its final name is always complete.
- **Resume state** lives in a single control file per link: `.mgdl-HANDLE` beside
  a downloaded file, or at the top of a downloaded folder. It records each chunk's
  progress and the MAC computation so far. Checkpoints always land on MAC chunk
  boundaries, so resuming never re-reads data from disk; at most the last couple of
  seconds of transfer are fetched again. The control file is deleted once
  everything has finished.
- **Several links download together.** All links are looked up first, then
  their files share one queue and the `-j` file slots, so the next link starts as
  soon as a slot is free instead of waiting for the previous link's slowest file.
  Folders with the same name merge into one; when two links have a file (or a file
  and a folder) with the same name, both are kept and the later link's one gets
  `_HANDLE` added, with a notice. At the end, a report lists how each link went,
  followed by the totals for the whole run.
- **Disk space** is checked before anything starts: if what the selection (across
  all links) still needs (counting only what's left of partial downloads) doesn't fit, mgdl stops
  and says how much is missing. Each file is re-checked as it starts, allowing for
  what the files already downloading still have to write. `.part` files are sparse
  (flagged sparse on NTFS), so they only take up space as data arrives. On
  filesystems without sparse files (exFAT, FAT32), large files are fetched one
  connection at a time instead, so they never need to be allocated in full.
- **Names** that can't be used as-is (Windows-reserved names like `CON`, path
  separators, duplicates within a folder) are changed, and each change is reported
  on stderr, e.g. `renamed   CON.txt → _CON.txt  (CON is a reserved name on Windows)`.
  mgdl never deletes a file it didn't write: if a different file (wrong size) or a
  folder is already where a download should go, it is left alone and the download
  is saved beside it with `_HANDLE` added.
- **Output**: a live dashboard on a terminal; plain timestamped status lines every
  10s when stdout is piped or redirected. The per-link report and the dry run's
  file list fit long names to the terminal width, and never cut them when piped. A finished download prints `done`: green
  if it was verified, yellow if it couldn't be (part of it was downloaded with
  `--no-verify`; run `--verify-existing` later to check it), plain with
  `--no-verify`. With `--verify-existing`, a file already on disk that checks out
  prints `verified`.

## Proxy

`worker/index.js` is the Cloudflare Worker that relays `*.userstorage.mega.co.nz`
requests. Deploy your own and pass it with `-x`.

## Test

```sh
go test -race ./...
```

Hermetic: a fake MEGA API, storage server and proxy run in-process.
