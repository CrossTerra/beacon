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
export FIXTURE_DRIVER_PID=$$
uname() { if [[ $1 == '-s' ]]; then printf '%s\n' "$OS"; else echo arm64; fi; }
id() { echo 501; }
mkdir() {
	command mkdir "$@" || return
	# Interrupt only this fixture driver, after mkdir succeeded but before lock ownership is recorded.
	if [[ ${INTERRUPT_LOCK:-} == yes && "$1" == */.beacon-install.lock ]]; then kill -TERM "$$"; fi
}
check_unit() { grep -q 'serve' "$1"; }
swift_pgrep() { [[ -f "$HOME/swift-running" ]]; }
operator_terminal() { [[ ${NO_TERMINAL:-} != yes ]]; } # The driver captures a fixture code, never a real one.
fixture_pid=4242
pid_alive() {
	[[ "$1" == "$fixture_pid" && -f "$HOME/serve-pid" ]] || return 1
	echo 'pid still running' >> "$trace"
}
fake_stop() {
	rm -f "$HOME/serve-running"
	echo "stop $1" >> "$trace"
	case ${SLOW_STOP:-} in
		yes) touch "$HOME/serve-loaded" "$HOME/serve-pid" "$HOME/instance-held" ;;
		stuck) touch "$HOME/instance-held" ;;
	esac
	if [[ ${ENROLL_DURING_STOP:-} == yes ]]; then touch "$state/passkeys.json"; fi
	if [[ ${INTERRUPT_RECOVERY_STOP:-} == yes ]]; then kill -TERM "$$"; fi # Only this fixture driver.
	if [[ ${STOP_EXIT:-0} != 0 ]]; then
		printf 'stop returned %s\n' "$STOP_EXIT" >> "$trace"
		printf '%s stop is already in progress\n' "$1" >&2
		return "$STOP_EXIT"
	fi
}
inspect_fails() {
	if [[ ${STOP_INSPECT:-} == transient && -f "$HOME/serve-loaded" ||
		${STOP_INSPECT:-} == persistent && ! -f "$HOME/serve-running" ]]; then
		echo 'manager inspection failed' >> "$trace"
		echo 'Operation now in progress' >&2
		return 0
	fi
	return 1
}
sleep() {
	if [[ ${SLOW_STOP:-} == stuck || ${STOP_INSPECT:-} == persistent || ${BROKEN_MANAGER:-} == yes ]]; then
		SECONDS=$((SECONDS + 61)) # Exercise the deadline without spending a minute in CI.
		return 0
	fi
	command sleep "$1"
	# The manager can forget the job before the PID exits, and its reaper drains last.
	if [[ -f "$HOME/serve-loaded" ]]; then
		rm "$HOME/serve-loaded"
		echo 'job unloaded' >> "$trace"
	elif [[ -f "$HOME/serve-pid" ]]; then
		rm "$HOME/serve-pid"
		echo 'pid exited' >> "$trace"
	elif [[ -f "$HOME/instance-held" ]]; then
		rm "$HOME/instance-held"
		echo 'lock released' >> "$trace"
	fi
}
log_start() {
	if [[ -e "$HOME/serve-loaded" || -e "$HOME/serve-pid" || -e "$HOME/instance-held" ]]; then
		echo 'started before shutdown finished' >&2
		return 99
	fi
	touch "$HOME/serve-running"
	"$bin" version | sed -n 's/^commit:    /start /p' >> "$trace"
}
macctl() {
	[[ ${BROKEN_MANAGER:-} != yes ]] || return 2
	case "$1 $2" in
		'print gui/501') return 0 ;;
		'print-disabled gui/501')
			echo 'disabled services = {'
			if [[ -f "$HOME/service-disabled" ]]; then echo '"dev.beacon.serve" => disabled'; else echo '"dev.beacon.serve" => enabled'; fi
			echo '}'; return 0 ;;
		'print gui/501/dev.beacon.serve')
			if inspect_fails; then return 36; fi
			if [[ -f "$HOME/serve-running" || -f "$HOME/serve-loaded" ]]; then
				local loaded_unit=$unit
				if [[ ${WRONG_SERVICE:-} == yes ]]; then loaded_unit+='.other'; fi
				printf 'gui/501/dev.beacon.serve = {\n\tpid = %s\n\tstate = running\n\tpath = %s\n\tprogram = %s\n\targuments = {\n\t\t%s\n\t\tserve\n\t\t--state-dir\n\t\t%s\n\t\t--config\n\t\t%s\n\t}\n}\n' \
					"$fixture_pid" "$loaded_unit" "$bin" "$bin" "$state" "$config"
				return 0
			fi ;;
		'print gui/501/dev.beacon.agent') if [[ -f "$HOME/legacy-gui" ]]; then return 0; fi ;;
		'print user/501/dev.beacon.agent') if [[ -f "$HOME/legacy-user" ]]; then return 0; fi ;;
		'bootout gui/501/dev.beacon.serve') fake_stop launchd; return "$?" ;;
		'enable gui/501/dev.beacon.serve') echo 'enable launchd' >> "$trace"; return 0 ;;
		'bootstrap gui/501')
			[[ "$3" == "$unit" && -f "$unit" ]] || return 99
			if [[ -f "$HOME/bootstrap-5" ]]; then
				rm "$HOME/bootstrap-5"
				echo 'bootstrap error 5' >> "$trace"
				echo 'Bootstrap failed: 5: Input/output error' >&2
				return 5
			fi
			log_start; return "$?" ;;
		*) return 99 ;;
	esac
	printf 'Could not find service "fixture"\n' >&2
	return 113
}
systemctl() {
	[[ "$1" == --user && ${BROKEN_MANAGER:-} != yes ]] || return 2
	case "$2" in
		show)
			if inspect_fails; then return 1; fi
			if [[ -f "$unit" ]]; then echo LoadState=loaded; else echo LoadState=not-found; fi
			local loaded_unit=$unit
			if [[ ${WRONG_SERVICE:-} == yes ]]; then loaded_unit+='.other'; fi
			printf 'FragmentPath=%s\nDropInPaths=\nNeedDaemonReload=no\n' "$loaded_unit"
			if [[ -f "$HOME/serve-running" ]]; then
				printf 'ActiveState=active\nMainPID=%s\n' "$fixture_pid"
			elif [[ -f "$HOME/serve-loaded" ]]; then
				printf 'ActiveState=deactivating\nMainPID=%s\n' "$fixture_pid"
			else
				printf 'ActiveState=inactive\nMainPID=0\n'
			fi ;;
		stop) [[ "$3" == beacon.service || "$3 $4" == '--no-block beacon.service' ]] || return 99; fake_stop systemd ;;
		start) [[ "$3 $4" == '--no-block beacon.service' ]] || return 99; log_start ;;
		is-enabled)
			if [[ -f "$HOME/service-disabled" ]]; then echo disabled; return 1; fi
			echo enabled ;;
		enable) [[ "$3" == --now && "$4" == beacon.service && -f "$unit" ]] || return 99
			echo 'enable systemd' >> "$trace"
			mkdir -p "${unit%/*}/default.target.wants"
			ln -sfn ../beacon.service "${unit%/*}/default.target.wants/beacon.service"
			log_start ;;
		disable) [[ "$3" == beacon.service && -f "$unit" ]] || return 99
			rm -f "${unit%/*}/default.target.wants/beacon.service" ;;
		daemon-reload) ;;
		*) return 99 ;;
	esac
}
wait_ready() {
	[[ -z $(/usr/bin/printenv recovery_code || :) ]] || return 99
	echo health >> "$trace"
	if [[ ${BAD_HEALTH:-} == yes ]]; then
		if [[ "$OS" == Darwin ]]; then touch "$HOME/bootstrap-5"; fi
		return 1
	fi
}
build_beacon() {
	[[ ! -e "$1/untracked" && -f "$1/scripts/install.sh" ]] || return 99
	printf 'build %s\n' "$2" >> "$trace"
	cat > "$3" <<EOF
#!/bin/bash
set -euo pipefail
if [[ \$1 == version ]]; then echo "commit:    $2"; exit; fi
if [[ \$1 == migrate-swift ]]; then
	[[ -f "\$3/app/journal.sqlite" ]]
	[[ ! -e "\$HOME/Applications/Beacon.app/Contents/MacOS/Beacon" && ! -e "\$HOME/Library/LaunchAgents/dev.beacon.agent.plist" ]]
	if [[ -d "\$3/swift-archive" ]]; then
		echo verify >> "\$trace"
	else
		mkdir "\$3/swift-archive"
		echo archive >> "\$trace"
		echo 'Archived jrn_fixture. No Swift actions will be replayed.'
	fi
	exit
fi
[[ \$1 == setup && \$2 == --state-dir ]]
if [[ \$4 == --show-origin ]]; then sed -nE 's/^  "origin": "([a-z0-9.:/-]+)",?$/\1/p' "\$3/settings.json"; exit; fi
if [[ \$4 == --check-stopped ]]; then
	if [[ -e "\$HOME/serve-loaded" || -e "\$HOME/serve-pid" ]]; then echo 'checked lock too early' >> "\$trace"; fi
	if [[ -e "\$HOME/instance-held" ]]; then
		echo 'lock still held' >> "\$trace"
		echo 'beacon: another process holds the approval instance lock' >&2
		exit 1
	fi
	exit
fi
if [[ \$4 == --check-reissue-code || \$4 == --reissue-code ]]; then
	[[ \$# == 4 && -f "\$3/settings.json" ]]
	if [[ \$4 == --reissue-code ]]; then
		echo 'offline issue' >> "\$trace"
		[[ ! -e "\$HOME/serve-running" && ! -e "\$HOME/instance-held" && ! -e "\$HOME/serve-pid" && ! -e "\$HOME/serve-loaded" ]]
	fi
	if [[ -f "\$3/passkeys.json" ]]; then echo 'beacon: a passkey is already registered' >&2; exit 1; fi
	if [[ \$4 == --reissue-code ]]; then
		if [[ \${ISSUE_FAIL:-} == yes ]]; then echo 'fixture issuance failed' >&2; exit 1; fi
		if [[ \${INTERRUPT_ISSUE:-} == yes ]]; then kill -TERM "\$FIXTURE_DRIVER_PID"; exit 1; fi
		echo 'fixture-only code hash' > "\$3/setup-code.json"
		echo 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
	fi
	exit
fi
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

export FIXTURE=$tmp TMPDIR=$tmp
unset BEACON_CONFIG XDG_STATE_HOME XDG_CONFIG_HOME

test_toolchain() {
	local tools="$tmp/tools" src="$tmp/go-src" target="$tmp/go-beacon" output="$tmp/go.out"
	mkdir -p "$tools" "$src"
	cat > "$tools/go" <<'SH'
#!/bin/bash
set -euo pipefail
case "$1" in
	env)
		[[ "$2" == GOVERSION ]]
		case "$GOTOOLCHAIN" in
			local) echo "$FAKE_GO_VERSION" ;;
			go1.24.3)
				if [[ ${GOPROXY:-} == off && "$FAKE_GO_VERSION" != go1.24.3 ]]; then
					echo 'fixture toolchain download failure' >&2
					exit 1
				fi
				echo go1.24.3 ;;
			*) exit 99 ;;
		esac ;;
	build)
		[[ "$PWD" == "$FAKE_GO_SRC" && "$GOTOOLCHAIN:$CGO_ENABLED:$GOFLAGS:$GOOS:$GOARCH" == go1.24.3:0::: ]]
		[[ $# == 8 && "$2:$3:$4:$5:$6:$8" == '-trimpath:-buildvcs=false:-ldflags:-X main.commit=fixture:-o:./cmd/beacon' ]]
		touch "$7" ;;
	*) exit 99 ;;
esac
SH
	chmod +x "$tools/go"
	# shellcheck disable=SC2016 # these are the inner bash's positional arguments
	local run='source "$1"; build_beacon "$2" fixture "$3"'
	local version
	for version in go1.21.0 go1.24.3 go1.27.1; do
		PATH="$tools:$PATH" FAKE_GO_VERSION=$version FAKE_GO_SRC=$src GOPROXY=fixture \
			/bin/bash -u -c "$run" fixture "$repo/scripts/install.sh" "$src" "$target"
		[[ -f "$target" ]]
		rm "$target"
	done
	if PATH="$tools:$PATH" FAKE_GO_VERSION=go1.20.14 FAKE_GO_SRC=$src \
		/bin/bash -u -c "$run" fixture "$repo/scripts/install.sh" "$src" "$target" >"$output" 2>&1; then return 1; fi
	grep -Fq 'Put Go 1.21 or newer on PATH' "$output"
	if PATH="$tools:$PATH" FAKE_GO_VERSION=go1.27.1 FAKE_GO_SRC=$src GOPROXY=off \
		/bin/bash -u -c "$run" fixture "$repo/scripts/install.sh" "$src" "$target" >"$output" 2>&1; then return 1; fi
	grep -Fq 'install Go 1.24.3 from https://go.dev/dl/' "$output"
	[[ $(wc -l < "$output") -eq 1 && ! -e "$target" ]]
}

