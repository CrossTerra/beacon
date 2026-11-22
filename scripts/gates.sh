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
# Use macOS's system Bash 3.2 even if PATH has a newer bash.
for script in scripts/install.sh scripts/install-lib.sh scripts/test-install{,-lib}.sh; do
	/bin/bash -n "$script"
done
bash scripts/test-checkout.sh
if [[ $(uname -s) == Darwin ]]; then
	/bin/bash scripts/test-install-lib.sh
fi
if command -v shellcheck >/dev/null; then
	shellcheck scripts/*.sh
	echo "shellcheck: ok"
else
	echo "shellcheck: not installed here; CI runs it on Linux"
fi
scripts/repro.sh
