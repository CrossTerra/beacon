#!/bin/bash
# Exercise the installer transaction with temporary homes and fake managers. No live services or credentials.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/beacon-install-test.XXXXXXXX")
trap 'rm -rf "$tmp"' EXIT
repo="$tmp/fixture"
mkdir -p "$repo/scripts"
cp scripts/install.sh "$repo/scripts/"
fixture_commit() { git -C "$repo" -c user.name=Fixture -c user.email=fixture@example.test -c commit.gpgsign=false commit -qm "$1" --allow-empty; }
git -C "$repo" init -q
git -C "$repo" add scripts
fixture_commit first
touch "$repo/untracked"
cat > "$repo/driver" <<'SH'
#!/bin/bash
source "${0%/*}/scripts/install.sh"
[[ "$HOME" == "$FIXTURE"/home-* ]] || exit 99
uname() { if [[ $1 == '-s' ]]; then printf '%s\n' "$OS"; else echo arm64; fi; }
id() { echo 501; }
check_unit() { grep -q 'serve' "$1"; }
swift_pgrep() { [[ -f "$HOME/swift-running" ]]; }
log_start() {
	touch "$HOME/serve-running"
	"$bin" version | sed -n 's/^commit:    /start /p' >> "$trace"
}
macctl() {
	[[ ${BROKEN_MANAGER:-} != yes ]] || return 2
	case "$1 $2" in
		'print gui/501') return 0 ;;
		'print gui/501/dev.beacon.serve') if [[ -f "$HOME/serve-running" ]]; then return 0; fi ;;
		'print gui/501/dev.beacon.agent') if [[ -f "$HOME/legacy-gui" ]]; then return 0; fi ;;
		'print user/501/dev.beacon.agent') if [[ -f "$HOME/legacy-user" ]]; then return 0; fi ;;
		'bootout gui/501/dev.beacon.serve') rm -f "$HOME/serve-running"; echo 'stop launchd' >> "$trace"; return ;;
		'enable gui/501/dev.beacon.serve') return 0 ;;
		'bootstrap gui/501') [[ "$3" == "$unit" && -f "$unit" ]] || return 99; log_start; return ;;
		*) return 99 ;;
	esac
	printf 'Could not find service "fixture"\n' >&2
	return 113
}
systemctl() {
	[[ "$1" == --user && ${BROKEN_MANAGER:-} != yes ]] || return 2
	case "$2" in
		show)
			if [[ -f "$unit" ]]; then echo LoadState=loaded; else echo LoadState=not-found; fi
			if [[ -f "$HOME/serve-running" ]]; then echo ActiveState=active; else echo ActiveState=inactive; fi ;;
		stop) [[ "$3" == beacon.service ]] || return 99; rm -f "$HOME/serve-running"; echo 'stop systemd' >> "$trace" ;;
		enable) [[ "$3" == --now && "$4" == beacon.service && -f "$unit" ]] || return 99
			mkdir -p "${unit%/*}/default.target.wants"
			ln -sfn ../beacon.service "${unit%/*}/default.target.wants/beacon.service"
			log_start ;;
		disable) [[ "$3" == beacon.service && -f "$unit" ]] || return 99
			rm -f "${unit%/*}/default.target.wants/beacon.service" ;;
		daemon-reload) ;;
		*) return 99 ;;
	esac
}
wait_ready() { echo health >> "$trace"; [[ ${BAD_HEALTH:-} != yes ]]; }
build_beacon() {
	[[ ! -e "$1/untracked" && -f "$1/scripts/install.sh" ]] || return 99
	printf 'build %s\n' "$2" >> "$trace"
	cat > "$3" <<EOF
#!/bin/bash
set -euo pipefail
if [[ \$1 == version ]]; then echo "commit:    $2"; exit; fi
if [[ \$1 == migrate-swift ]]; then
	[[ ! -e "\$HOME/Applications/Beacon.app/Contents/MacOS/Beacon" && ! -e "\$HOME/Library/LaunchAgents/dev.beacon.agent.plist" ]]
	echo archive >> "\$trace"
	mkdir -p "\$3/swift-archive"
	exit
fi
[[ \$1 == setup && \$2 == --state-dir ]]
if [[ \$4 == --show-origin ]]; then sed -nE 's/^  "origin": "([a-z0-9.:/-]+)",?$/\1/p' "\$3/settings.json"; exit; fi
if [[ \$4 == --check-stopped ]]; then exit; fi
[[ \$4 == --origin ]]
printf 'setup %s\n' "\$5" >> "\$trace"
mkdir -p "\$3"
origin=http://localhost:7317
[[ \$5 == localhost ]] || origin=https://fixture.tail.ts.net
printf '{\n  "origin": "%s"\n}\n' "\$origin" > "\$3/settings.json"
if [[ \${SWIFT_RELAUNCH:-} == yes ]]; then touch "\$HOME/swift-running"; fi
EOF
	chmod 755 "$3"
}
main "$@"
SH

