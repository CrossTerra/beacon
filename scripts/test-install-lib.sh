#!/bin/bash
# Exercise scripts/install-lib.sh with fake processes, a fake installed CLI and fake AppleScript: the ordered quit
# paths and their diagnostics, the keychain repair count and when it runs, and the next steps for each state of the
# settings. No keychain, no real Beacon, no Apple events.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=scripts/install-lib.sh
source scripts/install-lib.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}"/beacon-install-lib.XXXXXXXX)
tmp=$(cd "$tmp" && pwd -P)
fake_pid=
stop_fake_app() {
	if [[ -n "$fake_pid" ]]; then
		kill -KILL "$fake_pid" 2>/dev/null || true
		wait "$fake_pid" 2>/dev/null || true
		fake_pid=
	fi
}
cleanup() {
	stop_fake_app
	rm -rf "$tmp"
}
trap cleanup EXIT
fail() {
	echo "test-install-lib: $*" >&2
	exit 1
}

# A stand-in whose executable path is exactly Beacon.app/Contents/MacOS/Beacon. A copy at another path, and a
# process that only mentions that path, must not count.
app="$tmp/Test Applications/Beacon.app"
export BEACON_APP_PATH="$app"
exe="$app/Contents/MacOS/Beacon"
mkdir -p "${exe%/*}"
# A binary we compile, not a copy of /bin/bash: a platform binary copied off its path is killed.
cat > "$tmp/fake-beacon.c" <<'EOF'
#include <signal.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>
static volatile sig_atomic_t stop;
static void finish(int sig) { (void)sig; stop = 1; }
int main(int argc, char **argv) {
    FILE *ready;
    if (argc < 3) return 2;
    signal(SIGTERM, strcmp(argv[2], "stubborn") == 0 ? SIG_IGN : finish);
    signal(SIGUSR1, finish);
    ready = fopen(argv[1], "w");
    if (ready) fclose(ready);
    while (!stop) pause();
    return 0;
}
EOF
/usr/bin/cc -o "$exe" "$tmp/fake-beacon.c"
fake_app() { # $1: "term" quits on TERM; "stubborn" ignores it
	local i
	rm -f "$tmp/ready"
	# USR1 stands for the app handling a socket or an AppleScript quit, distinct from the installer's SIGTERM.
	"$exe" "$tmp/ready" "$1" &
	fake_pid=$!
	for ((i = 0; i < 40; i++)); do
		[[ -f "$tmp/ready" ]] && break
		sleep 0.05
	done
	[[ -f "$tmp/ready" && "$(beacon_pids)" == "$fake_pid" ]] || fail "the fake app did not start: $(beacon_pids)"
}

# A fake installed CLI, with a space in the bundle path. `act quit` can stop the app, return a reply without
# exiting, or be refused; `keychain list` prints FAKE_ITEMS lines; `keychain repair` records that it ran.
cli="$app/Contents/Helpers/beacon"
mkdir -p "${cli%/*}"
cat > "$cli" <<SCRIPT
#!/bin/bash
case "\$1 \$2" in
"act quit")
	printf 'socket:%s\n' "\$0" >> "$tmp/quit-paths"
	case "\${FAKE_QUIT:-}" in
	socket) kill -USR1 "\$FAKE_PID" || exit 1; echo '{"status":"quitting"}' ;;
	ack) echo '{"status":"quitting"}' ;;
	unadmitted) echo 'beacon: the app closed the connection without a reply' >&2; exit 1 ;;
	*) echo '{"error":"invalid","field":"/op"}'; exit 1 ;;
	esac ;;
"keychain list") for ((i = 0; i < \${FAKE_ITEMS:-0}; i++)); do echo "source:item\$i"; done ;;
"keychain repair") echo repaired >> "$tmp/repairs"; [[ "\${FAKE_REPAIR_FAILS:-}" != yes ]] ;;
*) exit 2 ;;
esac
SCRIPT
chmod +x "$cli"

