#!/usr/bin/env bash
# Runs every gate (AGENTS.md): gofmt, go vet, staticcheck, go test with the race detector, and the reproducible
# build check. CI runs this script.
set -euo pipefail

unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
	echo "gofmt: these files need formatting:" >&2
	echo "$unformatted" >&2
	exit 1
fi
echo "gofmt: ok"
go vet ./...
echo "go vet: ok"
staticcheck ./...
echo "staticcheck: ok"
go test -race -count=1 ./...
scripts/repro.sh
