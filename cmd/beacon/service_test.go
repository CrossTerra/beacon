package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/service"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
)

type fakeLaunch struct {
	mu       sync.Mutex
	calls    []string
	printOut string
	printErr error
	bootErr  error
}

func (f *fakeLaunch) Bootstrap(_ context.Context, domain, plist string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "bootstrap "+domain+" "+plist)
	return f.bootErr
}

func (f *fakeLaunch) Bootout(_ context.Context, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "bootout "+target)
	return nil
}

func (f *fakeLaunch) Enable(_ context.Context, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "enable "+target)
	return nil
}

func (f *fakeLaunch) Print(_ context.Context, target string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "print "+target)
	return f.printOut, f.printErr
}

func (f *fakeLaunch) joined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

// serviceApp is an app whose service commands cannot touch the real LaunchAgents directory or launchd.
func serviceApp(t *testing.T, h *harness, agents string, fl *fakeLaunch, signed bool) *app {
	t.Helper()
	a := h.app(nil)
	a.agentsDir = func() (string, error) { return agents, nil }
	a.launchctl = fl
	a.uid = func() int { return 501 }
	a.identify = func(context.Context, string) (service.Identity, error) {
		if !signed {
			return service.Identity{AdHoc: true, Identifier: "beacon"}, nil
		}
		return service.Identity{Authority: "Beacon Local Signing", Identifier: "dev.beacon.cli"}, nil
	}
	return a
}

func testBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "beacon")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServiceInstallPlistAndRefusals(t *testing.T) {
	secret := "sekret-from-env-must-not-appear"
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)

	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "dry-run", "svc")
	agents := t.TempDir()
	bin := testBinary(t)
	fl := &fakeLaunch{}
	a := serviceApp(t, h, agents, fl, true)
	if code := a.main(context.Background(), []string{"service", "install", "--config", cfg, "--binary", bin}); code != 0 {
		t.Fatalf("install: %d\n%s", code, h.stderr.String())
	}
	plistPath := filepath.Join(agents, service.PlistName)
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(plistPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plist mode %v", info)
	}
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "EnvironmentVariables") || strings.Contains(string(data), "--once") || strings.Contains(string(data), "UserName") {
		t.Fatalf("plist:\n%s", data)
	}
	if !strings.Contains(string(data), "\n\t\t<string>run</string>\n") || !strings.Contains(string(data), service.StdoutName) {
		t.Fatalf("plist:\n%s", data)
	}
	calls := fl.joined()
	if !strings.Contains(calls, "bootstrap gui/501 "+plistPath) || !strings.Contains(calls, "enable gui/501/"+service.Label) {
		t.Fatalf("calls:\n%s", calls)
	}
	state := filepath.Join(dir, "state")
	if info, err := os.Stat(state); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("state mode %v err %v", info, err)
	}
	for _, name := range []string{service.StdoutName, service.StderrName} {
		info, err := os.Stat(filepath.Join(state, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v err %v", name, info, err)
		}
	}

	// A second install does not bootstrap again.
	fl2 := &fakeLaunch{}
	a = serviceApp(t, h, agents, fl2, true)
	if code := a.main(context.Background(), []string{"service", "install", "--config", cfg, "--binary", bin}); code == 0 || !strings.Contains(h.stderr.String(), "--force") {
		t.Fatalf("second install: %d\n%s", code, h.stderr.String())
	}
	if fl2.joined() != "" {
		t.Fatalf("second install called launchd:\n%s", fl2.joined())
	}

	// Ad-hoc signature: nothing written, nothing bootstrapped. Use a fresh agents dir.
	agents2 := t.TempDir()
	fl3 := &fakeLaunch{}
	a = serviceApp(t, h, agents2, fl3, false)
	if code := a.main(context.Background(), []string{"service", "install", "--config", cfg, "--binary", bin}); code == 0 || !strings.Contains(h.stderr.String(), "scripts/build-local.sh") {
		t.Fatalf("adhoc: %d\n%s", code, h.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(agents2, service.PlistName)); !os.IsNotExist(err) || fl3.joined() != "" {
		t.Fatalf("adhoc wrote a plist or called launchd: %v\n%s", err, fl3.joined())
	}

	// Root is refused before launchd.
	fl4 := &fakeLaunch{}
	a = serviceApp(t, h, t.TempDir(), fl4, true)
	a.euid = func() int { return 0 }
	if code := a.main(context.Background(), []string{"service", "install", "--config", cfg, "--binary", bin}); code == 0 || !strings.Contains(h.stderr.String(), "refusing to run as root") {
		t.Fatalf("root: %d\n%s", code, h.stderr.String())
	}
	if fl4.joined() != "" {
		t.Fatal("root called launchd")
	}
}

func TestServiceUninstallStopsAndRemoves(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "dry-run", "un")
	agents := t.TempDir()
	fl := &fakeLaunch{}
	a := serviceApp(t, h, agents, fl, true)
	bin := testBinary(t)
	if code := a.main(context.Background(), []string{"service", "install", "--config", cfg, "--binary", bin}); code != 0 {
		t.Fatal(h.stderr.String())
	}
	state := filepath.Join(dir, "state")
	if err := os.WriteFile(filepath.Join(state, "outbox.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fl = &fakeLaunch{}
	a = serviceApp(t, h, agents, fl, true)
	if code := a.main(context.Background(), []string{"service", "uninstall"}); code != 0 {
		t.Fatalf("uninstall: %d\n%s", code, h.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(agents, service.PlistName)); !os.IsNotExist(err) {
		t.Fatal("plist still there")
	}
	if _, err := os.Stat(filepath.Join(state, "outbox.jsonl")); err != nil {
		t.Fatal("uninstall deleted state")
	}
	if !strings.Contains(fl.joined(), "bootout gui/501/"+service.Label) {
		t.Fatalf("calls:\n%s", fl.joined())
	}
	if !strings.Contains(h.stdout.String(), "Stopped and removed") {
		t.Fatalf("stdout: %s", h.stdout.String())
	}
	// Idempotent.
	if code := a.main(context.Background(), []string{"service", "uninstall"}); code != 0 || !strings.Contains(h.stdout.String(), "not installed") {
		t.Fatalf("second uninstall: %d\n%s", code, h.stdout.String())
	}
}

func TestServiceStatusParsing(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "dry-run", "st")
	fl := &fakeLaunch{printOut: "gui/501/" + service.Label + " = {\n\tstate = running\n\tpid = 4242\n\tprogram = /Users/winton/.local/bin/beacon\n}\n"}
	a := serviceApp(t, h, t.TempDir(), fl, true)
	if err := service.WritePass(filepath.Join(dir, "state"), service.Pass{
		At: time.Date(2026, 11, 10, 15, 4, 5, 0, time.UTC), Outcome: "ok", Lease: service.LeaseHeld, Holder: "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if code := a.main(context.Background(), []string{"service", "status", "--config", cfg}); code != 0 {
		t.Fatal(h.stderr.String())
	}
	want := "loaded: yes\nrunning: yes\npid: 4242\nlast_pass: 2026-11-10T15:04:05Z\noutcome: ok\nlease: held\nholder: 0123456789abcdef\n"
	if h.stdout.String() != want {
		t.Fatalf("status:\n%s", h.stdout.String())
	}

	// A tampered record is not printed.
	if err := os.WriteFile(filepath.Join(dir, "state", service.PassName), []byte("{\"at\":\"2026-11-10T15:04:05Z\",\"outcome\":\"ok\\nfixture:chat:1\",\"lease\":\"held\",\"holder\":\"not-an-id\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fl.printOut = "state = not running\n"
	a = serviceApp(t, h, t.TempDir(), fl, true)
	if code := a.main(context.Background(), []string{"service", "status", "--config", cfg}); code != 0 {
		t.Fatal(h.stderr.String())
	}
	out := h.stdout.String()
	if strings.Contains(out, "fixture") || strings.Contains(out, "not-an-id") || !strings.Contains(out, "loaded: yes\nrunning: no\n") || !strings.Contains(out, "last_pass: unreadable\n") {
		t.Fatalf("status:\n%s", out)
	}

	fl.printErr = os.ErrNotExist
	fl.printOut = "Could not find service\n"
	a = serviceApp(t, h, t.TempDir(), fl, true)
	if err := os.Remove(filepath.Join(dir, "state", service.PassName)); err != nil {
		t.Fatal(err)
	}
	if code := a.main(context.Background(), []string{"service", "status", "--config", cfg}); code != 0 {
		t.Fatal(h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "loaded: no\nrunning: no\npid: none\nlast_pass: none\n") {
		t.Fatalf("status:\n%s", h.stdout.String())
	}
}

func TestServiceLogs(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "dry-run", "lg")
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, service.StderrName), []byte("{\"msg\":\"run_finished\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, service.StdoutName), []byte("stdout-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := serviceApp(t, h, t.TempDir(), &fakeLaunch{}, true)
	if code := a.main(context.Background(), []string{"service", "logs", "--config", cfg}); code != 0 || h.stdout.String() != "{\"msg\":\"run_finished\"}\n" {
		t.Fatalf("logs: %d\n%s%s", code, h.stdout.String(), h.stderr.String())
	}
	a = serviceApp(t, h, t.TempDir(), &fakeLaunch{}, true)
	if code := a.main(context.Background(), []string{"service", "logs", "--stdout", "--config", cfg}); code != 0 || h.stdout.String() != "stdout-line\n" {
		t.Fatalf("stdout log: %d\n%s", code, h.stdout.String())
	}
}

func TestRunLoopSIGTERMFinishesCurrentSend(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, want := writeSetup(t, dir, h.server.URL, "send", "term")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(data, []byte("poll_interval: 50ms\nlease:\n  period: 1m\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var events int
	h.server.Script = func(_ int, req signaltest.Request) signaltest.Reply {
		if strings.HasPrefix(req.Key, "claim:") {
			return signaltest.Reply{}
		}
		mu.Lock()
		events++
		n := events
		mu.Unlock()
		if n == 1 {
			once.Do(func() { close(started) })
			<-release
		}
		return signaltest.Reply{}
	}
	term, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := h.app(h.egressList(cfg))
	a.term = term
	done := make(chan int, 1)
	go func() {
		done <- a.main(context.Background(), []string{"run", "--config", cfg})
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("send did not start")
	}
	cancel()
	close(release)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, h.stdout.String(), h.stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not exit after SIGTERM")
	}
	mu.Lock()
	n := events
	mu.Unlock()
	if n != 1 {
		t.Fatalf("event sends %d, want the in-flight send only", n)
	}
	if !strings.Contains(h.stderr.String(), `"msg":"run_stopped"`) {
		t.Fatalf("no run_stopped:\n%s", h.stderr.String())
	}
	pass, err := service.ReadPass(filepath.Join(dir, "state"))
	if err != nil || pass.Outcome != "stopped" || pass.Lease != service.LeaseHeld || pass.Holder == "" {
		t.Fatalf("pass %+v err %v", pass, err)
	}
	if strings.Contains(h.stderr.String(), "UNLISTED-SECRET") || strings.Contains(string(mustRead(t, filepath.Join(dir, "state", service.PassName))), want[0].key) {
		t.Fatal("pass record or log contains item content")
	}
}

func TestDryRunLoopStopsOnSIGTERMWithoutWriting(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "dry-run", "loop")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(data, []byte("poll_interval: 30s\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	term, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := h.app(h.egressList(cfg))
	a.term = term
	saw := make(chan struct{})
	var once sync.Once
	var buf syncBuf
	a.stdout = &watchWriter{buf: &buf, saw: saw, once: &once, needle: "Dry run"}
	done := make(chan int, 1)
	go func() {
		done <- a.main(context.Background(), []string{"run", "--config", cfg})
	}()
	select {
	case <-saw:
	case code := <-done:
		t.Fatalf("exited %d before the loop could be stopped\n%s", code, h.stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("no pass")
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, h.stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dry run did not exit after SIGTERM")
	}
	if _, err := os.Stat(filepath.Join(dir, "state", service.PassName)); !os.IsNotExist(err) {
		t.Fatal("dry run wrote a pass record")
	}
	if !strings.Contains(h.stderr.String(), `"msg":"run_stopped"`) {
		t.Fatalf("no run_stopped:\n%s", h.stderr.String())
	}
}

func TestRunRefusesRoot(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "dry-run", "root")
	a := h.app(nil)
	a.euid = func() int { return 0 }
	if code := a.main(context.Background(), []string{"run", "--once", "--config", cfg}); code == 0 || !strings.Contains(h.stderr.String(), "refusing to run as root") {
		t.Fatalf("exit %d\n%s", code, h.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("root run created state")
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

type watchWriter struct {
	buf    *syncBuf
	saw    chan struct{}
	once   *sync.Once
	needle string
}

func (w *watchWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	w.buf.mu.Lock()
	got := strings.Contains(w.buf.b.String(), w.needle)
	w.buf.mu.Unlock()
	if got {
		w.once.Do(func() { close(w.saw) })
	}
	return n, err
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
