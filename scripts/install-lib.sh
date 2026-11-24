#!/bin/bash
# What scripts/install.sh does around the build: quitting a running Beacon, repairing the keychain items after a
# rebuild, and the next steps. Sourced; scripts/test-install-lib.sh exercises each with fakes.

# The installed bundle whose executable this installer may signal. Tests set it; a real install uses ~/Applications.
beacon_app_path() {
	printf '%s' "${BEACON_APP_PATH:-$HOME/Applications/Beacon.app}"
}

# PIDs of that bundle's executable, one per line. The command must be that path, not a regex and not a lookalike
# whose arguments merely contain it. A staged .Beacon.app.new is a different path and is not this app.
beacon_pids() {
	local want pid command
	want="$(beacon_app_path)/Contents/MacOS/Beacon"
	[[ -n "$want" ]] || return 0
	/bin/ps -axww -o pid=,command= | while read -r pid command; do
		case "$command" in
		"$want"|"$want "*) printf '%s\n' "$pid" ;;
		esac
	done
}

# Waits up to $1 seconds for Beacon to exit. Returns 0 once it has.
beacon_gone_within() {
	local i
	for ((i = 0; i < $1 * 5; i++)); do
		[[ -z "$(beacon_pids)" ]] && return 0
		sleep 0.2
	done
	[[ -z "$(beacon_pids)" ]]
}

# Kept separate so the installer fixtures never send a real Apple event.
beacon_osascript() {
	/usr/bin/osascript "$@"
}

# Quits the running Beacon: use the CLI in the installed bundle ($1), not the staged CLI, so a new signature or
# CLI version cannot prevent socket admission. The app can still refuse a caller it cannot verify as external.
# Fall back to SIGTERM and finally AppleScript. $2 is how long to wait for exit after each request (Quit itself
# waits at most 23 seconds for services). Print the outcome on stdout and why a path failed on stderr.
quit_beacon() {
	local app=$1 cli="$1/Contents/Helpers/beacon" wait=${2:-30} pids reply term_status=0
	# Only this bundle's executable is signaled, even if the caller exported another BEACON_APP_PATH.
	local BEACON_APP_PATH=$app
	pids=$(beacon_pids)
	if [[ -z "$pids" ]]; then
		echo "not running"
		return 0
	fi
	if [[ ! -f "$cli" || ! -x "$cli" ]]; then
		printf 'Socket quit skipped: no executable installed CLI at %s.\n' "$cli" >&2
	elif reply=$("$cli" act quit 2>&1); then
		if beacon_gone_within "$wait"; then
			echo "quit through its socket"
			return 0
		fi
		printf 'Socket quit returned success, but Beacon did not exit within %s seconds (%s).\n' "$wait" "${reply:-no reply}" >&2
	else
		printf 'Socket quit via %s failed (exit %d): %s\n' "$cli" "$?" "${reply:-no output}" >&2
	fi
	pids=$(beacon_pids)
	if [[ -z "$pids" ]]; then
		echo "exited during the socket quit attempt"
		return 0
	fi
	# One argument per PID from pgrep. Even if one signal fails, another may have been sent.
	# shellcheck disable=SC2086
	reply=$(kill -TERM $pids 2>&1) || term_status=$?
	if [[ "$term_status" -ne 0 ]]; then
		printf 'SIGTERM failed (exit %d): %s\n' "$term_status" "${reply:-no output}" >&2
	fi
	if beacon_gone_within "$wait"; then
		if [[ "$term_status" -eq 0 ]]; then
			echo "quit with SIGTERM"
		else
			echo "exited during the SIGTERM attempt"
		fi
		return 0
	fi
	if [[ "$term_status" -eq 0 ]]; then
		printf 'SIGTERM was sent, but Beacon did not exit within %s seconds.\n' "$wait" >&2
	fi

	echo "Trying AppleScript to quit Beacon; macOS may ask for Automation permission." >&2
	# Check again inside the script so a late exit does not turn the quit event into a launch.
	if reply=$(beacon_osascript -e "with timeout of $wait seconds" \
		-e 'if application id "dev.beacon.app" is running then quit app id "dev.beacon.app"' \
		-e 'end timeout' 2>&1); then
		if beacon_gone_within "$wait"; then
			echo "quit with AppleScript"
			return 0
		fi
		printf 'AppleScript returned success, but Beacon did not exit within %s seconds.\n' "$wait" >&2
	else
		printf 'AppleScript quit failed (exit %d): %s\n' "$?" "${reply:-no output}" >&2
		if [[ -z "$(beacon_pids)" ]]; then
			echo "exited during the AppleScript attempt"
			return 0
		fi
	fi
	return 1
}

