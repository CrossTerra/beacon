#!/bin/bash
# Prove the quit deadlock, and that scheduling terminate on the run loop avoids it. A private bundle id, in a
# temporary directory. Never the installed Beacon.app, and no Apple event.
set -euo pipefail
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
tmp=$(mktemp -d "${TMPDIR:-/tmp}"/beacon-quit-deadlock.XXXXXXXX)
tmp=$(cd "$tmp" && pwd -P)
app="$tmp/QuitProbe.app"
pid=
cleanup() {
	if [[ -n "$pid" ]]; then
		kill -KILL "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT
fail() {
	echo "test-quit-deadlock: $*" >&2
	exit 1
}
[[ "$app" != "$HOME/Applications/Beacon.app" ]] || fail "fixture is the installed app"

mkdir -p "$app/Contents/MacOS"
cat > "$app/Contents/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>test.beacon.quit-deadlock</string>
<key>CFBundleName</key><string>QuitProbe</string>
<key>CFBundleExecutable</key><string>QuitProbe</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>LSUIElement</key><true/>
</dict></plist>
EOF
cat > "$tmp/main.swift" <<'SWIFT'
import AppKit
import Darwin
import Dispatch

final class Delegate: NSObject, NSApplicationDelegate {
    let mode: String
    init(_ mode: String) { self.mode = mode }

    func applicationDidFinishLaunching(_ notification: Notification) {
        fputs("ready \(mode)\n", stdout)
        fflush(stdout)
        if mode == "nested" {
            DispatchQueue.main.async { NSApp.terminate(nil) }
        } else {
            RunLoop.main.perform(inModes: [.default, .common, .modalPanel]) { NSApp.terminate(nil) }
        }
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        fputs("should-terminate\n", stdout)
        fflush(stdout)
        if mode == "nested" {
            // The bug: the reply is a main-queue block, and terminate is already inside one.
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) {
                fputs("main-queue-reply\n", stdout)
                fflush(stdout)
                NSApp.reply(toApplicationShouldTerminate: true)
            }
        } else {
            let timer = DispatchSource.makeTimerSource(queue: DispatchQueue.global(qos: .userInitiated))
            timer.schedule(deadline: .now() + 0.2)
            timer.setEventHandler {
                timer.cancel()
                RunLoop.main.perform(inModes: [.default, .common, .modalPanel]) {
                    fputs("runloop-reply\n", stdout)
                    fflush(stdout)
                    NSApp.reply(toApplicationShouldTerminate: true)
                }
            }
            timer.activate()
        }
        return .terminateLater
    }
}

let mode = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "nested"
let delegate = Delegate(mode)
let app = NSApplication.shared
app.setActivationPolicy(.accessory)
app.delegate = delegate
app.run()
SWIFT
xcrun swiftc "$tmp/main.swift" -o "$app/Contents/MacOS/QuitProbe" -framework AppKit

run_mode() { # $1 mode, $2 seconds; prints the output and returns 0 if the process exited
	local mode=$1 seconds=$2
	"$app/Contents/MacOS/QuitProbe" "$mode" > "$tmp/$mode.out" 2>"$tmp/$mode.err" &
	pid=$!
	local i
	for ((i = 0; i < seconds * 10; i++)); do
		if ! kill -0 "$pid" 2>/dev/null; then
			wait "$pid" || true
			pid=
			return 0
		fi
		sleep 0.1
	done
	return 1
}

if run_mode nested 2; then
	fail "terminate from a main-queue callback exited: $(cat "$tmp/nested.out")"
fi
grep -q 'should-terminate' "$tmp/nested.out" || fail "nested fixture did not reach terminate: $(cat "$tmp/nested.out")"
if grep -q 'main-queue-reply' "$tmp/nested.out"; then
	fail "a main-queue terminate drained the main-queue reply"
fi
kill -KILL "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true
pid=

if ! run_mode deferred 3; then
	fail "run-loop terminate did not exit: $(cat "$tmp/deferred.out")"
fi
grep -q 'runloop-reply' "$tmp/deferred.out" || fail "deferred terminate exited without the reply: $(cat "$tmp/deferred.out")"
identifier=$(/usr/bin/plutil -extract CFBundleIdentifier raw -o - "$app/Contents/Info.plist")
[[ "$identifier" == test.beacon.quit-deadlock ]] || fail "fixture bundle id was $identifier"
echo "ok quit deadlock: a main-queue terminate does not exit; a run-loop terminate does"
