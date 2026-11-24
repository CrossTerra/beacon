#!/bin/bash
# What scripts/install.sh does around the build: quitting a running Beacon, repairing the keychain items after a
# rebuild, and the next steps. Sourced; scripts/test-install-lib.sh exercises each with fakes.

# Same-user processes whose executable is $1 (device and inode, so a relative path or a symlink still matches) and,
# with $2, processes whose bundle id is $2. Prints "exe PID" and "bundle PID". Exit 2 if the inspection itself fails;
# an absent executable prints nothing and exits 0.
beacon_inspect() {
	local exe=$1 bundle=${2:-}
	/usr/bin/python3 - "$exe" "$bundle" <<'PY'
import ctypes, ctypes.util, os, subprocess, sys
exe, bundle = sys.argv[1], sys.argv[2]
if not os.path.exists(exe):
    sys.exit(0)
try:
    want = os.stat(exe)
except OSError:
    sys.exit(2)
libc = ctypes.CDLL(ctypes.util.find_library("c"))
proc_pidpath = libc.proc_pidpath
proc_pidpath.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_uint32]
proc_pidpath.restype = ctypes.c_int
uid = os.getuid()
buf = ctypes.create_string_buffer(4096)
try:
    listing = subprocess.check_output(["/bin/ps", "-axo", "pid=,uid="], text=True)
except (OSError, subprocess.CalledProcessError):
    sys.exit(2)
for line in listing.splitlines():
    parts = line.split()
    if len(parts) < 2 or int(parts[1]) != uid:
        continue
    pid = int(parts[0])
    n = proc_pidpath(pid, buf, len(buf))
    if n <= 0:
        continue
    path = os.fsdecode(buf.value)
    try:
        st = os.stat(path)
    except OSError:
        continue
    if (st.st_dev, st.st_ino) == (want.st_dev, want.st_ino):
        print(f"exe {pid}")
    if bundle and "/Contents/MacOS/" in path:
        plist = os.path.join(os.path.dirname(os.path.dirname(path)), "Info.plist")
        try:
            found = subprocess.check_output(
                ["/usr/bin/plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-", plist],
                text=True, stderr=subprocess.DEVNULL).strip()
        except (OSError, subprocess.CalledProcessError):
            continue
        if found == bundle:
            print(f"bundle {pid}")
PY
}

# PIDs of $1's Beacon executable, one per line. Exit 2 if inspection fails: that is not "not running".
beacon_pids() {
	local app=${1:?} exe=$1/Contents/MacOS/Beacon pid
	[[ -e "$exe" ]] || return 0
	local listing
	if ! listing=$(beacon_inspect "$exe"); then
		echo "Could not inspect processes for $app." >&2
		return 2
	fi
	while read -r kind pid; do
		[[ "$kind" == exe && -n "$pid" ]] && printf '%s\n' "$pid"
	done <<< "$listing"
	return 0
}

# True when pid still executes $1's Beacon binary. Call this immediately before signalling: a pid can be reused.
beacon_pid_matches() {
	local app=$1 pid=$2 found
	found=$(beacon_pids "$app" | awk -v want="$pid" '$0 == want { print; exit }')
	[[ "$found" == "$pid" ]]
}

# The proposal socket's listener, or nothing when it is not open. Exit 2 if the open socket cannot be identified.
# Tests replace this; a fixture must not connect to the installed app's socket.
beacon_socket_owner() {
	local sock=${BEACON_ACT_SOCKET:-$HOME/Library/Application Support/beacon/run/act.sock}
	[[ -S "$sock" ]] || return 0
	/usr/bin/python3 - "$sock" <<'PY'
import ctypes, socket, sys
LOCAL_PEERPID = 2
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
try:
    s.settimeout(2)
    s.connect(sys.argv[1])
except OSError:
    sys.exit(0)
pid = ctypes.c_int()
n = ctypes.c_uint(ctypes.sizeof(pid))
if ctypes.CDLL(None).getsockopt(s.fileno(), 0, LOCAL_PEERPID, ctypes.byref(pid), ctypes.byref(n)) != 0:
    sys.exit(2)
print(pid.value)
PY
}

# Waits until SECONDS reaches $2 for $1's Beacon to exit. Exit 2 if inspection fails; that is not "gone".
beacon_gone_until() {
	local app=$1 deadline=$2 pids
	while ((SECONDS < deadline)); do
		pids=$(beacon_pids "$app") || return 2
		[[ -z "$pids" ]] && return 0
		sleep 0.2
	done
	pids=$(beacon_pids "$app") || return 2
	[[ -z "$pids" ]]
}

# Kept separate so the installer fixtures never send a real Apple event.
beacon_osascript() {
	/usr/bin/osascript "$@"
}

# True when $1 is a signed dev.beacon.cli. $2 is the expected certificate SHA-1, or "-" for an ad-hoc test signature.
# A failure skips the helper; it is not executed.
cli_is_beacon() {
	local cli=$1 expect=$2 details id req
	if ! details=$(/usr/bin/codesign --verify --strict "$cli" 2>&1); then
		printf 'Socket quit skipped: %s does not verify (%s).\n' "$cli" "${details:-codesign failed}" >&2
		return 1
	fi
	id=$(/usr/bin/codesign -d --verbose=1 "$cli" 2>&1 | sed -n 's/^Identifier=//p')
	if [[ "$id" != dev.beacon.cli ]]; then
		printf 'Socket quit skipped: %s is %s, not dev.beacon.cli.\n' "$cli" "${id:-unsigned}" >&2
		return 1
	fi
	if [[ -n "$expect" && "$expect" != "-" ]]; then
		req=$(/usr/bin/codesign -d -r- "$cli" 2>&1 | tr '[:lower:]' '[:upper:]')
		if [[ "$req" != *"CERTIFICATE LEAF = H\"$(printf '%s' "$expect" | tr '[:lower:]' '[:upper:]')\""* ]]; then
			printf 'Socket quit skipped: %s is not signed by the expected certificate.\n' "$cli" >&2
			return 1
		fi
	fi
}

