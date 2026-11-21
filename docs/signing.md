# Local build and signing on macOS

The person builds and signs the exact commit named in the [audit report](audit.md). Keep the signing key
locked while reviewing, building and staging. Verify the full commit ID and the staged artifact before
unlocking the signer (13.00-D15). Sign locally, relock immediately and verify the signature at the path
you will run. Agents use ad-hoc builds and fixtures, and never run anything that uses the login keychain.

## Choose a local signing identity

- **Apple Development.** With a free Apple ID (Apple Account), sign in to Xcode under **Settings → Accounts**.
  Select your Personal Team, then **Manage Certificates → + → Apple Development**. Record the certificate
  fingerprint and Team ID. Use the same certificate until you have reviewed and tested a change of identity.
  [Apple on Personal Teams](https://developer.apple.com/help/account/basics/about-your-developer-account) ·
  [Managing signing certificates in Xcode](https://developer.apple.com/documentation/xcode/sharing-your-teams-signing-certificates)
- **Self-signed.** In Keychain Access, choose **Certificate Assistant → Create a Certificate**, with identity
  type **Self Signed Root** and certificate type **Code Signing**. Put it in your dedicated signing keychain
  and scope any trust setting to code signing for your account. The repo's
  [developer helper](../scripts/dev-signing-setup.sh) creates an identity in the login keychain. Use Keychain
  Access to choose the dedicated location for this workflow. A self-signed build is useful for local
  development. Expect Keychain access prompts when a rebuild changes its code hash.
  [Apple on self-signed identities and designated requirements](https://developer.apple.com/library/archive/technotes/tn2206/)

Use a dedicated, lockable signing keychain whose private key you control. In Keychain Access choose
**File → New Keychain** to create it. If Xcode placed the signing identity in the login keychain, transfer the
certificate **and private key** through Keychain Access into the dedicated keychain. Verify the transfer, then
remove any unlocked copy of that dedicated identity and any temporary export. Keep the signer out of the
agent's reach. Run `security find-identity -v -p codesigning "$signing_keychain"` yourself to record the exact leaf
hash listed for the identity. This is a local development signature. Distribution and notarization have their
own signing requirements.

## What the Keychain checks on a rebuild

The existing CLI uses the file-based Keychain. On creation, it uses `SecTrustedApplicationCreateFromPath` and
`SecAccessCreate` to give the item's access control list (ACL) the creating program's designated requirement.
The CLI identifier is `dev.beacon.cli`. The requirement identifies the code that is trusted for that item.
The keychain package refuses ad-hoc binaries before it opens the store, except the opt-in test override.
See [the ACL and certificate check](../internal/keychain/darwin.go) and
[Apple on the file-based Keychain](https://developer.apple.com/documentation/technotes/tn3137-on-mac-keychains).

The partition list is a second check. With a qualifying Apple Development certificate the partition is
`teamid:<team>`. A rebuilt binary can keep the same designated requirement and team partition while its code
hash changes. With a self-signed certificate the partition is `cdhash:<build hash>`. A rebuild can meet the
same requirement and still prompt because its partition changed. **Always Allow** can add access to an item.
Leave existing ACLs and partitions intact during the experiment. A certificate change also needs an explicit
review of the existing CLI item's ACLs and the LaunchAgent's pinned leaf if you use it. See
[the signing helper's partition check](../scripts/build-local.sh) and
[`beacon keychain repair`](commands.md#beacon-keychain-repair).

## Stage and sign the audited collector

Run this yourself in Bash from a fresh detached checkout of the approved fork commit. Substitute the full
approved ID, the dedicated keychain's absolute path and the exact identity hash you recorded. Use the audited
Go and macOS toolchain and its reviewed compiler settings. Keep this checkout and the staging directory
outside the supervised accounts' write access. The signing helper in this tree chooses an identity and builds
from the working directory. It does not enforce the audited-commit check.

~~~bash
(
  set -euo pipefail
  umask 077
  audited_commit='FULL_COMMIT_FROM_THE_APPROVED_REPORT'
  signing_keychain='/absolute/path/to/your-signing.keychain-db'
  signing_cert='EXACT_CERTIFICATE_SHA1'
  cli_requirement="identifier \"dev.beacon.cli\" and certificate leaf = H\"$signing_cert\""
  verify_checkout() {
    local current changes
    current=$(git rev-parse --verify 'HEAD^{commit}') || return 1
    changes=$(git status --porcelain=v1 --untracked-files=all --ignored) || return 1
    if [ "$current" != "$audited_commit" ] || [ -n "$changes" ]; then
      printf '%s\n' 'Stop: the checkout must be clean and match the full audited commit.' >&2
      return 1
    fi
  }
  verify_checkout
  security lock-keychain "$signing_keychain"
  export GOENV=off GOWORK=off GOTOOLCHAIN=local GOFLAGS= CGO_ENABLED=1
  stage=$(mktemp -d "$HOME/.beacon-build.XXXXXXXX")
  mkdir "$stage/src"
  git archive "$audited_commit" | tar -x -C "$stage/src"
  (
    cd "$stage/src"
    go mod verify
    go build -trimpath -buildvcs=false \
      -ldflags "-X main.version=dev -X main.commit=$audited_commit" \
      -o "$stage/beacon" ./cmd/beacon
  )
  shasum -a 256 "$stage/beacon" > "$stage/unsigned.sha256"
  verify_checkout
  shasum -a 256 --check "$stage/unsigned.sha256"
  printf 'Verified %s\nStaged artifact %s\n' "$audited_commit" "$stage/beacon"
  (
    trap 'security lock-keychain "$signing_keychain"' EXIT
    set -e
    security unlock-keychain "$signing_keychain"
    codesign --force --sign "$signing_cert" --keychain "$signing_keychain" --timestamp=none \
      --options runtime --identifier dev.beacon.cli --requirements "=designated => $cli_requirement" \
      "$stage/beacon"
  )
  codesign --verify --strict -R "=$cli_requirement" "$stage/beacon"
  codesign -d -r- "$stage/beacon"
  "$stage/beacon" version
  shasum -a 256 "$stage/beacon"
)
~~~

Check the printed commit against the report and record the signed hash and designated requirement. When
placing the binary at its approved location, verify that same hash and signature there before using it.
A different commit, changed build input, or replaced artifact needs review with the signer locked. Run
[`scripts/repro.sh`](../scripts/repro.sh) during the audit for the two-path build check. The staged binary is
the input to signing. The check concerns those pre-signing bytes.

## Part 1 manual rebuild test

Winton runs this in his own Terminal on his Mac before real credentials are provisioned. It makes a minimal
`.app` and two disposable generic passwords in a separate file-based test keychain. The items stand in for the
vault key and journal anchor. The probe uses the same creator ACL mechanism as the collector. It has no
network calls, uses its own service name, and never opens existing Beacon items. Part 2 must repeat this
test with the finished bundle and its actual Keychain configuration.

1. Record the full approved commit, macOS version and positive certificate hash (try Apple Development first).
   Obtain a second distinct certificate for the wrong-signer control in the dedicated signing keychain. An
   existing second development certificate or a disposable self-signed code-signing certificate works. In
   a fresh Bash shell, from the fork, fill in the four values below. This reads the probe from the audited
   document and makes two independent bundles with different executable bytes, plus two copies for the
   controls. The signer stays locked through the builds.

~~~bash
set -euo pipefail
umask 077
audited_commit='FULL_COMMIT_FROM_THE_APPROVED_REPORT'
signing_keychain='/absolute/path/to/your-signing.keychain-db'
signing_cert='EXACT_POSITIVE_CERTIFICATE_SHA1'
other_cert='EXACT_DIFFERENT_CERTIFICATE_SHA1'
probe_dir=$(mktemp -d "$HOME/.beacon-keychain-test.XXXXXXXX")
items_keychain="$probe_dir/items.keychain-db"

verify_probe_checkout() {
  local current changes
  current=$(git rev-parse --verify 'HEAD^{commit}') || return 1
  changes=$(git status --porcelain=v1 --untracked-files=all --ignored) || return 1
  [ "$current" = "$audited_commit" ] && [ -z "$changes" ]
}
verify_probe_checkout
[ "$signing_cert" != "$other_cert" ]
security lock-keychain "$signing_keychain"
git show "$audited_commit:docs/signing.md" |
  awk '$0 == "~~~swift" { copy=1; next } copy && $0 == "~~~" { copy=0; next } copy' > "$probe_dir/Probe.swift"
[ -s "$probe_dir/Probe.swift" ]
build_probe() {
  local bundle="$probe_dir/$1/BeaconProbe.app"
  shift
  mkdir -p "$bundle/Contents/MacOS"
  cat > "$bundle/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>dev.beacon.signing-probe</string>
  <key>CFBundleExecutable</key><string>Probe</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleVersion</key><string>1</string>
</dict>
</plist>
PLIST
  xcrun swiftc -suppress-warnings "$@" \
    "$probe_dir/Probe.swift" -o "$bundle/Contents/MacOS/Probe"
}
build_probe A
build_probe B -D REBUILD
cp -R "$probe_dir/B" "$probe_dir/other"
cp -R "$probe_dir/B" "$probe_dir/adhoc"
shasum -a 256 "$probe_dir/Probe.swift" \
  "$probe_dir/"{A,B,other,adhoc}/BeaconProbe.app/Contents/{Info.plist,MacOS/Probe} > "$probe_dir/staged.sha256"
~~~

2. Verify the checkout and staged hashes again, then unlock the signer for these bundles. The positive
   requirement pins the bundle identifier and certificate leaf. The other copy keeps that identifier and
   uses the second leaf. Relock the signer, verify both positive signatures and confirm that A and B
   have different `CDHash` values. Both controls must have valid signatures of their stated type and fail the
   positive requirement.

~~~bash
verify_probe_checkout
shasum -a 256 --check "$probe_dir/staged.sha256"
printf 'Verified probe at %s from %s\n' "$probe_dir" "$audited_commit"
probe_requirement() {
  printf 'identifier "dev.beacon.signing-probe" and certificate leaf = H"%s"' "$1"
}
positive=$(probe_requirement "$signing_cert")
(
  trap 'security lock-keychain "$signing_keychain"' EXIT
  set -e
  security unlock-keychain "$signing_keychain"
  for build in A B other; do
    cert="$signing_cert"
    if [ "$build" = other ]; then cert="$other_cert"; fi
    codesign --force --sign "$cert" --keychain "$signing_keychain" --timestamp=none --options runtime \
      --requirements "=designated => $(probe_requirement "$cert")" "$probe_dir/$build/BeaconProbe.app"
  done
)
codesign --force --sign - --options runtime "$probe_dir/adhoc/BeaconProbe.app"
for build in A B; do
  codesign --verify --strict -R "=$positive" "$probe_dir/$build/BeaconProbe.app"
  codesign -d -r- "$probe_dir/$build/BeaconProbe.app"
  codesign -dvvv "$probe_dir/$build/BeaconProbe.app" 2>&1 |
    grep '^CDHash=' | tee "$probe_dir/$build.cdhash"
done
if cmp -s "$probe_dir/A.cdhash" "$probe_dir/B.cdhash"; then
  printf '%s\n' 'Stop: the rebuild has the same CDHash.' >&2
  exit 1
fi
codesign --verify --strict -R "=$(probe_requirement "$other_cert")" "$probe_dir/other/BeaconProbe.app"
codesign --verify --strict "$probe_dir/adhoc/BeaconProbe.app"
for build in other adhoc; do
  if codesign --verify --strict -R "=$positive" "$probe_dir/$build/BeaconProbe.app"; then
    printf 'Stop: %s satisfied the positive requirement\n' "$build" >&2
    exit 1
  fi
done
~~~

3. Create and unlock only the disposable items keychain. Enter a throwaway password when prompted, off the
   command line. Run A to seed both items and verify their values. Run B to read the vault key, read the
   original anchor and advance it. Record prompt counts for both builds and both items. For the positive
   rebuild, access must succeed without any prompt or ACL change. If B asks for access, click **Deny**
   and record a failed trial.

~~~bash
security create-keychain "$items_keychain"
security unlock-keychain "$items_keychain"
"$probe_dir/A/BeaconProbe.app/Contents/MacOS/Probe" seed "$items_keychain"
"$probe_dir/B/BeaconProbe.app/Contents/MacOS/Probe" advance "$items_keychain"
~~~

4. With that same unlocked items keychain, run the two controls. Click **Deny** on any access prompt. The
   controls must fail to read either item or update the anchor. B must then read the original vault key and
   the anchor it wrote. An unrelated error such as a missing item does not count as a denial.

~~~bash
"$probe_dir/other/BeaconProbe.app/Contents/MacOS/Probe" denied "$items_keychain"
"$probe_dir/adhoc/BeaconProbe.app/Contents/MacOS/Probe" denied "$items_keychain"
"$probe_dir/B/BeaconProbe.app/Contents/MacOS/Probe" verify "$items_keychain"
~~~

5. Attach the commit, macOS version, signing certificate fingerprint and Team ID, designated requirement,
   both CDHashes, file-based Keychain choice, prompt counts and each pass or denial to the audit record.
   If testing self-signed as the positive identity, start with a new disposable keychain and record whether
   changing the build hash prompts again. Clear Part 1 only for a signing and Keychain configuration that
   passed both the rebuild and denial checks. Delete the disposable keychain with
   `security delete-keychain "$items_keychain"` and remove the private probe directory when finished.

### Probe source

~~~swift
import Foundation
import Security

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data("\(message)\n".utf8))
    exit(1)
}

func require(_ status: OSStatus, _ operation: String) {
    guard status == errSecSuccess else {
        fail("\(operation) failed (OSStatus \(status))")
    }
}

let arguments = CommandLine.arguments
guard arguments.count == 3 else {
    fail("Usage: Probe seed|advance|denied|verify /absolute/path/to/items.keychain-db")
}
let mode = arguments[1]
var opened: SecKeychain?
require(SecKeychainOpen(arguments[2], &opened), "open disposable keychain")
guard let keychain = opened else { fail("No disposable keychain") }

let vault = "vault-key"
let anchor = "journal-anchor"
let vaultFixture = "disposable vault bytes"
let initialAnchor = "sequence 1, digest fixture A"
let updatedAnchor = "sequence 2, digest fixture B"

func attributes(_ account: String) -> [String: Any] {
    [
        kSecClass as String: kSecClassGenericPassword,
        kSecAttrService as String: "dev.beacon.signing-probe",
        kSecAttrAccount as String: account,
    ]
}

func query(_ account: String) -> [String: Any] {
    var search = attributes(account)
    search[kSecMatchSearchList as String] = [keychain]
    return search
}

func add(_ account: String, _ fixture: String, _ access: SecAccess) {
    var item = attributes(account)
    item[kSecUseKeychain as String] = keychain
    item[kSecAttrAccess as String] = access
    item[kSecValueData as String] = Data(fixture.utf8)
    require(SecItemAdd(item as CFDictionary, nil), "add \(account)")
}

func read(_ account: String, _ result: inout CFTypeRef?) -> OSStatus {
    var search = query(account)
    search[kSecMatchLimit as String] = kSecMatchLimitOne
    search[kSecReturnData as String] = true
    return SecItemCopyMatching(search as CFDictionary, &result)
}

func expect(_ account: String, _ fixture: String) {
    var result: CFTypeRef?
    require(read(account, &result), "read \(account)")
    guard result as? Data == Data(fixture.utf8) else {
        fail("Unexpected contents of \(account)")
    }
}

func updateAnchor(_ fixture: String) -> OSStatus {
    let update = [kSecValueData as String: Data(fixture.utf8)]
    return SecItemUpdate(query(anchor) as CFDictionary, update as CFDictionary)
}

func expectDenied(_ status: OSStatus, _ operation: String) {
    guard [errSecAuthFailed, errSecInteractionNotAllowed, errSecUserCanceled].contains(status) else {
        fail("\(operation) did not return access denial (OSStatus \(status))")
    }
    print("\(operation) denied (OSStatus \(status))")
}

switch mode {
case "seed":
    var creator: SecTrustedApplication?
    require(SecTrustedApplicationCreateFromPath(nil, &creator), "identify creating app")
    guard let creator else { fail("No creating app") }
    var access: SecAccess?
    require(SecAccessCreate("Beacon signing probe" as CFString, [creator] as CFArray, &access), "create ACL")
    guard let access else { fail("No creator ACL") }
    add(vault, vaultFixture, access)
    add(anchor, initialAnchor, access)
    expect(vault, vaultFixture)
    expect(anchor, initialAnchor)
case "advance":
    expect(vault, vaultFixture)
    expect(anchor, initialAnchor)
    require(updateAnchor(updatedAnchor), "update anchor")
    expect(anchor, updatedAnchor)
case "denied":
    var result: CFTypeRef?
    expectDenied(read(vault, &result), "vault read")
    expectDenied(read(anchor, &result), "anchor read")
    expectDenied(updateAnchor("wrong signer"), "anchor update")
case "verify":
    expect(vault, vaultFixture)
    expect(anchor, updatedAnchor)
default:
    fail("Unknown probe mode")
}

#if REBUILD
let generation = "B"
#else
let generation = "A"
#endif
print("Probe build \(generation): \(mode) passed")
~~~
