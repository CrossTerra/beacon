package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlistGolden(t *testing.T) {
	got, err := Plist(Spec{
		Binary: "/Users/winton/.local/bin/beacon",
		Config: "/Users/winton/Library/Application Support/beacon/config.yaml",
		Stdout: "/Users/winton/Library/Application Support/beacon/state/service.stdout.log",
		Stderr: "/Users/winton/Library/Application Support/beacon/state/service.stderr.log",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>dev.beacon.agent</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/winton/.local/bin/beacon</string>
		<string>run</string>
		<string>--config</string>
		<string>/Users/winton/Library/Application Support/beacon/config.yaml</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ExitTimeOut</key>
	<integer>180</integer>
	<key>Umask</key>
	<integer>63</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>/Users/winton/Library/Application Support/beacon/state/service.stdout.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/winton/Library/Application Support/beacon/state/service.stderr.log</string>
</dict>
</plist>
`
	if string(got) != want {
		t.Fatalf("plist:\n%s", got)
	}
	for _, forbidden := range []string{"EnvironmentVariables", "UserName", "GroupName", "--once", "root", "SECRET", "token"} {
		if strings.Contains(string(got), forbidden) {
			t.Fatalf("plist contains %q", forbidden)
		}
	}
	if !strings.Contains(string(got), "\n\t\t<string>run</string>\n") {
		t.Fatal("plist does not run the poll loop")
	}
}

func TestPlistEscapesAndRejects(t *testing.T) {
	got, err := Plist(Spec{
		Binary: "/usr/local/bin/beacon",
		Config: "/tmp/a&b<c>.yaml",
		Stdout: "/tmp/state/out.log",
		Stderr: "/tmp/state/err.log",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "/tmp/a&amp;b&lt;c&gt;.yaml") {
		t.Fatalf("path not escaped:\n%s", got)
	}
	if strings.Contains(string(got), "a&b<c>") {
		t.Fatal("raw path leaked into the plist")
	}
	if _, err := Plist(Spec{Binary: "/bin/beacon", Config: "/tmp/bad\n.yaml", Stdout: "/tmp/o", Stderr: "/tmp/e"}); err == nil {
		t.Fatal("newline in a path was accepted")
	}
	if _, err := Plist(Spec{Binary: "beacon", Config: "/tmp/c", Stdout: "/tmp/o", Stderr: "/tmp/e"}); err == nil {
		t.Fatal("relative binary was accepted")
	}
}

func TestRotateAppendDescriptor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, StdoutName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(bytesRepeat('a', 100)); err != nil {
		t.Fatal(err)
	}
	if err := Rotate(path, 50); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil || string(backup) != string(bytesRepeat('a', 100)) {
		t.Fatalf("backup %q err %v", backup, err)
	}
	if _, err := f.Write([]byte("next")); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "next" {
		t.Fatalf("after truncate, append wrote %q", got)
	}
	info, err := os.Stat(path + ".1")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v err %v", info, err)
	}
	if err := Rotate(path, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal(err)
	}
}

func TestRotateSkipsSmallAndHuge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.log")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Rotate(path, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("small file was rotated")
	}
	if err := Rotate(filepath.Join(dir, "missing.log"), 1); err != nil {
		t.Fatal(err)
	}
}

func TestPassRoundTrip(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 10, 15, 4, 5, 0, time.UTC)
	in := Pass{At: at, Outcome: "ok", Lease: LeaseHeld, Holder: "0123456789abcdef"}
	if err := WritePass(dir, in); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, PassName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v err %v", info, err)
	}
	got, err := ReadPass(dir)
	if err != nil || !got.At.Equal(at) || got.Outcome != "ok" || got.Lease != LeaseHeld || got.Holder != in.Holder {
		t.Fatalf("%+v %v", got, err)
	}
	if err := WritePass(dir, Pass{At: at, Outcome: "ok\nkey", Lease: LeaseHeld, Holder: in.Holder}); err == nil {
		t.Fatal("outcome with a newline was written")
	}
	if err := os.WriteFile(filepath.Join(dir, PassName), []byte("{\"at\":\"2026-11-10T15:04:05Z\",\"outcome\":\"ok\\nfixture:chat:1\",\"lease\":\"held\",\"holder\":\"not-an-id\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPass(dir); err != ErrPass || strings.Contains(err.Error(), "fixture") {
		t.Fatalf("err %v", err)
	}
}

func TestParsePrint(t *testing.T) {
	running := ParsePrint(`gui/501/dev.beacon.agent = {
	state = running
	program = /Users/winton/.local/bin/beacon
	pid = 12345
	runs = 1
}`)
	if !running.Loaded || !running.Running || running.PID != 12345 || running.Program != "/Users/winton/.local/bin/beacon" {
		t.Fatalf("%+v", running)
	}
	waiting := ParsePrint("state = not running\n")
	if !waiting.Loaded || waiting.Running || waiting.PID != 0 {
		t.Fatalf("%+v", waiting)
	}
	if job := ParsePrint("Could not find service \"dev.beacon.agent\" in domain\n"); job.Loaded || job.Running {
		t.Fatalf("not loaded parsed as %+v", job)
	}
}

func TestParseIdentity(t *testing.T) {
	stable := ParseIdentity(`Identifier=dev.beacon.cli
CodeDirectory v=20400 size=1 flags=0x0(none) hashes=1
Authority=Beacon Local Signing
TeamIdentifier=not set
`)
	if !stable.Stable() || stable.Authority != "Beacon Local Signing" || stable.Identifier != "dev.beacon.cli" {
		t.Fatalf("%+v", stable)
	}
	adhoc := ParseIdentity("Identifier=beacon\nCodeDirectory v=20400 flags=0x2(adhoc) hashes=1\nSignature=adhoc\n")
	if adhoc.Stable() || !adhoc.AdHoc {
		t.Fatalf("%+v", adhoc)
	}
	if ParseIdentity("Identifier=beacon\n").Stable() {
		t.Fatal("unsigned identity counted as stable")
	}
}

func TestFormatStatus(t *testing.T) {
	at := time.Date(2026, 11, 10, 15, 4, 5, 0, time.UTC)
	got := FormatStatus(Job{Loaded: true, Running: true, PID: 42}, Pass{At: at, Outcome: "ok", Lease: LeaseHeld, Holder: "0123456789abcdef"}, nil)
	want := "loaded: yes\nrunning: yes\npid: 42\nlast_pass: 2026-11-10T15:04:05Z\noutcome: ok\nlease: held\nholder: 0123456789abcdef\n"
	if got != want {
		t.Fatalf("%q", got)
	}
	other := FormatStatus(Job{Loaded: true}, Pass{At: at, Outcome: "ok", Lease: LeaseOther}, nil)
	if !strings.Contains(other, "holder: another instance\n") || strings.Contains(other, "0123456789abcdef") {
		t.Fatalf("%s", other)
	}
	bad := FormatStatus(Job{}, Pass{}, ErrPass)
	if strings.Contains(bad, "fixture") || !strings.Contains(bad, "last_pass: unreadable\n") {
		t.Fatalf("%s", bad)
	}
	none := FormatStatus(Job{}, Pass{}, os.ErrNotExist)
	if !strings.Contains(none, "loaded: no\n") || !strings.Contains(none, "last_pass: none\n") {
		t.Fatalf("%s", none)
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
