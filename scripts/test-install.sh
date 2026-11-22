#!/usr/bin/env bash
# Exercise scripts/install.sh --dry-run: build, ad-hoc sign and install into a temporary prefix, twice, and check the
# result. Uses no signing identity and no keychain, and must leave $HOME's Beacon untouched. Needs a checkout with no
# changes to tracked files; an untracked and an ignored file are added for the run, as a person's checkout may have.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
tmp=${TMPDIR:-/tmp}
# The physical path, as install.sh resolves it: GitHub's runners set TMPDIR with a trailing slash, and /var is a
# symlink to /private/var.
prefix=$(cd "$(mktemp -d "${tmp%/}/beacon-install-test.XXXXXXXX")" && pwd -P)
# An untracked file and an ignored one (.e2e.env, only if there is none already) must not stop the install.
untracked=".test-install-untracked.$$"
touch "$untracked"
made_ignored=false
if [[ ! -e .e2e.env ]]; then
	printf 'FIXTURE=1\n' > .e2e.env
	made_ignored=true
fi
cleanup() {
	rm -rf "$prefix"
	rm -f "$untracked"
	if [[ "$made_ignored" == true ]]; then rm -f .e2e.env; fi
}
trap cleanup EXIT

fingerprint() {
	local path=$1
	if [[ -L "$path" ]]; then printf 'link:%s' "$(readlink "$path")"
	elif [[ -e "$path" ]]; then stat -f 'inode:%i:%m' "$path"
	else printf 'absent'
	fi
}
before=$(fingerprint "$HOME/Applications/Beacon.app")$(fingerprint "$HOME/.local/bin/beacon")

fail() {
	echo "test-install: $*" >&2
	exit 1
}

mkdir -p "$prefix/.local/bin"
printf 'old cli\n' > "$prefix/.local/bin/beacon"
scripts/install.sh --dry-run --prefix "$prefix" > "$prefix/first.log"
app="$prefix/Applications/Beacon.app"
for file in Contents/Info.plist Contents/MacOS/Beacon Contents/Helpers/beacon Contents/Helpers/tether; do
	[[ -f "$app/$file" ]] || fail "missing $file"
done
for path in Contents/MacOS/Beacon Contents/Helpers/beacon Contents/Helpers/tether; do
	[[ -x "$app/$path" ]] || fail "$path is not executable"
done
identifier() { /usr/bin/codesign -d --verbose=1 "$1" 2>&1 | sed -n 's/^Identifier=//p'; }
[[ $(identifier "$app") == dev.beacon.app ]] || fail "app identifier"
[[ $(identifier "$app/Contents/Helpers/beacon") == dev.beacon.cli ]] || fail "CLI identifier"
[[ $(identifier "$app/Contents/Helpers/tether") == dev.beacon.tether ]] || fail "tether identifier"
/usr/bin/codesign --verify --strict --deep "$app" || fail "the installed app does not verify"
[[ $(readlink "$prefix/.local/bin/beacon") == "$app/Contents/Helpers/beacon" ]] || fail "beacon is not linked to the app's CLI"
[[ $(cat "$prefix/.local/bin/beacon.pre-app") == "old cli" ]] || fail "the old CLI was not kept aside"
"$prefix/.local/bin/beacon" version > /dev/null || fail "the linked CLI does not run"
grep -q 'Dry run' "$prefix/first.log" || fail "no dry-run summary"
if grep -Eqi 'untracked|ignored' "$prefix/first.log"; then fail "install.sh mentioned untracked or ignored files"; fi
first=$(stat -f %i "$app")

# Again: idempotent, and the new app replaces the old one in place.
scripts/install.sh --dry-run --prefix "$prefix" > "$prefix/second.log"
[[ $(stat -f %i "$app") != "$first" ]] || fail "the second install did not replace the app"
/usr/bin/codesign --verify --strict --deep "$app" || fail "the replaced app does not verify"
[[ $(readlink "$prefix/.local/bin/beacon") == "$app/Contents/Helpers/beacon" ]] || fail "the link changed"
leftovers=$(find "$prefix/Applications" "$prefix/.local/bin" -name '.*.new.*' -o -name '.*.old.*')
[[ -z "$leftovers" ]] || fail "left behind: $leftovers"

after=$(fingerprint "$HOME/Applications/Beacon.app")$(fingerprint "$HOME/.local/bin/beacon")
[[ "$before" == "$after" ]] || fail "the dry run changed \$HOME"

# Neither file reached the build.
for name in "$untracked" .e2e.env; do
	[[ -z $(find "$app" -name "$name") ]] || fail "$name reached the app"
done

# A real install refuses to run without a terminal, and --prefix without --dry-run.
if scripts/install.sh < /dev/null > /dev/null 2>&1; then fail "a real install ran without a terminal"; fi
if scripts/install.sh --prefix "$prefix" > /dev/null 2>&1; then fail "--prefix was accepted for a real install"; fi
echo "install.sh dry run: built, ad-hoc signed, installed and replaced under a temporary prefix"
