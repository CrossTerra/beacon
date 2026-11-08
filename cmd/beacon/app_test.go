package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/egress/egresstest"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/queue"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
)

const token = "sig_TOKEN_must_never_be_logged"

// A send pass prints decisions without bodies; --bodies adds them. A dry run always shows them.
func TestSendModeBodies(t *testing.T) {
	h := newHarness(t)
	cfg, want := writeSetup(t, t.TempDir(), h.server.URL, "send", "bodies")
	allowed := h.egressList(cfg)
	if code := h.run(h.app(allowed), "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	out := h.stdout.String()
	if strings.Contains(out, `"subject"`) || !strings.Contains(out, "send   "+want[0].key) {
		t.Fatalf("send pass printed bodies or lacks decisions:\n%s", out)
	}
	if code := h.run(h.app(allowed), "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	if strings.Contains(h.stdout.String(), want[0].key+"  rule") || strings.Contains(h.stdout.String(), "skip ") {
		t.Fatalf("a later pass repeats earlier decisions:\n%s", h.stdout.String())
	}
	cfg2, want2 := writeSetup(t, t.TempDir(), h.server.URL, "send", "bodies2")
	h2 := newHarness(t)
	if code := h2.run(h2.app(h2.egressList(cfg2)), "run", "--once", "--bodies", "--config", cfg2); code != 0 {
		t.Fatal(h2.stderr.String())
	}
	if !strings.Contains(h2.stdout.String(), `"key":"`+want2[0].key+`"`) {
		t.Fatalf("--bodies printed no body:\n%s", h2.stdout.String())
	}
}

// A refused key, or a refused lease claim, fails the run with the reason, not "held by another instance".
func TestRunExitsOnRefusal(t *testing.T) {
	for name, c := range map[string]struct {
		token  string
		script func(int, signaltest.Request) signaltest.Reply
		want   string
	}{
		"401": {token: "sig_wrong", want: "Signal refused the key (401)"},
		"claim refused": {token: token, want: "Signal refused the lease claim with 403 forbidden", script: func(int, signaltest.Request) signaltest.Reply {
			return signaltest.Reply{Status: 403, Body: `{"error":{"code":"forbidden","message":"No."}}`}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.store.Put("test", c.token)
			h.server.Script = c.script
			cfg, _ := writeSetup(t, t.TempDir(), h.server.URL, "send", "refused")
			code := h.run(h.app(h.egressList(cfg)), "run", "--once", "--config", cfg)
			if code == 0 || !strings.Contains(h.stderr.String(), c.want) || strings.Contains(h.stdout.String(), "held by another") {
				t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, h.stdout.String(), h.stderr.String())
			}
			if !strings.Contains(h.stderr.String(), `"msg":"run_failed"`) {
				t.Fatalf("no run_failed log line:\n%s", h.stderr.String())
			}
		})
	}
}

// During run, error text that names keys or rule patterns stays off standard error.
func TestRunErrorsNameNoKeysOrPatterns(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "send", "errs")
	data, _ := fixture("errs")
	dup := strings.Replace(string(data), `"fixture:chat5-errs:1"`, `"fixture:misc6-errs:1"`, 1)
	must(t, os.WriteFile(filepath.Join(dir, "fixture.json"), []byte(dup), 0o600))
	code := h.run(h.app(nil), "run", "--once", "--config", cfg)
	if code == 0 || strings.Contains(h.stderr.String(), "misc6") || !strings.Contains(h.stderr.String(), "beacon rules check shows why") {
		t.Fatalf("exit %d, stderr:\n%s", code, h.stderr.String())
	}
	must(t, os.WriteFile(filepath.Join(dir, "fixture.json"), data, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte("rules:\n  - name: r\n    match: {type: {regex: \"SECRET(\"}}\n    tier: send\n"), 0o600))
	code = h.run(h.app(nil), "run", "--once", "--config", cfg)
	if code == 0 || strings.Contains(h.stderr.String(), "SECRET") {
		t.Fatalf("exit %d, stderr:\n%s", code, h.stderr.String())
	}
}

