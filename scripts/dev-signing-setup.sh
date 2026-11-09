#!/usr/bin/env bash
# One-time setup on a developer's Mac: a self-signed code-signing identity, so local builds of beacon keep the same
# code signature across rebuilds and the keychain's "Always Allow" survives them.
#
# Why: `go build` signs the binary ad hoc, so every rebuild is a different program to the keychain and every read of
# a Beacon secret asks for the login password again. Release builds are signed with a Developer ID (Phase 7).
#
# This creates "Beacon Local Signing" in the login keychain and trusts it for code signing for this user only.
# macOS asks for your login password twice: to let codesign use the key, and to add the trust setting. The private key never leaves the keychain afterwards.
#   scripts/dev-signing-setup.sh
set -euo pipefail

name="Beacon Local Signing"
keychain="$HOME/Library/Keychains/login.keychain-db"

if security find-identity -v -p codesigning "$keychain" | grep -q "\"$name\""; then
	echo "\"$name\" already exists and is trusted for code signing."
	exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
pass=$(openssl rand -hex 16)

openssl req -x509 -newkey rsa:3072 -nodes -days 3650 \
	-keyout "$tmp/key.pem" -out "$tmp/cert.pem" -subj "/CN=$name" \
	-addext "basicConstraints=critical,CA:false" \
	-addext "keyUsage=critical,digitalSignature" \
	-addext "extendedKeyUsage=critical,codeSigning" 2>/dev/null
# The keychain reads PKCS#12 with the older algorithms. OpenSSL 3 and later need -legacy for them; the LibreSSL that
# ships with macOS uses them by default and rejects the flag. Ask openssl whether it has the flag.
legacy=""
if openssl pkcs12 -help 2>&1 | grep -q -- '-legacy'; then
	legacy="-legacy"
fi
# shellcheck disable=SC2086 # $legacy is empty or one flag
openssl pkcs12 -export $legacy -inkey "$tmp/key.pem" -in "$tmp/cert.pem" -name "$name" \
	-out "$tmp/id.p12" -passout "pass:$pass"

# Only codesign may use the private key: -T adds it to the key's access list, and the partition list must name
# Apple's tools too, or codesign still prompts on every signing. set-key-partition-list asks for the login
# keychain password here, once; it is never a command argument.
security import "$tmp/id.p12" -k "$keychain" -P "$pass" -T /usr/bin/codesign
echo "Enter your login keychain password to let codesign use the key without asking:"
security set-key-partition-list -S apple-tool:,apple: -s -l "$name" "$keychain" >/dev/null
security add-trusted-cert -r trustRoot -p codeSign -k "$keychain" "$tmp/cert.pem"

security find-identity -v -p codesigning "$keychain" | grep "\"$name\""
echo "Done. Build with scripts/build-local.sh."
