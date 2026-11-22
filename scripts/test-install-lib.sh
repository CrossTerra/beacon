#!/bin/bash
# Exercise scripts/install-lib.sh with fake processes and a fake CLI: quitting through the socket, the SIGTERM
# fallback and a process that will not quit; the keychain repair's count and when it runs; the next steps for each
# state of the settings. No keychain, no real Beacon.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=scripts/install-lib.sh
source scripts/install-lib.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}"/beacon-install-lib.XXXXXXXX)
tmp=$(cd "$tmp" && pwd -P)
fake_pid=
cleanup() {
	if [[ -n "$fake_pid" ]]; then
		kill -KILL "$fake_pid" 2>/dev/null || true
		wait "$fake_pid" 2>/dev/null || true
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT
fail() {
	echo "test-install-lib: $*" >&2
	exit 1
}

# A stand-in for Beacon.app: a process whose command line ends in a marker unique to this test.
marker="beacon-fake-app-$$"
export BEACON_APP_PATTERN="$marker\$"
fake_app() { # $1: "term" quits on TERM; "stubborn" ignores it
	if [[ $1 == stubborn ]]; then
		/bin/bash -c 'trap "" TERM; while :; do sleep 0.1; done' "$marker" &
	else
		/bin/bash -c 'trap "exit 0" TERM; while :; do sleep 0.1; done' "$marker" &
	fi
	fake_pid=$!
	sleep 0.3
	[[ -n "$(beacon_pids)" ]] || fail "the fake app did not start"
}

# A fake CLI: `act quit` succeeds by stopping the app when FAKE_QUIT=socket, fails otherwise; `keychain list`
# prints FAKE_ITEMS lines; `keychain repair` records that it ran.
cli="$tmp/beacon"
cat > "$cli" <<SCRIPT
#!/bin/bash
case "\$1 \$2" in
"act quit")
	[[ "\${FAKE_QUIT:-}" == socket ]] || exit 1
	pkill -TERM -f "$marker\\\$"
	echo '{"status":"quitting"}' ;;
"keychain list") for ((i = 0; i < \${FAKE_ITEMS:-0}; i++)); do echo "source:item\$i"; done ;;
"keychain repair") echo repaired >> "$tmp/repairs"; [[ "\${FAKE_REPAIR_FAILS:-}" != yes ]] ;;
*) exit 2 ;;
esac
SCRIPT
chmod +x "$cli"

[[ $(quit_beacon "$cli" 2) == "not running" ]] || fail "quit with nothing running"
fake_app term
[[ $(FAKE_QUIT=socket quit_beacon "$cli" 2) == "quit through its socket" ]] || fail "the socket quit"
wait "$fake_pid"
fake_app term
[[ $(quit_beacon "$cli" 2) == "quit with SIGTERM" ]] || fail "the SIGTERM fallback"
wait "$fake_pid"
fake_app stubborn
if quit_beacon "$cli" 1 >/dev/null; then fail "reported a stubborn app as quit"; fi
kill -KILL "$fake_pid"
wait "$fake_pid" 2>/dev/null || true
fake_pid=
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