// TestMain doubles as a helper process: with BEACON_TEST_HELPER set, the test binary runs `beacon run --once` and
// stops at the point the variable names, so a test can kill Beacon at an exact step and restart it.
func TestMain(m *testing.M) {
	if mode := os.Getenv("BEACON_TEST_HELPER"); mode != "" {
		os.Exit(helper(mode, os.Getenv("BEACON_TEST_CONFIG")))
	}
	os.Exit(m.Run())
}

func helper(mode, cfg string) int {
	store := keychain.NewMemory()
	if k := os.Getenv("BEACON_TEST_TOKEN"); k != "" {
		store.Put("test", k)
	} else {
		store.Put("test", token)
	}
	a := &app{stdin: strings.NewReader(""), stdout: os.Stdout, stderr: os.Stderr, now: time.Now,
		keychain: func() (keychain.Store, error) { return store, nil }, transport: egress.Transport}
	switch mode {
	case "crash-after-append":
		// Killed after the first body is appended and synced, before anything is sent.
		a.afterAppend = func(string) { os.Exit(86) }
	case "crash-after-send":
		// Killed after Signal wrote the first event, before the outbox records the answer.
		a.transport = func(allowed []string) http.RoundTripper {
			return exitAfterEvent{egress.Transport(allowed)}
		}
	}
	return a.main(context.Background(), []string{"run", "--once", "--config", cfg})
}

type exitAfterEvent struct{ next http.RoundTripper }

func (e exitAfterEvent) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := req.GetBody()
	var b struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(body).Decode(&b)
	resp, err := e.next.RoundTrip(req)
	if err == nil && !strings.HasPrefix(b.Key, "claim:") {
		os.Exit(87)
	}
	return resp, err
}

// runHelper runs the helper process and returns its exit code.
func runHelper(t *testing.T, mode, cfg string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "BEACON_TEST_HELPER="+mode, "BEACON_TEST_CONFIG="+cfg)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out.String())
	}
	return 0
}

type harness struct {
	t      *testing.T
	server *signaltest.Server
	rec    *egresstest.Recorder
	store  *keychain.Memory
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, server: signaltest.New(token), store: keychain.NewMemory()}
	t.Cleanup(h.server.Close)
	h.store.Put("test", token)
	return h
}

// app returns an app whose every request goes through the recorder, which allows only allowed. Behind it is the
// production transport, guard included, so the recorder sees every request Beacon attempts and the requests go out
// exactly as they would in use.
func (h *harness) app(allowed []string) *app {
	if h.rec == nil {
		h.rec = egresstest.New(h.t, allowed, http.DefaultTransport)
	}
	h.stdout.Reset()
	h.stderr.Reset()
	return &app{
		stdin: strings.NewReader(""), stdout: &h.stdout, stderr: &h.stderr, now: time.Now,
		keychain: func() (keychain.Store, error) { return h.store, nil },
		transport: func(allowed []string) http.RoundTripper {
			return h.rec.Wrap(egress.Transport(allowed))
		},
		configureClient: func(c *signal.Client) {
			c.Sleep = func(context.Context, time.Duration) error { return nil }
		},
	}
}

func (h *harness) run(a *app, args ...string) int {
	return a.main(context.Background(), args)
}

// egressList runs `beacon egress` and returns the addresses it prints.
func (h *harness) egressList(cfg string) []string {
	h.t.Helper()
	var out bytes.Buffer
	a := &app{stdout: &out, stderr: &out}
	if code := a.main(context.Background(), []string{"egress", "--config", cfg}); code != 0 {
		h.t.Fatalf("egress: %d %s", code, out.String())
	}
	var addrs []string
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) > 0 {
			addrs = append(addrs, f[0])
		}
	}
	return addrs
}

func eventRequests(s *signaltest.Server) []signaltest.Request {
	var out []signaltest.Request
	for _, r := range s.Requests() {
		if !strings.HasPrefix(r.Key, "claim:") {
			out = append(out, r)
		}
	}
	return out
}