# Quits the Beacon at $1. $2 is the overall wait in seconds, $3 the bundle id, $4 the CLI's expected certificate
# SHA-1 ("-" allows the ad-hoc signature the fixtures use). The staged helper is never run: an old app admits only
# the requirement it stored at launch, and a new certificate cannot change that. Prints how it quit, including a
# successful socket reply. A failed process inspection is not treated as the app having exited.
quit_beacon() {
	local app="$1" cli="$1/Contents/Helpers/beacon" wait="${2:-30}" bundle_id="${3:-dev.beacon.app}" expect=${4:-}
	local pids reply owner pid signalled=0 deadline remain instances
	if [[ -n "${BEACON_INSTALL_TEST:-}" && "$app" == "$HOME/Applications/Beacon.app" ]]; then
		echo "refusing to quit the installed Beacon.app from a test" >&2
		return 1
	fi
	[[ "$bundle_id" =~ ^[A-Za-z0-9._-]+$ ]] || {
		echo "refusing bundle id $bundle_id" >&2
		return 1
	}
	if ! pids=$(beacon_pids "$app"); then
		return 1
	fi
	if [[ -z "$pids" ]]; then
		echo "not running"
		return 0
	fi
	if [[ ! -f "$cli" || ! -x "$cli" ]]; then
		printf 'Socket quit skipped: no executable installed CLI at %s.\n' "$cli" >&2
	elif ! cli_is_beacon "$cli" "$expect"; then
		:
	elif ! owner=$(beacon_socket_owner); then
		echo "Socket quit skipped: the proposal socket is open but its listener could not be identified." >&2
	elif [[ -n "$owner" ]] && ! beacon_pid_matches "$app" "$owner"; then
		printf 'Socket quit skipped: the proposal socket belongs to pid %s, not this Beacon.\n' "$owner" >&2
	elif reply=$("$cli" act quit 2>&1); then
		printf 'socket reply: %s\n' "$reply" >&2
		deadline=$((SECONDS + wait))
		if beacon_gone_until "$app" "$deadline"; then
			printf 'quit through its socket (%s)\n' "$reply"
			return 0
		fi
		printf 'Socket quit returned success, but Beacon did not exit (%s).\n' "${reply:-no reply}" >&2
	else
		printf 'Socket quit via %s failed (exit %d): %s\n' "$cli" "$?" "${reply:-no output}" >&2
	fi
	if ! pids=$(beacon_pids "$app"); then
		return 1
	fi
	if [[ -z "$pids" ]]; then
		echo "exited during the socket quit attempt"
		return 0
	fi
	while read -r pid; do
		[[ -n "$pid" ]] || continue
		if ! beacon_pid_matches "$app" "$pid"; then
			printf 'Not signalling %s: it is no longer this Beacon.\n' "$pid" >&2
			continue
		fi
		if kill -TERM "$pid"; then
			signalled=1
		else
			printf 'SIGTERM of %s failed.\n' "$pid" >&2
		fi
	done <<< "$pids"
	deadline=$((SECONDS + wait))
	if beacon_gone_until "$app" "$deadline"; then
		if [[ "$signalled" -eq 1 ]]; then
			echo "quit with SIGTERM"
		else
			echo "exited during the SIGTERM attempt"
		fi
		return 0
	fi
	if [[ "$signalled" -eq 1 ]]; then
		echo "SIGTERM was sent, but Beacon did not exit." >&2
	fi
	if ! instances=$(beacon_inspect "$app/Contents/MacOS/Beacon" "$bundle_id"); then
		echo "AppleScript quit skipped: could not tell how many copies are running." >&2
		return 1
	fi
	if [[ "$(awk '$1 == "bundle" { n++ } END { print n + 0 }' <<< "$instances")" -ne 1 ]]; then
		echo "AppleScript quit skipped: bundle id $bundle_id is not exactly one process. Quit that copy from its menu." >&2
		return 1
	fi
	deadline=$((SECONDS + wait))
	remain=$wait
	echo "Trying AppleScript to quit the one $bundle_id; macOS may ask for Automation permission." >&2
	# One deadline covers the Apple event and the exit wait. The running check avoids launching a copy.
	if reply=$(beacon_osascript -e "with timeout of $remain seconds" \
		-e "if application id \"$bundle_id\" is running then quit app id \"$bundle_id\"" \
		-e 'end timeout' 2>&1); then
		if beacon_gone_until "$app" "$deadline"; then
			echo "quit with AppleScript"
			return 0
		fi
		echo "AppleScript returned success, but Beacon did not exit before the deadline." >&2
	else
		printf 'AppleScript quit failed (exit %d): %s\n' "$?" "${reply:-no output}" >&2
		if pids=$(beacon_pids "$app") && [[ -z "$pids" ]]; then
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