# Record SIGTERM but send it to the fake app for real. The AppleScript stub must never call the system tool.
kill() {
	if [[ $1 == -TERM ]]; then
		echo SIGTERM >> "$tmp/quit-paths"
		if [[ ${FAKE_TERM_ERROR:-} == yes ]]; then
			echo "kill: $2: Operation not permitted" >&2
			return 1
		fi
	fi
	builtin kill "$@"
}
beacon_osascript() {
	echo AppleScript >> "$tmp/quit-paths"
	if [[ $# -ne 6 || $1 != -e || $2 != 'with timeout of '* || $3 != -e ||
		$4 != 'if application id "dev.beacon.app" is running then quit app id "dev.beacon.app"' ||
		$5 != -e || $6 != 'end timeout' ]]; then
		echo "unexpected AppleScript: $*" >&2
		return 2
	fi
	case ${FAKE_AUTOMATION:-} in
	quit) kill -USR1 "$fake_pid" ;;
	ack) return 0 ;;
	*) echo 'Not authorized to send Apple events to Beacon. (-1743)' >&2; return 1 ;;
	esac
}
try_quit() {
	: > "$tmp/quit-paths"
	FAKE_PID="$fake_pid" quit_beacon "$app" "${1:-2}" 2> "$tmp/quit-details"
}
expect_paths() {
	local seen
	seen=$(cat "$tmp/quit-paths")
	[[ "$seen" == "$1" ]] || fail "expected quit paths '$1', got '$seen'"
}
expect_detail() {
	grep -Fq -- "$1" "$tmp/quit-details" || fail "missing quit diagnostic '$1': $(cat "$tmp/quit-details")"
}
reap_fake_app() {
	wait "$fake_pid"
	fake_pid=
}

[[ $(try_quit) == "not running" ]] || fail "quit with nothing running"
expect_paths ""
socket="socket:$cli"
fake_app stubborn
staged="$tmp/.Beacon.app.new/Contents/MacOS/Beacon"
mkdir -p "${staged%/*}"
cp "$exe" "$staged"
"$staged" "$tmp/staged-ready" stubborn &
staged_pid=$!
for ((i = 0; i < 40; i++)); do
	[[ -f "$tmp/staged-ready" ]] && break
	sleep 0.05
done
[[ -f "$tmp/staged-ready" ]] || fail "the staged lookalike did not start"
# argv mentions the real executable, but this process is not it. disown keeps Bash from announcing the kill.
/bin/bash -c 'sleep 30' _ "$exe" &
mention_pid=$!
disown "$mention_pid" 2>/dev/null || true
sleep 0.1
[[ "$(beacon_pids)" == "$fake_pid" ]] || fail "not exactly the bundle executable: $(/bin/ps -p "$fake_pid,$staged_pid,$mention_pid" -o pid=,command=)"
kill -KILL "$staged_pid" "$mention_pid" 2>/dev/null || true
wait "$staged_pid" 2>/dev/null || true
wait "$mention_pid" 2>/dev/null || true
[[ $(FAKE_QUIT=socket try_quit) == "quit through its socket" ]] || fail "the installed CLI's socket quit"
expect_paths "$socket"
reap_fake_app

fake_app term
[[ $(FAKE_QUIT=unadmitted try_quit) == "quit with SIGTERM" ]] || fail "the SIGTERM fallback"
expect_paths "$socket"$'\nSIGTERM'
expect_detail 'the app closed the connection without a reply'
reap_fake_app

fake_app term
[[ $(FAKE_QUIT=ack try_quit 1) == "quit with SIGTERM" ]] || fail "the socket reply alone was treated as exit"
expect_paths "$socket"$'\nSIGTERM'
expect_detail 'Socket quit returned success, but Beacon did not exit within 1 seconds'
reap_fake_app

fake_app stubborn
[[ $(FAKE_AUTOMATION=quit try_quit 1) == "quit with AppleScript" ]] || fail "the AppleScript fallback"
expect_paths "$socket"$'\nSIGTERM\nAppleScript'
expect_detail '{"error":"invalid","field":"/op"}'
expect_detail 'SIGTERM was sent, but Beacon did not exit within 1 seconds'
expect_detail 'macOS may ask for Automation permission'
reap_fake_app

