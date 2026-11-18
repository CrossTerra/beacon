package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/coverage"
	"github.com/CrossTerra/beacon/internal/egress/egresstest"
	"github.com/CrossTerra/beacon/internal/google/googletest"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/service"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
)

// Each source runs on its own loop (DEC-41): while Gmail's token is refused on every pass, the fixture keeps
// collecting and sending on its own interval. Coverage records both (DEC-42): Gmail as failing with
// source_unauthorized, in coverage.json, in `beacon status --coverage`, and in Signal as a snapshot on its own subject;
// the fixture as ok, with a heartbeat lease. Nothing in any of it is a token or item content.
func TestSourceLoopsReportCoverage(t *testing.T) {
	srv := googletest.New(t)
	sig := signaltest.New("sig-key")
	t.Cleanup(sig.Close)
	sig.Leases = true
	sig.Restrict("gmail:*")
	dir := t.TempDir()
	data, _ := fixture("loops")
	must(t, os.WriteFile(filepath.Join(dir, "fixture.json"), data, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte(testRules), 0o600))
	conf := strings.Replace(googleConfig, "https://signal.example.com", sig.URL+"\n  keychain_account: test", 1)
	conf = strings.Replace(conf, "mode: dry-run", "mode: send", 1)
	conf = strings.Replace(conf, "    drive: email:winton@example.com\n", "    drive: email:winton@example.com\n    fixture: fixture:user:winton\n", 1)
	conf += "  - kind: fixture\n    path: fixture.json\n  - kind: gmail\n    account: winton@example.com\n    interval: 1s\n" +
		"poll_interval: 50ms\nlease:\n  period: 1m\n"
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte(conf), 0o600))
	state := filepath.Join(dir, "state")
	must(t, os.MkdirAll(state, 0o700))
	// The fixture's first run needs no acceptance; Gmail's is accepted, and its grant is refused.
	must(t, os.WriteFile(filepath.Join(state, "accepted.json"), []byte(`["gmail"]`), 0o600))
	store := keychain.NewMemory()
	store.Put("test", "sig-key")
	store.Put(keychain.GoogleClientAccount(googletest.Email), `{"client_id":"`+googletest.ClientID+`","client_secret":"`+googletest.ClientSecret+`"}`)
	store.Put(keychain.GoogleRefreshAccount(googletest.Email), "revoked-refresh-token")

	var out, errOut strings.Builder
	var rec *egresstest.Recorder
	a := googleApp(t, srv, store, &rec, &out, &errOut)
	a.now = time.Now
	a.configureClient = func(c *signal.Client) { c.Attempts = 1 }
	term, stop := context.WithCancel(context.Background())
	defer stop()
	a.term = term
	done := make(chan int, 1)
	go func() { done <- a.main(context.Background(), []string{"run", "--config", cfg}) }()

	snapshots := func() map[string]coverage.Snapshot {
		got := map[string]coverage.Snapshot{}
		for _, e := range sig.Events() {
			var b struct {
				Subject string `json:"subject"`
				Type    string `json:"type"`
				State   struct {
					Fields coverage.Snapshot `json:"fields"`
				} `json:"state"`
			}
			if json.Unmarshal(e.Body, &b) == nil && b.Type == "beacon.coverage" {
				got[b.Subject] = b.State.Fields
			}
		}
		return got
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		snaps := snapshots()
		if snaps["beacon:coverage:winton:gmail"].Status == coverage.StatusFailing && snaps["beacon:coverage:winton:fixture"].Status == coverage.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			stop()
			<-done
			t.Fatalf("snapshots %+v\n%s", snaps, errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the loops did not stop on SIGTERM")
	}

	log := errOut.String()
	fixturePasses := strings.Count(log, `"msg":"collected","source":"fixture"`)
	gmailFailures := strings.Count(log, `"msg":"collect_failed","source":"gmail","reason":"source_unauthorized"`)
	if gmailFailures < 2 || fixturePasses < 2*gmailFailures {
		t.Fatalf("fixture passes %d, gmail failures %d: the fixture waited on gmail\n%s", fixturePasses, gmailFailures, log)
	}
	if !strings.Contains(log, `"msg":"run_stopped"`) {
		t.Fatal("no run_stopped")
	}

	gm := snapshots()["beacon:coverage:winton:gmail"]
	if gm.Reason != "source_unauthorized" || gm.Failures < 2 || gm.FailingSince == "" || gm.LastSuccess != "" {
		t.Fatalf("gmail snapshot %+v", gm)
	}
	// The fixture holds a heartbeat; Gmail, which never collected, does not.
	var beats []string
	for _, r := range sig.Requests() {
		if r.Path == "/api/leases" {
			var c struct{ Name, Run string }
			_ = json.Unmarshal(r.Body, &c)
			if strings.HasPrefix(c.Name, "beacon:heartbeat:") {
				beats = append(beats, c.Name)
				if !strings.HasSuffix(c.Run, ":heartbeat") {
					t.Fatalf("heartbeat run %q", c.Run)
				}
			}
		}
	}
	if len(beats) == 0 || strings.Contains(strings.Join(beats, " "), "gmail") || beats[0] != "beacon:heartbeat:winton:fixture" {
		t.Fatalf("heartbeats %v", beats)
	}

	rec2, err := coverage.Read(state)
	if err != nil {
		t.Fatal(err)
	}
	if g := rec2.Sources["gmail"]; g == nil || g.Status(time.Now()) != coverage.StatusFailing || g.EverySeconds != 1 {
		t.Fatalf("gmail %+v", rec2.Sources["gmail"])
	}
	if f := rec2.Sources["fixture"]; f == nil || f.LastSuccess == nil || f.EverySeconds != 0 {
		// poll_interval is 50ms: under a second.
		t.Fatalf("fixture %+v", rec2.Sources["fixture"])
	}

	// beacon status --coverage prints it.
	out.Reset()
	if code := a.main(context.Background(), []string{"status", "--coverage", "--config", cfg}); code != 0 {
		t.Fatalf("status exit %d", code)
	}
	for _, want := range []string{"source: gmail\n  status: failing\n  every: 1s\n", " source_unauthorized\n", "source: fixture\n  status: ok\n", "last_pass: 20"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status lacks %q:\n%s", want, out.String())
		}
	}

	// The record, the coverage bodies and the log hold no token and no item content.
	cov, _ := os.ReadFile(filepath.Join(state, coverage.FileName))
	ob, err := outbox.Read(state)
	if err != nil {
		t.Fatal(err)
	}
	var bodies strings.Builder
	for _, e := range ob.Entries() {
		if e.Kind == outbox.Coverage {
			bodies.Write(e.Body)
		}
	}
	for _, secret := range []string{"revoked-refresh-token", "sig-key", googletest.ClientSecret, "UNLISTED-SECRET"} {
		if strings.Contains(string(cov), secret) || strings.Contains(bodies.String(), secret) || strings.Contains(log, secret) || strings.Contains(out.String(), secret) {
			t.Fatalf("%q leaked", secret)
		}
	}
}