# Whether the signing identity's name is a certificate Apple issued. A self-signed one ("Beacon Local Signing")
# makes macOS tie each keychain item to the exact build, so every rebuild asks again.
apple_issued() {
	[[ "$1" =~ ^(Apple\ Development|Apple\ Distribution|Developer\ ID\ Application|Mac\ Developer):\  ]]
}

# The Xcode steps for an Apple Development certificate.
apple_development_steps() {
	cat <<'EOF'
To stop these prompts on every rebuild, sign with an Apple Development certificate instead (free with an Apple ID):
  1. Open Xcode → Settings… (⌘,) → Accounts.
  2. Click +, choose Apple ID and sign in. Your account appears with a "(Personal Team)".
  3. Select that team, click Manage Certificates…, then + → Apple Development, and close the sheet.
  4. Run: security find-identity -v -p codesigning   (it now lists "Apple Development: …")
  5. Run: scripts/install.sh --identity "Apple Development: …"   with the name it printed.
Changing the certificate once needs a few extra steps (the app's act.json and vault key): see docs/signing.md,
"Changing the certificate". After that, rebuilds ask nothing.
EOF
}

# Re-creates the collector's keychain items with the new CLI ($1) so the collector reads them without asking:
# run after Beacon has quit (no collector reading at the same time) and before it opens again. $2 is "self" for a
# self-signed identity (needed after every rebuild), "apple" (needed only after a certificate change), or "force".
# Prints what it did; a failed repair is reported and left to the person.
repair_keychain() {
	local cli=$1 kind=$2 accounts count
	if [[ "$kind" == apple ]]; then
		echo "Keychain: signed by Apple Development, so the collector's items stay readable without a prompt."
		echo "(After switching certificates, run scripts/install.sh --repair once.)"
		return 0
	fi
	if ! accounts=$("$cli" keychain list 2>/dev/null); then
		echo "Keychain: could not list Beacon's items; run \`beacon keychain repair\` yourself if the collector asks."
		return 0
	fi
	count=$(grep -c . <<<"$accounts" || true)
	if [[ "$count" -eq 0 ]]; then
		echo "Keychain: no collector items to repair."
		return 0
	fi
	cat <<EOF

Keychain: re-creating the collector's $count item(s) for this build, now that Beacon has quit, so nothing reads
them at the same time. macOS will ask for your login keychain password, up to twice per item (to read it, and
to replace it): about $((count * 2)) prompts. Choose Always Allow and enter your login password each time.
Why: with a self-signed certificate macOS ties each item to the exact build, so a rebuild must be allowed again.
EOF
	if "$cli" keychain repair; then
		echo "Keychain: repaired $count item(s); the collector reads them without asking until the next rebuild."
	else
		echo "Keychain: the repair stopped. Run \`beacon keychain repair\` again; it finishes where it stopped." >&2
	fi
	if [[ "$kind" == self ]]; then
		echo
		apple_development_steps
	fi
}

# The steps Settings still needs, from Beacon.app's settings ($1): GitHub when no account is set, Signal when it is
# missing or invalid (no person, or the person is the app itself). Prints nothing for a step already done.
next_steps() {
	local settings=$1 n=1 person actor
	if ! /usr/bin/plutil -extract github.account raw -o - "$settings" >/dev/null 2>&1; then
		cat <<EOF
  $n. Beacon's menu → Settings… → GitHub: "Create a token on GitHub" (classic, repo scope), then paste it. If
     your organization uses SAML SSO, click Configure SSO → Authorize next to the new token.
EOF
		n=$((n + 1))
	fi
	# plutil prints its errors on standard output: only its status says whether a value was read.
	person=$(/usr/bin/plutil -extract signal.person raw -o - "$settings" 2>/dev/null) || person=""
	actor=$(/usr/bin/plutil -extract signal.actor raw -o - "$settings" 2>/dev/null) || actor=""
	if [[ -z "$person" ]]; then
		echo "  $n. Settings… → Signal: paste the Beacon setup code from Signal's /beacon page."
		n=$((n + 1))
	elif [[ "$person" != act_* || "$person" == "$actor" ]]; then
		echo "  $n. Settings… → Signal: the saved settings name the app as the person. Paste the setup code again."
		n=$((n + 1))
	fi
	echo "  $n. Approvals… (⌥⌘B) shows proposals as they arrive. Beacon is the beacon-light icon in the menu bar,"
	echo "     with a number while proposals wait; opening Beacon.app again also shows the window."
}
