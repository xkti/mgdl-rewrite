> [!IMPORTANT]
> This is an entirely vibecoded rewrite and overhaul of my original project, [mg-download.sh](https://github.com/xkti/mg-download), with far more features and (perceived) stability. However, as with anything vibecoded, it may contain bugs. Please report any issues you come across! No warranty or guarantee is provided, use at your own risk!

## mgdl-rewrite - a MEGA.nz downloader with quota bypass written in Go and JavaScript

#### Install

Download from [releases](../../releases) for your operating system, or build using `go build -trimpath -o mgdl .`

`./build.sh` can be used on Linux for cross-compiling Windows/Linux/Mac builds.

#### Main features

- On-the-fly decryption and verification
- Transfer quota bypass (see Proxy section)
- Faster downloads with concurrent chunked downloading
- Download speed limit with `--limit-rate`
- Interactive interface to selectively download files and folders
- Full download resume support (just run the command again)
- Multiple links, subfolder and file-in-folder links, old link format support

There are various behaviours that need more explanation, see Behaviour section.

#### Examples

```
# Run `mgdl --help` to view all options and features.

# Download a file
mgdl https://mega.nz/file/...

# Download a file and folder with eight connections per file and 512M chunks
mgdl -c 8 -C 512M https://mega.nz/file/... https://mega.nz/folder/...

# Select files and folders to download from a list of links with custom download directory
mgdl -s -o custom_dir -i links.txt

# Download specific subfolder and file from the same shared link
mgdl https://mega.nz/folder/.../folder/... https://mega.nz/folder/.../file/...

# Specify relative path to download in folder link, adding -s will preselect if path is found
mgdl -p "Season 01" https://mega.nz/folder/...
```

#### Behaviour

- Downloads are split into chunks and downloaded in parallel (`-C, --chunk-size`, default 256M, minimum 128M; `-c, --connections`, default 4) to improve download speeds. This behaviour can be disabled partially with `--sequential`, which will make mgdl download one chunk at a time. However, a parallel download can not be resumed with the `--sequential` flag, but a sequential download can be upgraded to a parallel download at any time. (Exception being sparse files not being supported on the filesystem.)
- mgdl will check if your download directory (`-o, --output`) has enough free space to download your links. You can disable this for whatever reason with `--no-space-check`. I do *not* recommend using that.
- When multiple links are given, mgdl will fetch the info for all of them and merge folders that have the same name. Using `-l, --list` will print each link's structure individually.
- If a folder contains duplicate folder/file names (case-insensitive), mgdl will download the first item in sort order to its original name and duplicates will use their MEGA handle as a suffix. (e.g. 1st `file.txt` -> as is, 2nd `file.txt` -> `file_HANDLE.txt`, ...) If multiple links have items with the same name, the latter links will have the same behaviour. When `-s, --select` is used, the latter links' same-name items will be highlighted in yellow.
- `-p, --path` can be used multiple times, and will look in *all* links given. **When file links are given, they will be skipped entirely.**
- If a subfolder or a `folder/.../file/...` link is used, mgdl will retain the full directory structure to said folder/file. Using `-s, --select` will have those folders/files preselected.
- Incomplete/partial downloads will create a `.mgdl-HANDLE` file either in the base download directory or in the root directory of a folder link, containing download/verification progress and metadata. It gets deleted once your download completes.
- **The `--verify-existing` flag can be destructive!** If mgdl sees that a file is complete *and* has the expected file size but is corrupted (MAC mismatch), it will automatically delete it and re-download it. If you need to use this flag, it's recommended to use `-o, --output` to isolate your download from other files.

#### Proxy

Same as mg-download, mgdl uses chunked downloads alongside a Cloudflare Workers proxy to get around MEGA's stingy bandwidth limits (~5GiB every 8 hours?). The proxy code can be found in [/worker](/worker). You *can* bypass the proxy with `--direct`, but expect failures after ~5GiB transferred.

By default, mgdl currently has five proxies available, randomly selecting one per chunk. It's recommended to deploy and specify your own using `-x, --proxy`.

Deploying and using your own proxy is fairly simple and entirely free.

1. [Create](https://dash.cloudflare.com/sign-up) a Cloudflare account and verify your email.
2. On the left side of the [dashboard](https://dash.cloudflare.com), go to Build > Compute > Workers & Pages.
3. Click on **Create application**, and choose **Start with Hello World!**.
4. Feel free to change the worker name. Click **Deploy**.
5. On the top right, click **Edit code**. Delete everything in the editor on the left and paste the contents of [index.js](worker/index.js) into the editor. Click **Deploy**.
6. Copy the worker URL seen under the **Deploy** button. Use it with `-x, --proxy`.

You're done! A free Cloudflare account has 100k requests daily, so if you keep chunk sizes to 256M, you should be able to theoretically download a max of ~24TB daily, less depending on file count and size. You can repeat this on a new account for more requests (call `-x, --proxy` again or add a comma between proxies), or you can pay for Workers Paid, which gives you *10 million* requests per month (no daily limit).

If you already use Cloudflare Workers, you can deploy this with wrangler.