// A dry-run loop writes no coverage record.
func TestDryRunLoopWritesNoCoverage(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "dry-run", "loopcov")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(cfg, append(data, []byte("poll_interval: 20ms\n")...), 0o600))
	term, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := h.app(h.egressList(cfg))
	a.term = term
	var buf syncBuf
	saw := make(chan struct{})
	var once sync.Once
	a.stdout = &watchWriter{buf: &buf, saw: saw, once: &once, needle: "Items:"}
	done := make(chan int, 1)
	go func() { done <- a.main(context.Background(), []string{"run", "--config", cfg}) }()
	select {
	case <-saw:
	case <-time.After(10 * time.Second):
		t.Fatal("no pass")
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", coverage.FileName)); !os.IsNotExist(err) {
		t.Fatal("a dry run wrote coverage")
	}
}

// With no source configured, the loop still runs passes: it keeps the lease and sends what is pending.
func TestLoopWithoutSourcesKeepsRunning(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "send", "nosources")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	conf := strings.Replace(string(data), "sources:\n  - kind: fixture\n    path: fixture.json\n", "sources: []\n", 1)
	must(t, os.WriteFile(cfg, []byte(conf+"poll_interval: 20ms\nlease:\n  period: 1m\n"), 0o600))
	term, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := h.app(h.egressList(cfg))
	a.term = term
	done := make(chan int, 1)
	go func() { done <- a.main(context.Background(), []string{"run", "--config", cfg}) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if p, err := service.ReadPass(filepath.Join(dir, "state")); err == nil && p.Lease == service.LeaseHeld {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("the loop exited %d with no source", code)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no pass")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
}
