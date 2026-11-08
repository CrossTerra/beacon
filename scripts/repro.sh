#!/usr/bin/env bash
# Checks that the beacon binary is reproducible: builds a commit twice, from two copies of the tree at different
# paths, and requires identical bytes.
#
#   scripts/repro.sh [REF]     default HEAD; uncommitted changes are not part of the build
set -euo pipefail

ref=${1:-HEAD}
sha=$(git rev-parse --verify "$ref^{commit}")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

build() {
	local dir=$tmp/$1/src
	mkdir -p "$dir"
	git archive "$sha" | tar -x -C "$dir"
	(cd "$dir" && go build -trimpath -buildvcs=false \
		-ldflags "-X main.version=${VERSION:-dev} -X main.commit=$sha" \
		-o "$tmp/$1/beacon" ./cmd/beacon)
}

build first
build second-copy
a=$(shasum -a 256 "$tmp/first/beacon" | cut -d' ' -f1)
b=$(shasum -a 256 "$tmp/second-copy/beacon" | cut -d' ' -f1)
echo "$a  first build of ${sha:0:12} ($(go env GOOS)/$(go env GOARCH), CGO_ENABLED=$(go env CGO_ENABLED))"
echo "$b  second build"
if [ "$a" != "$b" ]; then
	echo "not reproducible" >&2
	exit 1
fi
echo "reproducible"