func stateOf(t *testing.T, cfg string) (*outbox.Outbox, *queue.Queue) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(cfg), "state")
	ob, err := outbox.Read(dir)
	must(t, err)
	q, err := queue.Open(dir)
	must(t, err)
	return ob, q
}

// The exit gate: every item lands in its tier, held and dropped items never reach the network, and `beacon egress`
// lists every host the test transport saw.
func TestExitGateTiersAndEgress(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, want := writeSetup(t, dir, h.server.URL, "send", "gate")
	allowed := h.egressList(cfg)

	// Dry run first: tiers are printed, nothing is contacted, nothing is written.
	a := h.app(allowed)
	if code := h.run(a, "run", "--dry-run", "--once", "--config", cfg); code != 0 {
		t.Fatalf("dry run: %d\n%s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, w := range want {
		rule := "rule " + w.rule
		if w.rule == "" {
			rule = "(no rule matched: default never)"
		}
		line := fmt.Sprintf("%-6s %s  %s", w.tier, w.key, rule)
		if !strings.Contains(out, line) {
			t.Errorf("dry run lacks %q\n%s", line, out)
		}
	}
	if h.rec.Count() != 0 {
		t.Fatalf("dry run made %d requests", h.rec.Count())
	}
	if _, err := os.Stat(filepath.Join(dir, "state", outbox.FileName)); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote the outbox: %v", err)
	}

	// Send.
	a = h.app(allowed)
	if code := h.run(a, "run", "--once", "--config", cfg); code != 0 {
		t.Fatalf("send: %d\n%s\n%s", code, h.stdout.String(), h.stderr.String())
	}
	ob, q := stateOf(t, cfg)
	for _, w := range want {
		_, inOutbox := ob.Get(w.key)
		_, onServer := h.server.Event(w.key)
		switch w.tier {
		case rules.Send:
			e, _ := ob.Get(w.key)
			if !inOutbox || e.State != outbox.Sent || e.Rule != w.rule || !onServer {
				t.Errorf("%s: outbox %v %+v, server %v", w.key, inOutbox, e.State, onServer)
			}
		case rules.Hold:
			if inOutbox || onServer || !q.IsHeld(w.key) {
				t.Errorf("held %s: outbox %v server %v queue %v", w.key, inOutbox, onServer, q.IsHeld(w.key))
			}
		case rules.Never:
			if inOutbox || onServer || q.IsHeld(w.key) {
				t.Errorf("dropped %s reached outbox %v / server %v / queue", w.key, inOutbox, onServer)
			}
		}
	}
	// Held and dropped items, and unlisted fields, never reached the network.
	for _, r := range h.server.Requests() {
		for _, s := range secrets {
			if bytes.Contains(r.Body, []byte(s)) {
				t.Errorf("request body carries %s: %s", s, r.Body)
			}
		}
		for _, w := range want {
			if w.tier != rules.Send && bytes.Contains(r.Body, []byte(w.key)) {
				t.Errorf("request names %s item %s", w.tier, w.key)
			}
		}
	}
	// Each sent body is the exact stored bytes.
	for _, e := range ob.Entries() {
		ev, ok := h.server.Event(e.Key)
		if !ok || !bytes.Equal(ev.Body, e.Body) {
			t.Errorf("%s: server has %s, outbox %s", e.Key, ev.Body, e.Body)
		}
	}
	// Every host the transport saw is on the egress list.
	listed := map[string]bool{}
	for _, a := range allowed {
		listed[a] = true
	}
	if len(h.rec.Seen()) == 0 {
		t.Fatal("the transport saw nothing")
	}
	for _, s := range h.rec.Seen() {
		if !listed[s] {
			t.Errorf("transport saw %s; beacon egress lists %v", s, allowed)
		}
	}

	// A second pass sends nothing new.
	before := len(h.server.Requests())
	a = h.app(allowed)
	if code := h.run(a, "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	if n := len(eventRequests(h.server)); n != 4 {
		t.Fatalf("%d event requests after the second pass, want 4", n)
	}
	if after := len(h.server.Requests()); after != before {
		t.Fatalf("second pass made %d requests; the lease was already claimed ahead", after-before)
	}
}

