#!/usr/bin/env bash
# Builds beacon for local use and signs it with a certificate, so the keychain keeps recognising it across rebuilds.
# Same build flags as scripts/repro.sh.
#   scripts/build-local.sh [OUTPUT]     default ~/.local/bin/beacon
#
# The identity is $BEACON_SIGN_IDENTITY if set, else your "Apple Development" identity, else the self-signed
# "Beacon Local Signing" from scripts/dev-signing-setup.sh. Only an Apple-issued certificate keeps keychain access
# across rebuilds: macOS files a build signed any other way under its own hash (the item's partition list), so
# every rebuild asks once per item. See docs/development.md.
#
# A build this script signs can read Beacon's credentials, so it runs only from a terminal: an agent or a script
# running tests must not make one.
set -euo pipefail

if [ ! -t 0 ] || [ ! -t 1 ]; then
	echo "build-local.sh signs a build that can read Beacon's keychain items, so it runs only in a terminal." >&2
	echo "For a test build use go build or go test: an ad-hoc build never touches the keychain." >&2
	exit 1
fi

identifier="dev.beacon.cli"
out=${1:-$HOME/.local/bin/beacon}
sha=$(git rev-parse HEAD)
identities=$(security find-identity -v -p codesigning)

if [ -n "${BEACON_SIGN_IDENTITY:-}" ]; then
	name=$BEACON_SIGN_IDENTITY
elif dev=$(printf '%s\n' "$identities" | sed -n 's/.*"\(Apple Development: [^"]*\)".*/\1/p' | head -n 1) && [ -n "$dev" ]; then
	name=$dev
elif printf '%s\n' "$identities" | grep -q '"Beacon Local Signing"'; then
	name="Beacon Local Signing"
else
	echo "No signing identity. Create an Apple Development certificate in Xcode (Settings, Accounts, Manage" >&2
	echo "Certificates), or run scripts/dev-signing-setup.sh for a self-signed one. See docs/development.md." >&2
	exit 1
fi
# Sign by the certificate's hash: two certificates can share a name after a renewal.
hash=$(printf '%s\n' "$identities" | grep -F "\"$name\"" | head -n 1 | awk '{print $2}')
if [ -z "$hash" ]; then
	echo "No valid code-signing identity named \"$name\"." >&2
	exit 1
fi

mkdir -p "$(dirname "$out")"
go build -trimpath -buildvcs=false -ldflags "-X main.version=${VERSION:-dev} -X main.commit=$sha" -o "$out" ./cmd/beacon
codesign --force --sign "$hash" --identifier "$identifier" "$out"
codesign --verify --strict "$out"
codesign -dr - "$out" 2>&1 | sed -n 's/^designated => /requirement: /p'
echo "signed by: $name"

# securityd's rule for a stable partition (securityd/src/clientid.cpp): a Developer ID, Mac Development or Mac
# Distribution leaf under Apple's developer CAs.
team='anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] and certificate leaf[field.1.2.840.113635.100.6.1.13] or anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.1] and certificate leaf[field.1.2.840.113635.100.6.1.12] or anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.1] and certificate leaf[field.1.2.840.113635.100.6.1.7]'
if codesign --verify -R="$team" "$out" 2>/dev/null; then
	echo "keychain partition: teamid (stable across rebuilds)"
else
	echo "keychain partition: this build's hash. Every rebuild asks once per Beacon item; use an Apple Development" >&2
	echo "certificate to stop that (docs/development.md)." >&2
fi
echo "Built $out at $sha"