export FIXTURE=$tmp
unset BEACON_CONFIG XDG_STATE_HOME XDG_CONFIG_HOME
test_install() {
	export OS=$1 HOME="$tmp/home-$1" trace="$tmp/$1.trace"
	local state unit initial now bin before f
	mkdir "$HOME"
	bin="$HOME/.local/bin/beacon"
	if [[ "$OS" == Darwin ]]; then
		state="$HOME/Library/Application Support/beacon"
		unit="$HOME/Library/LaunchAgents/dev.beacon.serve.plist"
	else
		state="$HOME/.local/state/beacon"
		unit="$HOME/.config/systemd/user/beacon.service"
	fi
	initial=$(git -C "$repo" rev-parse HEAD)
	/bin/bash "$repo/driver" --origin tailscale
	grep -Fxq "start $initial" "$trace"
	grep -Fq "$bin" "$unit"
	if [[ "$OS" == Darwin ]]; then grep -Fq 'dev.beacon.serve' "$unit"; else grep -Fxq 'KillMode=control-group' "$unit"; fi
	for f in journal.log passkeys.json unattended.kdbx unattended.keyx write-vault.json; do
		printf 'keep %s\n' "$f" > "$state/$f"
	done
	fixture_commit "$OS-upgrade"
	now=$(git -C "$repo" rev-parse HEAD)
	/bin/bash "$repo/driver" --origin tailscale
	grep -Fxq "start $now" "$trace"
	[[ $(grep -c '^setup ' "$trace") == 1 ]]
	before=$(cat "$trace")
	if /bin/bash "$repo/driver" --origin localhost; then return 1; fi
	[[ $(cat "$trace") == "$before" ]]
	printf '\n# tracked edit\n' >> "$repo/scripts/install.sh"
	if /bin/bash "$repo/driver"; then return 1; fi
	[[ $(cat "$trace") == "$before" ]]
	git -C "$repo" show "$now:scripts/install.sh" > "$repo/scripts/install.sh"
	fixture_commit "$OS-fail"
	if BAD_HEALTH=yes /bin/bash "$repo/driver"; then return 1; fi
	"$bin" version | grep -Fq "$now"
	[[ $(tail -1 "$trace") == "start $now" ]]
	rm "$unit" # manager still owns the live job
	if BROKEN_MANAGER=yes /bin/bash "$repo/driver" --uninstall; then return 1; fi
	[[ -f "$bin" && -f "$HOME/serve-running" ]]
	/bin/bash "$repo/driver" --uninstall
	[[ ! -e "$HOME/serve-running" && ! -L "${unit%/*}/default.target.wants/beacon.service" ]]
	/bin/bash "$repo/driver" --uninstall
	[[ ! -e "$unit" && ! -e "$bin" ]]
	printf 'another binary\n' > "$bin"
	/bin/bash "$repo/driver" --uninstall
	grep -Fxq 'another binary' "$bin"
	for f in journal.log passkeys.json unattended.kdbx unattended.keyx write-vault.json; do
		grep -Fxq "keep $f" "$state/$f"
	done
	[[ $(grep -c '^setup ' "$trace") == 1 ]]
}

test_legacy() {
	export OS=Darwin HOME="$tmp/home-legacy" trace="$tmp/legacy.trace"
	local old_bin="$HOME/.local/bin/beacon" legacy="$HOME/Library/LaunchAgents/dev.beacon.agent.plist"
	local executable="$HOME/Applications/Beacon.app/Contents/MacOS/Beacon"
	mkdir -p "${old_bin%/*}" "${legacy%/*}" "${executable%/*}"
	printf 'legacy CLI\n' > "$old_bin" # the pre-Swift installer used a regular file
	touch "$HOME/legacy-user"
	if /bin/bash "$repo/driver" --origin localhost; then return 1; fi
	grep -Fxq 'legacy CLI' "$old_bin"
	if grep -q '^start ' "$trace"; then return 1; fi
	rm "$HOME/legacy-user"
	touch "$legacy" "$executable"
	if SWIFT_RELAUNCH=yes /bin/bash "$repo/driver" --migrate-swift --origin localhost; then return 1; fi
	[[ -f "$HOME/Library/dev.beacon.agent.plist.disabled" && -f "$executable.disabled" && ! -e "$legacy" && ! -e "$executable" ]]
	grep -Fxq 'legacy CLI' "$old_bin"
	[[ $(grep -c '^archive$' "$trace") == 1 ]]
	if grep -q '^start ' "$trace"; then return 1; fi
	rm "$HOME/swift-running"
	/bin/bash "$repo/driver" --migrate-swift
	[[ $(grep -c '^archive$' "$trace") == 3 && -f "$HOME/serve-running" ]]
	/bin/bash "$repo/driver" --uninstall
}

test_install Darwin
test_install Linux
test_legacy
echo "ok temporary launchd and systemd installs, upgrades, rollback and uninstall"
