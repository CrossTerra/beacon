#!/usr/bin/env bash
# Runs the tether fixtures as the current user. Installs nothing and never touches a Keychain.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/beacon-tether-test.XXXXXXXX")
trap 'rm -rf "$tmp"' EXIT
xcrun --sdk macosx clang -std=c17 -Wall -Wextra -Werror -mmacosx-version-min=14.0 \
	-I mac/Sources/TetherSupport/include -I mac/Sources/tether \
	mac/Sources/TetherSupport/launch.c mac/Sources/tether/{supervise,procs}.c \
	mac/Tests/TetherFixture/fixture.c -o "$tmp/fixture"
GOENV=off GOWORK=off GOAUTH=off GOTOOLCHAIN=local GOFLAGS='' CGO_ENABLED=1 \
	go build -o "$tmp/collector" ./cmd/beacon
TMPDIR="$tmp" PYTHONDONTWRITEBYTECODE=1 python3 -B mac/Tests/TetherFixture/test_tether.py "$tmp/fixture" "$tmp/collector"
