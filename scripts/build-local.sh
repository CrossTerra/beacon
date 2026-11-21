#!/usr/bin/env bash
# Person-run local build and signing. See docs/signing.md.
set -euo pipefail
umask 077

if [ ! -t 0 ] || [ ! -t 1 ]; then
	echo "build-local.sh signs a build that can read Beacon's keychain items, so it runs only in a terminal." >&2
	echo "For a test build use go build or go test: an ad-hoc build never touches the keychain." >&2
	exit 1
fi

if [[ $# -lt 3 || $# -gt 4 ]]; then
	echo "Usage: scripts/build-local.sh AUDITED_COMMIT SIGNING_KEYCHAIN CERTIFICATE_SHA1 [OUTPUT]" >&2
	echo "See docs/signing.md." >&2
	exit 1
fi
audited_commit=$1
signing_keychain=$2
signing_cert=$3
out=${4:-$HOME/.local/bin/beacon}
[[ "$out" == /* ]] || out="$PWD/$out"
if [[ "$signing_keychain" != /* || ! -f "$signing_keychain" ||
	"$signing_keychain" -ef "$HOME/Library/Keychains/login.keychain-db" ||
	"$signing_keychain" -ef "$HOME/Library/Keychains/login.keychain" ||
	! "$signing_cert" =~ ^[[:xdigit:]]{40}$ ]]; then
	echo "Supply the absolute path to the dedicated signing keychain and the certificate's SHA-1. See docs/signing.md." >&2
	exit 1
fi

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/audited-build.sh
source "$script_dir/audited-build.sh"
cd "$script_dir/.."
verify_audited_checkout "$audited_commit"
/usr/bin/security lock-keychain "$signing_keychain"

stage=$(mktemp -d "$HOME/.beacon-build.XXXXXXXX")
candidate=
cleanup() {
	rm -rf "$stage"
	if [[ -n "$candidate" ]]; then
		rm -f "$candidate"
	fi
}
trap cleanup EXIT
stage_audited_build "$audited_commit" "$stage"
unsigned_sha=$(artifact_sha256 "$stage/beacon")
verify_audited_checkout "$audited_commit"
verify_artifact "$stage/beacon" "$unsigned_sha"
printf 'Audited commit %s\nUnsigned SHA-256 %s\n' "$audited_commit" "$unsigned_sha"

identifier="dev.beacon.cli"
requirement="identifier \"$identifier\" and certificate leaf = H\"$signing_cert\""
(
	trap '/usr/bin/security lock-keychain "$signing_keychain" || exit 1' EXIT
	trap 'exit 129' HUP
	trap 'exit 130' INT
	trap 'exit 143' TERM
	/usr/bin/security unlock-keychain "$signing_keychain"
	verify_audited_checkout "$audited_commit"
	verify_artifact "$stage/beacon" "$unsigned_sha"
	/usr/bin/codesign --force --sign "$signing_cert" --keychain "$signing_keychain" --timestamp=none \
		--options runtime --identifier "$identifier" --requirements "=designated => $requirement" "$stage/beacon"
)

/usr/bin/codesign --verify --strict -R "=$requirement" "$stage/beacon"
signed_sha=$(artifact_sha256 "$stage/beacon")
if [[ -d "$out" ]]; then
	echo "Output must name a file." >&2
	exit 1
fi
output_dir=$(dirname "$out")
mkdir -p "$output_dir"
candidate=$(mktemp "$output_dir/.beacon-signed.XXXXXXXX")
cp -p "$stage/beacon" "$candidate"
verify_artifact "$candidate" "$signed_sha"
/usr/bin/codesign --verify --strict -R "=$requirement" "$candidate"
mv -f "$candidate" "$out"
candidate=
verify_artifact "$out" "$signed_sha"
/usr/bin/codesign --verify --strict -R "=$requirement" "$out"
/usr/bin/codesign -d -r- "$out"

# securityd's rule for a stable partition (securityd/src/clientid.cpp): a Developer ID, Mac Development or Mac
# Distribution leaf under Apple's developer CAs.
team='anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] and certificate leaf[field.1.2.840.113635.100.6.1.13] or anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.1] and certificate leaf[field.1.2.840.113635.100.6.1.12] or anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.1] and certificate leaf[field.1.2.840.113635.100.6.1.7]'
if /usr/bin/codesign --verify -R="$team" "$out" 2>/dev/null; then
	echo "keychain partition: teamid (stable across rebuilds)"
else
	echo "keychain partition: this build's hash. Every rebuild asks once per Beacon item; use an Apple Development" >&2
	echo "certificate for a stable partition (docs/signing.md)." >&2
fi
printf 'Built %s at %s\nSigned SHA-256 %s\n' "$out" "$audited_commit" "$signed_sha"
