#!/usr/bin/env bash
# Install Beacon from this checkout, in one command (plan 13.00-D15): build Beacon.app, sign it with your code-signing
# identity, install it in ~/Applications, point ~/.local/bin/beacon at its CLI, retire the old collector LaunchAgent
# and open the app. Run it yourself, in Terminal; it can run again any time. See docs/signing.md.
#
#   scripts/install.sh [--identity NAME_OR_SHA1] [--no-open]
#   scripts/install.sh --dry-run [--prefix DIR]
#
# --dry-run builds and ad-hoc signs into DIR (a new temporary directory by default) and checks the result. It uses
# no identity and no keychain, and touches nothing outside DIR: no running Beacon, no LaunchAgent, no app opened.
set -euo pipefail
umask 077

identity=""
dry_run=false
prefix=""
open_app=true
while [[ $# -gt 0 ]]; do
	case "$1" in
	--identity) identity=${2:?--identity needs a name or SHA-1}; shift 2 ;;
	--dry-run) dry_run=true; shift ;;
	--prefix) prefix=${2:?--prefix needs a directory}; shift 2 ;;
	--no-open) open_app=false; shift ;;
	*)
		echo "Usage: scripts/install.sh [--identity NAME_OR_SHA1] [--no-open] | --dry-run [--prefix DIR]" >&2
		exit 2
		;;
	esac
done
if [[ $(uname -s) != Darwin ]]; then
	echo "Beacon.app installs on macOS only." >&2
	exit 1
fi
if [[ -n "$prefix" && "$dry_run" != true ]]; then
	echo "--prefix is for --dry-run; a real install goes to ~/Applications and ~/.local/bin." >&2
	exit 2
fi
if [[ "$dry_run" != true && (! -t 0 || ! -t 1) ]]; then
	echo "install.sh signs Beacon with your identity, which can read Beacon's keychain items, so it runs only in" >&2
	echo "a terminal. For a test, use: scripts/install.sh --dry-run" >&2
	exit 1
fi
cd "$(dirname "${BASH_SOURCE[0]}")/.."
# A temporary directory without a trailing slash (GitHub's macOS runners set TMPDIR with one).
tmp=${TMPDIR:-/tmp}
tmp=${tmp%/}
commit=$(git rev-parse --verify 'HEAD^{commit}')

say() { printf '%s\n' "$*"; }
done_steps=()
step() {
	done_steps+=("$*")
	say "• $*"
}

# --- The signing identity.
if [[ "$dry_run" == true ]]; then
	cert="-"
	cert_name="ad hoc (dry run)"
	[[ -n "$prefix" ]] || prefix=$(mktemp -d "$tmp/beacon-install.XXXXXXXX")
	mkdir -p "$prefix"
	# The physical path: no symlinks (/var is /private/var) and no doubled slashes, so links and checks agree.
	prefix=$(cd "$prefix" && pwd -P)
else
	identities=$(/usr/bin/security find-identity -v -p codesigning |
		sed -nE 's/^[[:space:]]*[0-9]+\)[[:space:]]+([0-9A-Fa-f]{40})[[:space:]]+"(.*)"$/\1	\2/p')
	if [[ -n "$identity" ]]; then
		matches=$(awk -F '\t' -v want="$identity" 'tolower($1) == tolower(want) || $2 == want' <<< "$identities")
	else
		matches=$identities
	fi
	if [[ -z "$matches" || $(wc -l <<< "$matches") -ne 1 ]]; then
		if [[ -z "$identities" ]]; then
			echo "No valid code-signing identity. Create one with scripts/dev-signing-setup.sh, or an Apple" >&2
			echo "Development certificate in Xcode (docs/signing.md)." >&2
		else
			echo "Name one code-signing identity with --identity NAME_OR_SHA1. Valid identities:" >&2
			while IFS= read -r line; do printf '  %s\n' "$line" >&2; done <<< "$identities"
		fi
		exit 1
	fi
	cert=$(cut -f 1 <<< "$matches" | tr 'a-f' 'A-F')
	cert_name=$(cut -f 2 <<< "$matches")
fi
say "Installing Beacon at $commit, signed with: $cert_name"

# --- Build, then sign each part with its identifier and a requirement pinning the certificate.
log=$(mktemp "$tmp/beacon-build.XXXXXXXX")
stage=
cleanup() {
	rm -f "$log"
	if [[ -n "$stage" ]]; then rm -rf "$stage"; fi
	if [[ -n "${new:-}" ]]; then rm -rf "$new"; fi
}
trap cleanup EXIT
if ! scripts/build-app.sh "$commit" > "$log" 2>&1; then
	cat "$log" >&2
	echo "The build failed; nothing was installed." >&2
	exit 1
fi
bundle=$(sed -n 's/^Bundle: //p' "$log")
stage=$(dirname "$bundle")
step "Built Beacon.app from $commit"

