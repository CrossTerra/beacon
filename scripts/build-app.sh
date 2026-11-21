#!/usr/bin/env bash
# Stage an unsigned Beacon.app. Does not sign, install, or launch either executable.
set -euo pipefail
umask 077

if [[ $# -ne 1 || ! "${1:-}" =~ ^[0-9a-f]{40}$ ]]; then
	printf '%s\n' 'Usage: scripts/build-app.sh FULL_AUDITED_COMMIT_ID' >&2
	exit 1
fi
if [[ $(uname -s) != Darwin ]]; then
	printf '%s\n' 'Beacon.app builds on macOS only.' >&2
	exit 1
fi
audited_commit=$1
cd "$(dirname "${BASH_SOURCE[0]}")/.."

verify_checkout() {
	local current changes index
	current=$(git --no-replace-objects rev-parse --verify 'HEAD^{commit}') || return 1
	changes=$(git -c core.fsmonitor=false -c core.untrackedCache=false status \
		--porcelain=v1 --untracked-files=all --ignored --ignore-submodules=none) || return 1
	index=$(git ls-files -v) || return 1
	if [[ "$current" != "$audited_commit" || -n "$changes" ]] || LC_ALL=C grep -Eq '^[Sa-z] ' <<< "$index"; then
		printf '%s\n' 'The checkout must be clean and match the full audited commit. See docs/signing.md.' >&2
		return 1
	fi
}
verify_checkout

stage=$(mktemp -d "${TMPDIR:-/tmp}/beacon-app.XXXXXXXX")
trap 'rm -rf "$stage"' EXIT
bundle="$stage/Beacon.app"
mkdir -p "$stage/src" "$bundle/Contents/"{MacOS,Helpers}
git --no-replace-objects archive "$audited_commit" | tar -x -C "$stage/src"

swift_args=(--package-path "$stage/src/mac" --scratch-path "$stage/swift-build" --configuration release)
swift build "${swift_args[@]}" --product Beacon
swift_bin_dir=$(swift build "${swift_args[@]}" --show-bin-path)
install -m 700 "$swift_bin_dir/Beacon" "$bundle/Contents/MacOS/Beacon"
install -m 600 "$stage/src/mac/Info.plist" "$bundle/Contents/Info.plist"
/usr/bin/plutil -lint "$bundle/Contents/Info.plist"

(
	cd "$stage/src"
	export GOENV=off GOWORK=off GOTOOLCHAIN=local GOFLAGS='' GOAUTH=off CGO_ENABLED=1
	export GOOS=darwin MACOSX_DEPLOYMENT_TARGET=14.0
	GOARCH=$(go env GOHOSTARCH)
	export GOARCH
	go mod verify
	go build -trimpath -buildvcs=false \
		-ldflags "-X main.version=dev -X main.commit=$audited_commit" \
		-o "$bundle/Contents/Helpers/beacon" ./cmd/beacon
)

(
	cd "$stage"
	shasum -a 256 Beacon.app/Contents/{Info.plist,MacOS/Beacon,Helpers/beacon} > Beacon.app.sha256
	shasum -a 256 --check Beacon.app.sha256
)
rm -rf "$stage/src" "$stage/swift-build"
verify_checkout
trap - EXIT
printf 'Commit: %s\nBundle: %s\nSHA-256 manifest: %s\n' "$audited_commit" "$bundle" "$stage/Beacon.app.sha256"
