#!/usr/bin/env bash
# Builds beacon for local use and signs it with the stable local identity from scripts/dev-signing-setup.sh, so the
# keychain keeps recognising it across rebuilds. Same build flags as scripts/repro.sh.
#   scripts/build-local.sh [OUTPUT]     default ~/.local/bin/beacon
set -euo pipefail

name="Beacon Local Signing"
identifier="dev.beacon.cli"
out=${1:-$HOME/.local/bin/beacon}
sha=$(git rev-parse HEAD)

if ! security find-identity -v -p codesigning | grep -q "\"$name\""; then
	echo "No \"$name\" identity. Run scripts/dev-signing-setup.sh once." >&2
	exit 1
fi

mkdir -p "$(dirname "$out")"
go build -trimpath -buildvcs=false -ldflags "-X main.version=${VERSION:-dev} -X main.commit=$sha" -o "$out" ./cmd/beacon
codesign --force --sign "$name" --identifier "$identifier" "$out"
codesign --verify --strict "$out"
codesign -dr - "$out" 2>&1 | sed -n 's/^designated => /requirement: /p'
echo "Built $out at $sha"
