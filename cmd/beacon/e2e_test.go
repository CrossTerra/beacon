package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/egress/egresstest"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/outbox"
)

// TestE2E runs Beacon against a real Signal: scripts/signal-e2e.sh up, then
//
//	set -a; . ./.e2e.env; set +a; go test -race -run E2E -v ./cmd/beacon/
//
// It is skipped when BEACON_E2E_SIGNAL_URL is unset.
func TestE2E(t *testing.T) {
	base := os.Getenv("BEACON_E2E_SIGNAL_URL")
	if base == "" {
		t.Skip("BEACON_E2E_SIGNAL_URL is unset; see scripts/signal-e2e.sh")
	}
	key, reader := os.Getenv("BEACON_E2E_KEY"), os.Getenv("BEACON_E2E_READER_KEY")
	if key == "" || reader == "" || os.Getenv("BEACON_E2E_HANDLE") != "@winton" {
		t.Fatal("BEACON_E2E_KEY, BEACON_E2E_READER_KEY and BEACON_E2E_HANDLE=@winton are required")
	}
	sig := &signalReader{t: t, base: base, key: reader}
	store := keychain.NewMemory()
	store.Put("test", key)

	// One instance holds the lease for @winton in this Signal. Its state directory depends on the Beacon key, so a
	// rerun against the same Signal is the same instance and keeps the lease.
	sum := sha256.Sum256([]byte(base + key))
	state := filepath.Join(os.TempDir(), "beacon-e2e-"+hex.EncodeToString(sum[:6]))
	nonce := fmt.Sprintf("%d", time.Now().UnixNano())

	setup := func(dir, n, stateDir string) (string, []expected) {
		cfg, want := writeSetup(t, dir, base, "send", n)
		data, _ := os.ReadFile(cfg)
		must(t, os.WriteFile(cfg, bytes.Replace(data, []byte("state: state"), []byte("state: "+stateDir), 1), 0o600))
		return cfg, want
	}
	var stdout, stderr bytes.Buffer
	var rec *egresstest.Recorder
	beacon := func(args ...string) int {
		stdout.Reset()
		stderr.Reset()
		a := &app{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr, now: time.Now,
			keychain:  func() (keychain.Store, error) { return store, nil },
			transport: func(allowed []string) http.RoundTripper { return rec.Wrap(egress.Guard(allowed, http.DefaultTransport)) }}
		return a.main(context.Background(), args)
	}
	h := &harness{t: t}

	dir := t.TempDir()
	cfg, want := setup(dir, nonce, state)
	allowed := h.egressList(cfg)
	rec = egresstest.New(t, allowed, nil)
	pr1, issue3 := "fixture:pr:acme/app#1-"+nonce, "fixture:issue:3-"+nonce

	t.Run("dry run sends nothing", func(t *testing.T) {
		if code := beacon("run", "--dry-run", "--once", "--config", cfg); code != 0 {
			t.Fatal(stderr.String())
		}
		if rec.Count() != 0 {
			t.Fatalf("dry run made %d requests", rec.Count())
		}
		for _, s := range []string{pr1, issue3} {
			if sig.thread(s) != nil {
				t.Fatalf("a thread covers %s after the dry run", s)
			}
		}
	})

	t.Run("send writes the expected events and threads", func(t *testing.T) {
		if code := beacon("run", "--once", "--config", cfg); code != 0 {
			t.Fatalf("%s\n%s", stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "Lease: held until") {
			t.Fatalf("lease not held:\n%s", stdout.String())
		}
		ob, err := outbox.Read(state)
		must(t, err)
		for _, w := range want[:4] {
			if e, _ := ob.Get(w.key); e.State != outbox.Sent || !strings.HasPrefix(e.EventID, "evt_") {
				t.Fatalf("%s: %+v", w.key, e)
			}
		}

		th := sig.thread(pr1)
		if th == nil {
			t.Fatalf("no thread for %s", pr1)
		}
		if th.Title != "Add retries" || len(th.Events) != 2 || len(th.Asks) != 1 {
			t.Fatalf("pr thread: %+v", th)
		}
		ask, own := th.Events[0], th.Events[1]
		if ask.Key != want[0].key || ask.Type != "fixture.review_requested" || ask.By != nil {
			t.Fatalf("ask event: %+v", ask)
		}
		if own.Key != want[1].key || own.By == nil || own.By.Identity != "fixture:user:winton" || own.By.Actor.Handle != "winton" {
			t.Fatalf("own act: %+v", own)
		}
		a := th.Asks[0]
		if a.To.Handle != "winton" || a.Asker.Handle != "beacon-winton" || a.Priority != "high" || a.ClosedBy != own.ID || a.Outcome != "handled" {
			t.Fatalf("pr ask (DEC-20, DEC-27): %+v", a)
		}

		it := sig.thread(issue3)
		if it == nil || len(it.Events) != 2 || len(it.Asks) != 1 {
			t.Fatalf("issue thread: %+v", it)
		}
		a = it.Asks[0]
		if a.DueAt != "2026-11-09T17:00:00.000Z" || a.ClosedBy != it.Events[1].ID || it.Events[1].By != nil {
			t.Fatalf("issue ask closed by another person's answer (close_by_others): %+v", a)
		}

		if !sig.eventByKey(want[0].key) {
			t.Fatal("search by key does not find a sent event; the checks below would prove nothing")
		}
		for _, w := range want[4:] {
			if sig.eventByKey(w.key) {
				t.Fatalf("%s item %s reached Signal", w.tier, w.key)
			}
		}
		for _, s := range []string{"fixture:pr:other/lib#4-" + nonce, "fixture:chat:5-" + nonce, "fixture:misc:6-" + nonce} {
			if sig.thread(s) != nil {
				t.Fatalf("held or dropped subject %s has a thread", s)
			}
		}
		for _, s := range rec.Seen() {
			found := false
			for _, a := range allowed {
				found = found || a == s
			}
			if !found {
				t.Fatalf("transport saw %s, egress lists %v", s, allowed)
			}
		}
	})

	t.Run("a crash between append and send produces one event", func(t *testing.T) {
		n := nonce + "c"
		cfg, want := setup(dir, n, state)
		cmdEnv := []string{"BEACON_TEST_TOKEN=" + key}
		if code := runHelperEnv(t, "crash-after-append", cfg, cmdEnv); code != 86 {
			t.Fatalf("helper exit %d", code)
		}
		ob, _ := outbox.Read(state)
		e, _ := ob.Get(want[0].key)
		if e.State != outbox.Pending {
			t.Fatalf("after crash: %+v", e)
		}
		if code := beacon("run", "--once", "--config", cfg); code != 0 {
			t.Fatal(stderr.String())
		}
		th := sig.thread("fixture:pr:acme/app#1-" + n)
		count := 0
		for _, ev := range th.Events {
			if ev.Key == want[0].key {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%d events with the crashed key", count)
		}
	})

	t.Run("a crash after send is recorded as replayed", func(t *testing.T) {
		n := nonce + "r"
		cfg, want := setup(dir, n, state)
		if code := runHelperEnv(t, "crash-after-send", cfg, []string{"BEACON_TEST_TOKEN=" + key}); code != 87 {
			t.Fatalf("helper exit %d", code)
		}
		if code := beacon("run", "--once", "--config", cfg); code != 0 {
			t.Fatal(stderr.String())
		}
		ob, _ := outbox.Read(state)
		if e, _ := ob.Get(want[0].key); e.State != outbox.Replayed {
			t.Fatalf("state %s, want replayed", e.State)
		}
		if th := sig.thread("fixture:pr:acme/app#1-" + n); len(th.Events) != 2 {
			t.Fatalf("%d events", len(th.Events))
		}
	})

	t.Run("a 409 stops the key", func(t *testing.T) {
		n := nonce + "x"
		cfg, want := setup(dir, n, state)
		status := sig.post(key, fmt.Sprintf(`{"subject":"fixture:other-%s","type":"fixture.other","key":%q}`, n, want[0].key))
		if status != 201 {
			t.Fatalf("seed: %d", status)
		}
		if code := beacon("run", "--once", "--config", cfg); code != 0 {
			t.Fatal(stderr.String())
		}
		ob, _ := outbox.Read(state)
		if e, _ := ob.Get(want[0].key); e.State != outbox.Rejected || e.Status != 409 || e.Code != "idempotency_conflict" {
			t.Fatalf("%+v", e)
		}
	})

	t.Run("a second instance with the lease held sends nothing", func(t *testing.T) {
		n := nonce + "b"
		cfg, want := setup(t.TempDir(), n, filepath.Join(t.TempDir(), "state"))
		if code := beacon("run", "--once", "--config", cfg); code != 0 {
			t.Fatal(stderr.String())
		}
		if !strings.Contains(stdout.String(), "Lease: held by another instance; nothing was sent.") {
			t.Fatalf("%s", stdout.String())
		}
		for _, w := range want {
			if sig.eventByKey(w.key) {
				t.Fatalf("second instance sent %s", w.key)
			}
		}
		if sig.thread("fixture:pr:acme/app#1-"+n) != nil {
			t.Fatal("second instance opened a thread")
		}
	})
}

func runHelperEnv(t *testing.T, mode, cfg string, env []string) int {
	t.Helper()
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	return runHelper(t, mode, cfg)
}

// signalReader reads Signal as the person, to check what Beacon wrote. It is the test's client, not Beacon's.
type signalReader struct {
	t    *testing.T
	base string
	key  string
}

type actorRef struct {
	Handle string `json:"handle"`
}

type thread struct {
	Title  string `json:"title"`
	Events []struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Type string `json:"type"`
		By   *struct {
			Identity string   `json:"identity"`
			Actor    actorRef `json:"actor"`
		} `json:"by"`
	} `json:"events"`
	Asks []struct {
		To       actorRef `json:"to"`
		Asker    actorRef `json:"asker"`
		Priority string   `json:"priority"`
		DueAt    string   `json:"due_at"`
		ClosedBy string   `json:"closed_by"`
		Outcome  string   `json:"outcome"`
	} `json:"asks"`
}

func (s *signalReader) get(path string, out any) int {
	req, _ := http.NewRequest(http.MethodGet, s.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+s.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 && out != nil {
		must(s.t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}

func (s *signalReader) thread(subject string) *thread {
	var th thread
	switch code := s.get("/api/threads?include=events,asks&subject="+url.QueryEscape(subject), &th); code {
	case 200:
		return &th
	case 404:
		return nil
	default:
		s.t.Fatalf("thread %s: %d", subject, code)
		return nil
	}
}

// eventByKey reports whether a threaded event holds key. Held and dropped items' subjects have no thread either,
// which the test checks separately.
func (s *signalReader) eventByKey(key string) bool {
	var page struct {
		Data []struct {
			Event struct {
				Key string `json:"key"`
			} `json:"event"`
		} `json:"data"`
	}
	s.get("/api/search?group=event&limit=100&q="+url.QueryEscape("event-key:"+key), &page)
	for _, d := range page.Data {
		if d.Event.Key == key {
			return true
		}
	}
	return false
}

func (s *signalReader) post(key, body string) int {
	req, _ := http.NewRequest(http.MethodPost, s.base+"/api/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