test_install() {
	export OS=$1 HOME="$tmp/home-$1" trace="$tmp/$1.trace"
	local state unit initial now bin before f stop_exit=1 output="$tmp/$1.out"
	mkdir "$HOME"
	bin="$HOME/.local/bin/beacon"
	if [[ "$OS" == Darwin ]]; then
		stop_exit=36
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
	SLOW_STOP=yes STOP_EXIT=$stop_exit STOP_INSPECT=transient /bin/bash "$repo/driver" >"$output" 2>&1 || { cat "$output" >&2; return 1; }
	grep -Fxq "start $now" "$trace"
	grep -Fq 'Waiting for Beacon to finish stopping' "$output"
	grep -Fxq "stop returned $stop_exit" "$trace"
	grep -Fxq 'manager inspection failed' "$trace"
	if grep -Eq 'another process holds|already in progress|Could not inspect' "$output" ||
		grep -q 'checked lock too early' "$trace"; then return 1; fi
	[[ $(grep -c '^pid still running$' "$trace") == 1 && $(grep -c '^lock still held$' "$trace") == 1 ]]
	[[ $(grep -c '^setup ' "$trace") == 1 ]]
	before=$(cat "$trace")
	if /bin/bash "$repo/driver" --origin localhost; then return 1; fi
	[[ $(cat "$trace") == "$before" ]]
	printf '\n# tracked edit\n' >> "$repo/scripts/install.sh"
	if /bin/bash "$repo/driver"; then return 1; fi
	[[ $(cat "$trace") == "$before" ]]
	git -C "$repo" show "$now:scripts/install.sh" > "$repo/scripts/install.sh"
	fixture_commit "$OS-fail"
	if SLOW_STOP=yes STOP_EXIT=$stop_exit STOP_INSPECT=transient BAD_HEALTH=yes /bin/bash "$repo/driver" >"$output" 2>&1; then return 1; fi
	"$bin" version | grep -Fq "$now"
	[[ $(tail -1 "$trace") == "start $now" ]]
	[[ $(grep -c '^pid still running$' "$trace") == 3 && $(grep -c '^lock still held$' "$trace") == 3 ]]
	[[ $(grep -c '^manager inspection failed$' "$trace") == 3 ]]
	if [[ "$OS" == Darwin ]]; then
		[[ $(grep -c '^bootstrap error 5$' "$trace") == 1 ]]
		grep -Fq 'launchd bootstrap returned error 5; retrying' "$output"
	fi
	rm "$unit" # manager still owns the live job
	if BROKEN_MANAGER=yes /bin/bash "$repo/driver" --uninstall >"$output" 2>&1; then return 1; fi
	grep -Fq 'Timed out after 60 seconds waiting for Beacon to stop' "$output"
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

test_stop_timeout() {
	local slow=$1 inspect=$2 reason=$3 name="stop-timeout-$2"
	export OS=Darwin HOME="$tmp/home-$name" trace="$tmp/$name.trace"
	local initial bin="$HOME/.local/bin/beacon" output="$tmp/$name.out"
	mkdir "$HOME"
	initial=$(git -C "$repo" rev-parse HEAD)
	/bin/bash "$repo/driver" --origin localhost
	fixture_commit "$name"
	if SLOW_STOP=$slow STOP_INSPECT=$inspect /bin/bash "$repo/driver" >"$output" 2>&1; then return 1; fi
	grep -Fq "Timed out after 60 seconds waiting for Beacon to stop: $reason." "$output"
	"$bin" version | grep -Fq "$initial"
	[[ $(grep -c '^start ' "$trace") == 1 && ! -e "${bin%/*}/.beacon-install.lock" ]]
	rm -f "$HOME/instance-held"
	/bin/bash "$repo/driver" --uninstall
}

test_reissue() {
	export OS=$1 HOME="$tmp/home-reissue-$1" trace="$tmp/reissue-$1.trace"
	local bin="$HOME/.local/bin/beacon" state unit before flags stdout="$tmp/reissue-$1.out" stderr="$tmp/reissue-$1.err"
	local fixture_code=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
	mkdir "$HOME"
	if /bin/bash "$repo/driver" --reissue-code >"$stdout" 2>"$stderr"; then return 1; fi
	[[ ! -e "$HOME/.local" && ! -s "$stdout" ]]
	/bin/bash "$repo/driver" --origin localhost > /dev/null
	if [[ "$OS" == Darwin ]]; then
		state="$HOME/Library/Application Support/beacon"
		unit="$HOME/Library/LaunchAgents/dev.beacon.serve.plist"
	else
		state="$HOME/.local/state/beacon"
		unit="$HOME/.config/systemd/user/beacon.service"
	fi
	local receipt="${bin%/*}/.beacon-installed.sha256" lock="${bin%/*}/.beacon-install.lock" name
	before=$(cat "$trace")
	mkdir "$tmp/reissue-$OS-before"
	for name in "$bin" "$unit" "$receipt" "$state/settings.json"; do cp "$name" "$tmp/reissue-$OS-before/"; done

	touch "$state/passkeys.json"
	if /bin/bash "$repo/driver" --reissue-code >"$stdout" 2>"$stderr"; then return 1; fi
	grep -Fq 'a passkey is already registered' "$stderr"
	[[ $(cat "$trace") == "$before" && ! -s "$stdout" && ! -e "$lock" && -e "$HOME/serve-running" ]]
	rm "$state/passkeys.json"
	for flags in NO_TERMINAL WRONG_SERVICE; do
		if env "$flags=yes" /bin/bash "$repo/driver" --reissue-code >"$stdout" 2>"$stderr"; then return 1; fi
		[[ $(cat "$trace") == "$before" && ! -s "$stdout" && ! -e "$lock" && -e "$HOME/serve-running" ]]
	done
	printf '\n# modified service\n' >> "$unit"
	if /bin/bash "$repo/driver" --reissue-code >"$stdout" 2>"$stderr"; then return 1; fi
	[[ $(cat "$trace") == "$before" && ! -s "$stdout" && ! -e "$lock" ]]
	cp "$tmp/reissue-$OS-before/${unit##*/}" "$unit"

	# The trap restores the running service on a failed issuance, an enrollment that wins the race,
	# and a signal during either the stop or the offline command.
	local starts=1 slow
	for flags in ISSUE_FAIL ENROLL_DURING_STOP INTERRUPT_RECOVERY_STOP INTERRUPT_ISSUE; do
		slow=''
		if [[ "$flags" == INTERRUPT_RECOVERY_STOP ]]; then slow=yes; fi
		if SLOW_STOP=$slow env "$flags=yes" /bin/bash "$repo/driver" --reissue-code >"$stdout" 2>"$stderr"; then return 1; fi
		starts=$((starts + 1))
		if [[ $(grep -c '^start ' "$trace") != "$starts" || -s "$stdout" || -e "$lock" || ! -e "$HOME/serve-running" ]]; then
			cat "$stderr" >&2
			return 1
		fi
		rm -f "$state/passkeys.json"
	done
	[[ $(grep -c '^offline issue$' "$trace") == 3 ]] # Interrupted stop must not issue.
	# Even with shell tracing and an exported variable of the same name, the code goes only to the terminal.
	if [[ "$OS" == Darwin ]]; then touch "$HOME/bootstrap-5"; fi
	recovery_code=placeholder /bin/bash -ux "$repo/driver" --reissue-code >"$stdout" 2>"$stderr"
	[[ $(<"$stdout") == "$fixture_code" && $(grep -c '^start ' "$trace") == 6 ]]
	if [[ "$OS" == Darwin ]]; then grep -Fxq 'bootstrap error 5' "$trace"; fi
	if grep -Fq "$fixture_code" "$stderr" "$trace"; then return 1; fi

	touch "$HOME/service-disabled"
	before=$(cat "$trace")
	if /bin/bash "$repo/driver" --reissue-code >"$stdout" 2>"$stderr"; then return 1; fi
	[[ ! -s "$stdout" && $(cat "$trace") == "$before" && -e "$HOME/serve-running" ]]
	rm "$HOME/serve-running"
	/bin/bash "$repo/driver" --reissue-code >"$stdout" 2>"$stderr"
	[[ $(<"$stdout") == "$fixture_code" && ! -e "$HOME/serve-running" && -e "$HOME/service-disabled" ]]
	grep -Fq 'remains stopped' "$stderr"
	[[ $(grep -c '^start ' "$trace") == 6 && $(grep -c '^enable ' "$trace") == 1 && $(grep -c '^build ' "$trace") == 1 ]]
	[[ $(grep -c '^setup ' "$trace") == 1 && ! -e "$lock" ]]
	for name in "$bin" "$unit" "$receipt" "$state/settings.json"; do cmp -s "$name" "$tmp/reissue-$OS-before/${name##*/}"; done
}

test_legacy() {
	# Fake launchd on both hosts: Linux CI must exercise Bash 5's errexit here.
	export OS=Darwin HOME="$tmp/home-legacy" trace="$tmp/legacy.trace"
	local old_bin="$HOME/.local/bin/beacon" legacy="$HOME/Library/LaunchAgents/dev.beacon.agent.plist"
	local executable="$HOME/Applications/Beacon.app/Contents/MacOS/Beacon"
	local state="$HOME/Library/Application Support/beacon" output="$tmp/legacy.out" lock="${old_bin%/*}/.beacon-install.lock"
	mkdir -p "${old_bin%/*}" "${legacy%/*}" "${executable%/*}"
	printf 'legacy CLI\n' > "$old_bin" # the pre-Swift installer used a regular file
	touch "$HOME/legacy-user"
	if /bin/bash "$repo/driver" --origin localhost >"$output" 2>&1; then return 1; fi
	if ! grep -Fq 'Unload the retired user/501/dev.beacon.agent LaunchAgent before cutover.' "$output" ||
		grep -Fq 'unbound variable' "$output" || [[ -e "$lock" ]]; then
		cat "$output" >&2
		echo 'Legacy job refusal did not exit cleanly and release its lock.' >&2
		return 1
	fi
	if INTERRUPT_LOCK=yes /bin/bash "$repo/driver" --origin localhost; then return 1; fi
	[[ ! -e "$lock" ]]
	mkdir "$lock" # a busy install's lock is not ours to remove
	if /bin/bash "$repo/driver" --uninstall; then return 1; fi
	rmdir "$lock"
	grep -Fxq 'legacy CLI' "$old_bin"
	if grep -q '^start ' "$trace"; then return 1; fi
	rm "$HOME/legacy-user"
	touch "$legacy" "$executable"
	mkdir -p "$state/app"
	touch "$state/app/journal.sqlite"
	if SWIFT_RELAUNCH=yes /bin/bash "$repo/driver" --migrate-swift --origin localhost >"$output" 2>&1; then return 1; fi
	[[ -f "$HOME/Library/dev.beacon.agent.plist.disabled" && -f "$executable.disabled" && ! -e "$legacy" && ! -e "$executable" ]]
	grep -Fxq 'legacy CLI' "$old_bin"
	[[ $(grep -c '^archive$' "$trace") == 1 ]]
	[[ $(grep -c '^Archived jrn_' "$output") == 1 ]]
	if grep -q '^start ' "$trace"; then return 1; fi
	rm "$HOME/swift-running"
	/bin/bash "$repo/driver" --migrate-swift >"$output" 2>&1
	[[ $(grep -c '^verify$' "$trace") == 2 && -f "$HOME/serve-running" ]]
	if grep -q '^Archived ' "$output"; then return 1; fi
	/bin/bash "$repo/driver" >"$output" 2>&1
	[[ $(grep -c '^verify$' "$trace") == 3 && -f "$HOME/serve-running" ]]
	if grep -q '^Archived ' "$output"; then return 1; fi
	/bin/bash "$repo/driver" --uninstall
}

test_orphaned_act() {
	export OS=Darwin HOME="$tmp/home-orphaned-act" trace="$tmp/orphaned-act.trace"
	local state="$HOME/Library/Application Support/beacon"
	mkdir -p "$state"
	printf 'old client trust settings\n' > "$state/act.json"
	/bin/bash "$repo/driver" --origin localhost
	[[ -f "$HOME/serve-running" && ! -e "$state/swift-archive" ]]
	grep -Fxq 'old client trust settings' "$state/act.json"
	/bin/bash "$repo/driver" --uninstall
}

test_toolchain
test_install Darwin
test_install Linux
test_reissue Darwin
test_reissue Linux
test_stop_timeout stuck normal 'the approval instance lock is not available'
test_stop_timeout '' persistent 'the service manager could not confirm the job state'
test_legacy
test_orphaned_act
echo "ok temporary launchd and systemd installs, upgrades, rollback, code recovery and uninstall"
