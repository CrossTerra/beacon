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
swift_stopped() { echo 'fixture must not inspect Swift' >&2; return 99; }
stop_service() { echo stop >> "$trace"; }
start_service() { "$bin" version | sed -n 's/^commit:    /start /p' >> "$trace"; }
wait_ready() { echo health >> "$trace"; [[ ${BAD_HEALTH:-} != yes ]]; }
systemctl() { printf 'manager %s\n' "$*" >> "$trace"; }
build_beacon() {
	[[ ! -e "$1/untracked" && -f "$1/scripts/install.sh" ]] || return 99
	printf 'build %s\n' "$2" >> "$trace"
	cat > "$3" <<EOF
#!/bin/bash
set -euo pipefail
if [[ \$1 == version ]]; then echo "commit:    $2"; exit; fi
[[ \$1 == setup && \$2 == --state-dir ]]
if [[ \$4 == --show-origin ]]; then sed -nE 's/^  "origin": "([a-z0-9.:/-]+)",?$/\1/p' "\$3/settings.json"; exit; fi
if [[ \$4 == --check-stopped ]]; then exit; fi
[[ \$4 == --origin ]]
printf 'setup %s\n' "\$5" >> "\$trace"
mkdir -p "\$3"
origin=http://localhost:7317
[[ \$5 == localhost ]] || origin=https://fixture.tail.ts.net
printf '{\n  "origin": "%s"\n}\n' "\$origin" > "\$3/settings.json"
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
	/bin/bash "$repo/driver" --uninstall
	/bin/bash "$repo/driver" --uninstall
	[[ ! -e "$unit" && ! -e "$bin" ]]
	for f in journal.log passkeys.json unattended.kdbx unattended.keyx write-vault.json; do
		grep -Fxq "keep $f" "$state/$f"
	done
	[[ $(grep -c '^setup ' "$trace") == 1 ]]
}

test_install Darwin
test_install Linux
echo "ok temporary launchd and systemd installs, upgrades, rollback and uninstall"