// DEC-11/20/21 as narrowed by DEC-27, on the wire.
func TestEventShapes(t *testing.T) {
	h := newHarness(t)
	cfg, want := writeSetup(t, t.TempDir(), h.server.URL, "send", "shape")
	if code := h.run(h.app(h.egressList(cfg)), "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	get := func(key string) map[string]any {
		ev, ok := h.server.Event(key)
		if !ok {
			t.Fatalf("no event %s", key)
		}
		var m map[string]any
		must(t, json.Unmarshal(ev.Body, &m))
		return m
	}
	ask := get(want[0].key)
	if fmt.Sprint(ask["to"]) != "[@winton]" || ask["priority"] != "high" || ask["by"] != nil {
		t.Errorf("ask: %v", ask)
	}
	if md := ask["metadata"].(map[string]any); md["actor"] != "fixture:user:dana" {
		t.Errorf("ask metadata: %v", md)
	}
	if st := fmt.Sprint(ask["state"]); st != "map[observed_at:2026-11-07T09:01:30.000Z status:open version:1]" {
		t.Errorf("ask state (DEC-28): %s", st)
	}
	if get(want[2].key)["state"] != nil {
		t.Errorf("state on an item without a snapshot")
	}
	own := get(want[1].key)
	if fmt.Sprint(own["by"]) != "map[identity:fixture:user:winton]" || fmt.Sprint(own["closes"]) != fmt.Sprintf("[map[key:%s]]", want[0].key) || own["to"] != nil {
		t.Errorf("own act: %v", own)
	}
	assigned := get(want[2].key)
	if assigned["due_at"] != "2026-11-09T17:00:00.000Z" || fmt.Sprint(assigned["to"]) != "[@winton]" {
		t.Errorf("assigned: %v", assigned)
	}
	answer := get(want[3].key)
	if answer["by"] != nil || fmt.Sprint(answer["closes"]) != fmt.Sprintf("[map[key:%s]]", want[2].key) {
		t.Errorf("close_by_others: %v", answer)
	}
}

func TestDryRunIsDefault(t *testing.T) {
	h := newHarness(t)
	cfg, _ := writeSetup(t, t.TempDir(), h.server.URL, "dry-run", "dry")
	if code := h.run(h.app(nil), "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "Dry run: nothing is sent.") || h.rec.Count() != 0 || len(h.server.Requests()) != 0 {
		t.Fatalf("mode dry-run sent: %d requests\n%s", len(h.server.Requests()), h.stdout.String())
	}
}

