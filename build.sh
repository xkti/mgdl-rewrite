#!/usr/bin/env bash
# Cross-compile mgdl for Windows, macOS (Apple Silicon and Intel) and Linux.
# Binaries go to dist/, with SHA256SUMS. Usage: ./build.sh
set -euo pipefail
cd "$(dirname "$0")"

# Version from git: the tag if there is one, else the commit; "-dirty" if modified.
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

targets=(
  windows/amd64
  windows/arm64
  darwin/arm64
  darwin/amd64
  linux/amd64
  linux/arm64
)

rm -rf dist
mkdir -p dist

for t in "${targets[@]}"; do
  os=${t%/*} arch=${t#*/}
  out="dist/mgdl-${os}-${arch}"
  [[ $os == windows ]] && out+=.exe
  echo "building ${out}"
  # -trimpath keeps local paths out of the binary; CGO off gives static binaries;
  # -s -w drops debug info to shrink them.
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=${version}" -o "$out" .
done

(cd dist && sha256sum mgdl-* > SHA256SUMS)
echo "done (version ${version}):"
ls -lh dist | tail -n +2
