#!/usr/bin/env bash
# What scripts/install.sh does around the build: quitting a running Beacon, repairing the keychain items after a
# rebuild, and the next steps. Sourced; scripts/test-install-lib.sh exercises each with fakes.

# The running Beacon.app's process ids, one per line. BEACON_APP_PATTERN replaces the pattern in tests.
beacon_pids() {
	pgrep -U "$(id -u)" -f "${BEACON_APP_PATTERN:-/Beacon\.app/Contents/MacOS/Beacon$}" || true
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

# Quits a running Beacon as its menu's Quit does: its services stop and approvals not yet started expire. First
# through its socket with the app's CLI ($1, `beacon act quit`, which only that CLI may ask), then with SIGTERM.
# Neither needs permission to control another app. $2 is how long to wait for each, in seconds (Quit itself waits
# at most 23 for its services). Prints how it quit; returns 1 if Beacon is still running.
quit_beacon() {
	local cli=$1 wait=${2:-30} pids
	pids=$(beacon_pids)
	if [[ -z "$pids" ]]; then
		echo "not running"
		return 0
	fi
	if "$cli" act quit >/dev/null 2>&1 && beacon_gone_within "$wait"; then
		echo "quit through its socket"
		return 0
	fi
	pids=$(beacon_pids)
	if [[ -n "$pids" ]]; then
		# shellcheck disable=SC2086
		kill -TERM $pids 2>/dev/null || true
	fi
	if beacon_gone_within "$wait"; then
		echo "quit with SIGTERM"
		return 0
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