// The exit gate: a crash between append and send produces exactly one event, with the stored bytes.
func TestCrashBetweenAppendAndSend(t *testing.T) {
	h := newHarness(t)
	cfg, want := writeSetup(t, t.TempDir(), h.server.URL, "send", "crash")
	if code := runHelper(t, "crash-after-append", cfg); code != 86 {
		t.Fatalf("helper exit %d, want 86 (killed after append)", code)
	}
	ob, _ := stateOf(t, cfg)
	p := ob.Pending()
	if len(p) != 1 || p[0].Key != want[0].key {
		t.Fatalf("pending after the crash: %+v", p)
	}
	stored := p[0].Body
	if len(eventRequests(h.server)) != 0 {
		t.Fatal("an event was sent before the crash")
	}

	if code := h.run(h.app(h.egressList(cfg)), "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	reqs := h.server.RequestsFor(want[0].key)
	if len(reqs) != 1 || !bytes.Equal(reqs[0].Body, stored) {
		t.Fatalf("%d requests for the crashed key; first %s, stored %s", len(reqs), reqs[0].Body, stored)
	}
	n := 0
	for _, e := range h.server.Events() {
		if e.Key == want[0].key {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d events for the crashed key, want 1", n)
	}
	ob, _ = stateOf(t, cfg)
	if e, _ := ob.Get(want[0].key); e.State != outbox.Sent || !bytes.Equal(e.Body, stored) {
		t.Fatalf("after restart: %+v", e)
	}
	if len(ob.Pending()) != 0 {
		t.Fatalf("still pending: %d", len(ob.Pending()))
	}
}

// A crash after Signal wrote the event but before the outbox recorded it: the restart sends the stored bytes, Signal
// replays, and the key is recorded as replayed. Still one event.
func TestCrashAfterSendIsReplayed(t *testing.T) {
	h := newHarness(t)
	cfg, want := writeSetup(t, t.TempDir(), h.server.URL, "send", "replay")
	if code := runHelper(t, "crash-after-send", cfg); code != 87 {
		t.Fatalf("helper exit %d, want 87 (killed after send)", code)
	}
	if code := h.run(h.app(h.egressList(cfg)), "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	ob, _ := stateOf(t, cfg)
	e, _ := ob.Get(want[0].key)
	if e.State != outbox.Replayed {
		t.Fatalf("state %s, want replayed", e.State)
	}
	reqs := h.server.RequestsFor(want[0].key)
	if len(reqs) != 2 || !bytes.Equal(reqs[0].Body, reqs[1].Body) {
		t.Fatalf("%d requests, bodies differ or missing", len(reqs))
	}
	if got := len(h.server.Events()); got != 4+2 {
		t.Fatalf("%d events, want 4 events and 2 lease claims", got)
	}
}

// The exit gate: a retry resends the stored bytes, even after the rules change.
func TestRetryResendsStoredBytes(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "send", "retry")
	h.server.Script = func(_ int, r signaltest.Request) signaltest.Reply {
		if strings.HasPrefix(r.Key, "claim:") {
			return signaltest.Reply{}
		}
		return signaltest.Reply{Status: 503, Body: `{"error":{"code":"unavailable","message":"down"}}`}
	}
	allowed := h.egressList(cfg)
	if code := h.run(h.app(allowed), "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	ob, _ := stateOf(t, cfg)
	if len(ob.Pending()) != 4 {
		t.Fatalf("pending %d, want 4", len(ob.Pending()))
	}
	first := ob.Pending()[0]
	// Signal was down for the first body; later ones wait behind it.
	if n := len(eventRequests(h.server)); n != 5 {
		t.Fatalf("%d attempts, want 5 for the first body only", n)
	}

	// The person changes the rule. The pending body is already decided: its stored bytes are sent.
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"),
		[]byte(strings.Replace(testRules, "fields: [title, url, repository, actor]", "fields: [title]", 1)), 0o600))
	h.server.Script = nil
	if code := h.run(h.app(allowed), "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	ev, ok := h.server.Event(first.Key)
	if !ok || !bytes.Equal(ev.Body, first.Body) {
		t.Fatalf("sent %s, stored %s", ev.Body, first.Body)
	}
	for _, r := range h.server.RequestsFor(first.Key) {
		if !bytes.Equal(r.Body, first.Body) {
			t.Fatalf("an attempt sent other bytes: %s", r.Body)
		}
	}
}

// A 409 idempotency_conflict stops that key: it is recorded as rejected, reported, and never sent again.
func TestConflictStopsKey(t *testing.T) {
	h := newHarness(t)
	cfg, want := writeSetup(t, t.TempDir(), h.server.URL, "send", "conflict")
	h.server.Seed(want[0].key, []byte(`{"subject":"x","key":"`+want[0].key+`","title":"someone else"}`))
	allowed := h.egressList(cfg)
	if code := h.run(h.app(allowed), "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "conflict  "+want[0].key+"  409") {
		t.Fatalf("conflict not reported:\n%s", h.stdout.String())
	}
	ob, _ := stateOf(t, cfg)
	if e, _ := ob.Get(want[0].key); e.State != outbox.Rejected || e.Code != "idempotency_conflict" {
		t.Fatalf("%+v", e)
	}
	if code := h.run(h.app(allowed), "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	if n := len(h.server.RequestsFor(want[0].key)); n != 1 {
		t.Fatalf("a rejected key was sent %d times", n)
	}
	if code := h.run(h.app(allowed), "outbox", "--config", cfg); code != 0 || !strings.Contains(h.stdout.String(), "409 idempotency_conflict") {
		t.Fatalf("outbox does not show the conflict:\n%s", h.stdout.String())
	}
}

// A second instance with the lease held sends nothing.
func TestSecondInstanceSendsNothing(t *testing.T) {
	h := newHarness(t)
	cfgA, _ := writeSetup(t, t.TempDir(), h.server.URL, "send", "lease")
	cfgB, _ := writeSetup(t, t.TempDir(), h.server.URL, "send", "lease")
	allowed := h.egressList(cfgA)
	if code := h.run(h.app(allowed), "run", "--once", "--config", cfgA); code != 0 {
		t.Fatal(h.stderr.String())
	}
	sentByA := len(eventRequests(h.server))
	if code := h.run(h.app(allowed), "run", "--once", "--config", cfgB); code != 0 {
		t.Fatal(h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "Lease: held by another instance; nothing was sent.") {
		t.Fatalf("B:\n%s", h.stdout.String())
	}
	if n := len(eventRequests(h.server)); n != sentByA {
		t.Fatalf("B sent %d event requests", n-sentByA)
	}
	obB, _ := stateOf(t, cfgB)
	if len(obB.Pending()) != 4 {
		t.Fatalf("B collected %d pending, want 4", len(obB.Pending()))
	}
}

// Logs carry no content, keys, bodies or the token.
func TestLogsCarryNoContent(t *testing.T) {
	h := newHarness(t)
	cfg, want := writeSetup(t, t.TempDir(), h.server.URL, "send", "logs")
	h.server.Seed(want[2].key, []byte(`{"other":1}`))
	if code := h.run(h.app(h.egressList(cfg)), "run", "--once", "--config", cfg); code != 0 {
		t.Fatal(h.stderr.String())
	}
	logs := h.stderr.String()
	if logs == "" {
		t.Fatal("no logs")
	}
	banned := append([]string{token, "Add retries", "Rotate the keys", "fixture:user:", "acme/app", "@winton", `"subject"`, "rule"}, secrets...)
	for _, w := range want {
		banned = append(banned, w.key)
	}
	for _, b := range banned {
		if strings.Contains(logs, b) {
			t.Errorf("logs contain %q", b)
		}
	}
	sc := bufio.NewScanner(strings.NewReader(logs))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %s", sc.Text())
		}
	}
}

func TestExplain(t *testing.T) {
	h := newHarness(t)
	cfg, want := writeSetup(t, t.TempDir(), h.server.URL, "send", "explain")
	allowed := h.egressList(cfg)
	explain := func(key string) string {
		if code := h.run(h.app(allowed), "explain", "--config", cfg, key); code != 0 {
			t.Fatalf("explain %s: %s", key, h.stderr.String())
		}
		return h.stdout.String()
	}
	out := explain(want[0].key)
	if !strings.Contains(out, `Rule:       3 "review-requests"`) || !strings.Contains(out, "Body (would be sent as):") ||
		!strings.Contains(out, `"to":["@winton"]`) || !strings.Contains(out, "Classifier: not configured") {
		t.Fatalf("before send:\n%s", out)
	}
	if out := explain(want[6].key); !strings.Contains(out, "none matched; default never") || !strings.Contains(out, "nothing about this item leaves") {
		t.Fatalf("default never:\n%s", out)
	}
	if h.run(h.app(allowed), "run", "--once", "--config", cfg) != 0 {
		t.Fatal(h.stderr.String())
	}
	ev, _ := h.server.Event(want[0].key)
	out = explain(want[0].key)
	if !strings.Contains(out, "Outbox:     sent") || !strings.Contains(out, string(ev.Body)) || !strings.Contains(out, ev.ID) {
		t.Fatalf("after send:\n%s", out)
	}
	if out := explain(want[4].key); !strings.Contains(out, "Hold queue: held") {
		t.Fatalf("held:\n%s", out)
	}
	if h.run(h.app(allowed), "explain", "--config", cfg, "fixture:nope:1") != 1 {
		t.Fatal("explain of an unknown key succeeded")
	}
}

func TestRulesCheck(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "send", "check")
	if code := h.run(h.app(nil), "rules", "check", "--config", cfg); code != 0 {
		t.Fatalf("clean rules: %d\n%s", code, h.stdout.String())
	}
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte(testRules+`  - name: shadowed
    match: {own: true, type: commented}
    tier: hold
  - name: other-source
    match: {source: github}
    tier: send
`), 0o600))
	if code := h.run(h.app(nil), "rules", "check", "--config", cfg); code != 1 {
		t.Fatalf("exit %d", code)
	}
	out := h.stdout.String()
	if !strings.Contains(out, `rule 7 "shadowed"`) || !strings.Contains(out, `rule 1 "own-acts"`) || !strings.Contains(out, `rule 8 "other-source"`) {
		t.Fatalf("report:\n%s", out)
	}
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte("rules:\n  - name: x\n    match: {}\n    tier: send\n    bogus: 1\n"), 0o600))
	if code := h.run(h.app(nil), "rules", "check", "--config", cfg); code != 1 || !strings.Contains(h.stderr.String(), "bogus") {
		t.Fatalf("unknown key: %d %s", code, h.stderr.String())
	}
}