requirement() {
	if [[ "$cert" == "-" ]]; then
		printf 'identifier "%s"' "$1"
	else
		printf 'identifier "%s" and certificate leaf = H"%s"' "$1" "$cert"
	fi
}
sign() {
	local id=$1 path=$2
	if [[ "$cert" == "-" ]]; then
		/usr/bin/codesign --force --sign - --timestamp=none --options runtime --identifier "$id" "$path"
	else
		/usr/bin/codesign --force --sign "$cert" --timestamp=none --options runtime --identifier "$id" \
			--requirements "=designated => $(requirement "$id")" "$path"
	fi
}
verify() {
	local app=$1
	/usr/bin/codesign --verify --strict --deep "$app"
	/usr/bin/codesign --verify --strict -R "=$(requirement dev.beacon.app)" "$app"
	/usr/bin/codesign --verify --strict -R "=$(requirement dev.beacon.cli)" "$app/Contents/Helpers/beacon"
	/usr/bin/codesign --verify --strict -R "=$(requirement dev.beacon.tether)" "$app/Contents/Helpers/tether"
}
chmod 755 "$bundle" "$bundle/Contents" "$bundle/Contents/MacOS" "$bundle/Contents/Helpers"
chmod 755 "$bundle/Contents/MacOS/Beacon" "$bundle/Contents/Helpers/beacon" "$bundle/Contents/Helpers/tether"
chmod 644 "$bundle/Contents/Info.plist"
# The CLI keeps the identifier and requirement form the collector's keychain items were made with.
sign dev.beacon.cli "$bundle/Contents/Helpers/beacon"
sign dev.beacon.tether "$bundle/Contents/Helpers/tether"
sign dev.beacon.app "$bundle"
verify "$bundle"
step "Signed the app (dev.beacon.app), the CLI (dev.beacon.cli) and tether (dev.beacon.tether)"

# --- Install, replacing any earlier copy only once the new one is in place and verified.
home=${prefix:-$HOME}
apps="$home/Applications"
bin="$home/.local/bin"
app="$apps/Beacon.app"
mkdir -p "$apps" "$bin"
new="$apps/.Beacon.app.new.$$"
/usr/bin/ditto "$bundle" "$new"
verify "$new"

if [[ "$dry_run" != true ]]; then
	# A running Beacon quits as with Quit: it stops its services and expires approvals not yet started.
	running=$(pgrep -U "$(id -u)" -f '/Beacon\.app/Contents/MacOS/Beacon$' || true)
	if [[ -n "$running" ]]; then
		say "Quitting the running Beacon…"
		# shellcheck disable=SC2086
		kill -TERM $running 2>/dev/null || true
		for _ in $(seq 1 45); do
			pgrep -U "$(id -u)" -f '/Beacon\.app/Contents/MacOS/Beacon$' >/dev/null || break
			sleep 1
		done
		if pgrep -U "$(id -u)" -f '/Beacon\.app/Contents/MacOS/Beacon$' >/dev/null; then
			echo "Beacon is still running. Quit it from its menu, then run install.sh again." >&2
			exit 1
		fi
		step "Quit the running Beacon"
	fi
fi
old=
if [[ -e "$app" || -L "$app" ]]; then
	old="$apps/.Beacon.app.old.$$"
	mv "$app" "$old"
fi
if ! mv "$new" "$app"; then
	if [[ -n "$old" ]]; then mv "$old" "$app"; fi
	echo "Could not install $app; the earlier copy is back in place." >&2
	exit 1
fi
new=
if [[ -n "$old" ]]; then rm -rf "$old"; fi
verify "$app"
step "Installed $app"

# --- beacon on PATH is the app's CLI.
link="$bin/beacon"
target="$app/Contents/Helpers/beacon"
if [[ -e "$link" && ! -L "$link" ]]; then
	mv "$link" "$link.pre-app"
	step "Moved the old $link to $link.pre-app"
fi
ln -sfn "$target" "$link.new.$$"
mv -f "$link.new.$$" "$link"
step "Linked $link to the app's CLI"

if [[ "$dry_run" == true ]]; then
	[[ "$(readlink "$link")" == "$target" && -x "$link" ]]
	say ""
	say "Dry run: built, ad-hoc signed and installed under $prefix. No keychain, running app or LaunchAgent was touched."
	exit 0
fi

# --- The retired collector LaunchAgent: two collectors would contend for one outbox.
if [[ -e "$HOME/Library/LaunchAgents/dev.beacon.agent.plist" ]] ||
	/bin/launchctl print "gui/$(id -u)/dev.beacon.agent" >/dev/null 2>&1; then
	"$target" service uninstall
	step "Removed the old collector LaunchAgent (dev.beacon.agent); Beacon.app runs the collector now"
fi

if [[ "$open_app" == true ]]; then
	/usr/bin/open "$app"
	step "Opened Beacon"
fi

case ":$PATH:" in
*":$bin:"*) ;;
*) say "Note: $bin is not on your PATH. Add it so \`beacon\` and \`beacon act\` are the app's CLI." ;;
esac
cat <<EOF

Next:
  1. Beacon's menu → Settings… → GitHub: paste a GitHub token for your own account.
  2. Settings… → Signal: paste the Beacon setup code from Signal's /beacon page. If you connected Signal
     before, paste the code again.
  3. Approvals… (⌥⌘B) shows proposals as they arrive. Beacon is the beacon-light icon in the menu bar, with a
     number while proposals wait; opening Beacon.app again also shows the window.

Keychain: your collector's keychain items were made by a CLI signed with this certificate and identifier
dev.beacon.cli. The app's CLI keeps both, so the items stay readable. With a self-signed certificate, macOS may ask
once per item after a rebuild; choose Always Allow. With an Apple Development certificate it does not ask.
The first signed launch of the app creates its vault key in your login keychain.
EOF
