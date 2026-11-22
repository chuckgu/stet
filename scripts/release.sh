#!/usr/bin/env bash
# Build a versioned bundle of stet for this machine.
#
#   scripts/release.sh <version>      # e.g. 0.2.0 (no leading "v")
#
# Writes dist/stet_<version>_<os>_<arch>.tar.gz (one directory holding stet,
# LICENSE, README.md and stet.example.yaml) and updates dist/checksums.txt.
# Needs cgo: the SQL parser is C.
set -euo pipefail

version=${1:-}
if [[ ! $version =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$ ]]; then
  echo "usage: scripts/release.sh <version>   (like 0.2.0, without a leading v)" >&2
  exit 2
fi

cd "$(dirname "$0")/.."

goos=$(go env GOOS)
goarch=$(go env GOARCH)
commit=$(git rev-parse --short=12 HEAD)
name=stet_${version}_${goos}_${goarch}
archive=$name.tar.gz

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/$name" dist

CGO_ENABLED=1 go build -trimpath \
  -ldflags "-s -w -X main.version=$version -X main.commit=$commit" \
  -o "$stage/$name/stet" ./cmd/stet
cp LICENSE README.md stet.example.yaml "$stage/$name/"

tar -C "$stage" -czf "dist/$archive" "$name"

if command -v shasum >/dev/null 2>&1; then
  sum=$(shasum -a 256 "dist/$archive" | cut -d' ' -f1)
else
  sum=$(sha256sum "dist/$archive" | cut -d' ' -f1)
fi

# Replace this file's line, keep the others.
touch dist/checksums.txt
{ grep -v -F "  $archive" dist/checksums.txt || true; echo "$sum  $archive"; } > dist/checksums.txt.new
mv dist/checksums.txt.new dist/checksums.txt

echo "dist/$archive"