func TestInit(t *testing.T) {
	h := newHarness(t)
	store := keychain.NewMemory()
	path := filepath.Join(t.TempDir(), "beacon", "config.yaml")
	var out, errOut bytes.Buffer
	a := &app{stdin: strings.NewReader("sig_from_stdin\n"), stdout: &out, stderr: &errOut, now: time.Now,
		keychain: func() (keychain.Store, error) { return store, nil }}
	code := a.main(context.Background(), []string{"init", "--config", path, "--signal-url", "https://signal.example.com/", "--handle", "@winton"})
	if code != 0 {
		t.Fatalf("%d %s", code, errOut.String())
	}
	s, err := store.Get("signal:signal.example.com:443")
	if err != nil || s.Empty() {
		t.Fatalf("key not stored: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "sig_from_stdin") || !strings.Contains(string(data), "mode: dry-run") {
		t.Fatalf("config:\n%s", data)
	}
	rulesData, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "rules.yaml"))
	if f, err := rules.Parse(rulesData); err != nil || len(f.Rules) != 0 {
		t.Fatalf("rules: %v", err)
	}
	if code := a.main(context.Background(), []string{"init", "--config", path, "--signal-url", "https://x.example", "--handle", "@w"}); code != 1 {
		t.Fatal("init overwrote a config")
	}
	// The config init wrote loads and the egress list is the Signal host alone.
	if got := h.egressList(path); len(got) != 1 || got[0] != "signal.example.com:443" {
		t.Fatalf("egress %v", got)
	}
}

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	a := &app{stdout: &out, stderr: &out}
	if a.main(context.Background(), []string{"version"}) != 0 || !strings.Contains(out.String(), "beacon dev") || !strings.Contains(out.String(), "go:") {
		t.Fatal(out.String())
	}
}

// A request to a host off the list is refused before any connection, whatever the config says.
func TestProductionTransportRefusesOtherHosts(t *testing.T) {
	rt := egress.Transport([]string{"signal.example.com:443"})
	req, _ := http.NewRequest(http.MethodGet, "https://telemetry.example.net/ping", nil)
	_, err := rt.RoundTrip(req)
	var denied *egress.DeniedError
	if !errors.As(err, &denied) || denied.Address != "telemetry.example.net:443" {
		t.Fatalf("err %v", err)
	}
}