fake_app stubborn
if try_quit 1 >/dev/null; then fail "reported a stubborn app as quit after Automation was refused"; fi
expect_paths "$socket"$'\nSIGTERM\nAppleScript'
expect_detail 'AppleScript quit failed (exit 1): Not authorized to send Apple events to Beacon. (-1743)'
if FAKE_TERM_ERROR=yes FAKE_AUTOMATION=ack try_quit 1 >/dev/null; then fail "reported a stubborn app as quit"; fi
expect_paths "$socket"$'\nSIGTERM\nAppleScript'
expect_detail 'Operation not permitted'
expect_detail 'AppleScript returned success, but Beacon did not exit within 1 seconds'
stop_fake_app

fake_app term
chmod -x "$cli"
[[ $(try_quit) == "quit with SIGTERM" ]] || fail "missing installed CLI did not fall back to SIGTERM"
chmod +x "$cli"
expect_paths SIGTERM
expect_detail "no executable installed CLI at $cli"
reap_fake_app
echo "ok quit_beacon"

apple_issued "Apple Development: Winton Welsh (ABCDE12345)" || fail "Apple Development not recognised"
if apple_issued "Beacon Local Signing"; then fail "a self-signed identity counted as Apple's"; fi
if apple_issued "Apple Development"; then fail "a bare name counted as Apple's"; fi
echo "ok apple_issued"

out=$(FAKE_ITEMS=3 repair_keychain "$cli" self)
[[ $(wc -l < "$tmp/repairs") -eq 1 ]] || fail "a self-signed build was not repaired"
if ! grep -q "3 item(s)" <<<"$out" || ! grep -q "about 6 prompts" <<<"$out"; then fail "the prompt count: $out"; fi
grep -q "Manage Certificates" <<<"$out" || fail "no Apple Development steps for a self-signed build"
out=$(FAKE_ITEMS=3 repair_keychain "$cli" apple)
[[ $(wc -l < "$tmp/repairs") -eq 1 ]] || fail "an Apple Development build was repaired without --repair"
out=$(FAKE_ITEMS=2 repair_keychain "$cli" force)
[[ $(wc -l < "$tmp/repairs") -eq 2 ]] || fail "--repair did not repair"
if grep -q "Manage Certificates" <<<"$out"; then fail "Apple Development steps after --repair"; fi
out=$(FAKE_ITEMS=0 repair_keychain "$cli" self)
if [[ $(wc -l < "$tmp/repairs") -ne 2 ]] || ! grep -q "no collector items" <<<"$out"; then fail "repaired with no items"; fi
out=$(FAKE_ITEMS=1 FAKE_REPAIR_FAILS=yes repair_keychain "$cli" self 2>&1) || fail "a failed repair stopped the install"
grep -q "Run \`beacon keychain repair\` again" <<<"$out" || fail "a failed repair was not reported"
echo "ok repair_keychain"

settings="$tmp/settings.json"
steps() { next_steps "$settings"; }
rm -f "$settings"
out=$(steps)
if ! grep -q "GitHub" <<<"$out" || ! grep -q "paste the Beacon setup code" <<<"$out"; then fail "missing settings: $out"; fi
printf '%s' '{"version":1,"github":{"origin":"https://api.github.com","account":"github:user:1","login":"w"}}' > "$settings"
out=$(steps)
if grep -q "GitHub" <<<"$out"; then fail "a configured GitHub still asked: $out"; fi
grep -q "  1. Settings… → Signal" <<<"$out" || fail "Signal not first: $out"
printf '%s' '{"version":1,"github":{"origin":"https://api.github.com","account":"github:user:1","login":"w"},
  "signal":{"origin":"https://s.test","person":"act_01m6a6ft00e40r2081040g2081","actor":"act_01m6a6ft00e81840g2081040g3","handle":"b"}}' > "$settings"
out=$(steps)
if grep -q "Signal" <<<"$out"; then fail "a valid Signal still asked: $out"; fi
grep -q "  1. Approvals" <<<"$out" || fail "nothing left but Approvals: $out"
printf '%s' '{"version":1,"signal":{"origin":"https://s.test","person":"act_01m6a6ft00e81840g2081040g3","actor":"act_01m6a6ft00e81840g2081040g3","handle":"b"}}' > "$settings"
out=$(steps)
grep -q "name the app as the person" <<<"$out" || fail "an invalid Signal not flagged: $out"
echo "ok next_steps"
