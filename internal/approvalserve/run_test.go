package approvalserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/actionexec"
	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/supervisor"
	"github.com/CrossTerra/beacon/internal/vault"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if code, helper := supervisor.HelperExit(os.Args[1:]); helper {
		os.Exit(code)
	}
	if len(os.Args) > 1 && os.Args[1] == "run" {
		if err := fakeOpenCode(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type invocation struct {
	Args                []string
	Cwd, Prompt, PWD    string
	OLDPWD, ServiceOnly string
	NestedProposal      string
	PID                 int
}

// The only executable used by these runs is the test binary. It acts immediately on the socket,
// then stays live until the test lets it finish. The watchdog is only for a broken test.
func fakeOpenCode() error {
	signal.Ignore(unix.SIGTERM)
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	state, err := os.ReadFile("state")
	if err != nil || os.Getenv("BEACON_SUPERVISOR") == "" {
		return errors.New("run did not enter the fixture checkout through the reaper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := actsocket.Call(ctx, string(state), approval.Object{"op": "propose", "type": "agent.run", "reason": "fixture",
		"agent": approval.Object{"name": "opencode-fixture"}, "params": approval.Object{"checkout": "a", "model": "test/one", "prompt": "child proposal"}}, 0)
	if err != nil {
		return err
	}
	var nested struct{ ID string }
	if json.Unmarshal(b, &nested) != nil || !approval.ValidID(nested.ID, "apr") {
		return errors.New("fixture child could not propose")
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	inv, err := json.Marshal(invocation{Args: os.Args[1:], Cwd: cwd, Prompt: string(prompt),
		PWD: os.Getenv("PWD"), OLDPWD: os.Getenv("OLDPWD"), ServiceOnly: os.Getenv("FIXTURE_SERVICE_ONLY"), NestedProposal: nested.ID, PID: os.Getpid()})
	if err != nil {
		return err
	}
	if err := os.WriteFile("invocation.tmp", inv, 0o600); err != nil {
		return err
	}
	if err := os.Rename("invocation.tmp", "invocation"); err != nil {
		return err
	}
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat("finish"); err == nil {
			fmt.Println(`{"type":"text","part":{"id":"p1","messageID":"m1","text":"Thinking."}}`)
			fmt.Println(`{"type":"text","part":{"id":"p2","messageID":"m2","text":"Done in"}}`)
			fmt.Println(`{"type":"text","part":{"id":"prt_m2_text-0","messageID":"m2","text":" the approved checkout."}}`)
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("fixture watchdog")
}

type runProposal struct {
	ID     string `json:"id"`
	Runner string `json:"runner_id"`
}

func proposeRun(t *testing.T, dir, checkout, prompt string) runProposal {
	t.Helper()
	b, err := actsocket.Call(t.Context(), dir, approval.Object{"op": "propose", "type": "agent.run", "reason": "fixture",
		"agent": approval.Object{"name": "bosun-fixture", "session": "ses_fixture"}, "params": approval.Object{
			"checkout": checkout, "prompt": prompt, "model": "test/one",
		}}, 0)
	must(t, err)
	var p runProposal
	must(t, json.Unmarshal(b, &p))
	if !approval.ValidID(p.ID, "apr") || !approval.ValidID(p.Runner, "run") {
		t.Fatal("run proposal refused")
	}
	return p
}

func runOptions(t *testing.T) (Options, string, string) {
	t.Helper()
	t.Setenv("FIXTURE_SERVICE_ONLY", "not-for-opencode")
	t.Setenv("OLDPWD", "/not-the-checkout")
	opts := options(t)
	opts.Supervision = &supervisor.Options{Grace: 200 * time.Millisecond, KillWait: 2 * time.Second}
	self, err := os.Executable()
	must(t, err)
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	a, b := filepath.Join(root, "checkout a"), filepath.Join(root, "checkout b")
	for _, path := range []string{a, b} {
		must(t, os.MkdirAll(filepath.Join(path, ".git"), 0o700))
		must(t, os.WriteFile(filepath.Join(path, ".git", "HEAD"), []byte("ref: refs/heads/fixture\n"), 0o600))
		must(t, os.WriteFile(filepath.Join(path, "state"), []byte(opts.StateDir), 0o600))
	}
	s, err := json.Marshal(actionexec.RunSettings{Opencode: self, Models: []string{"test/one", "test/two"},
		Checkouts: []actionexec.Checkout{{Name: "a", Path: a}, {Name: "alias", Path: a}, {Name: "b", Path: b}}})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(opts.StateDir, "agent-runs.json"), s, 0o600))
	return opts, a, b
}

func (b browser) freezeRun(p runProposal, model string, status int) string {
	b.t.Helper()
	v := url.Values{"csrf": {b.csrf}, "id": {p.ID}}
	if model != "" {
		v.Set("model", model)
	}
	_, page := b.request(http.MethodPost, "/freeze", v, status)
	return page
}

func (b *browser) runCSRF(p runProposal) string {
	b.t.Helper()
	_, page := b.request(http.MethodGet, "/p/"+p.ID, nil, 200)
	b.csrf = capture(b.t, page, `name="csrf" value="([a-zA-Z0-9_-]+)"`)
	return page
}

func waitInvocation(t *testing.T, path string) invocation {
	t.Helper()
	for until := time.Now().Add(6 * time.Second); time.Now().Before(until); {
		b, err := os.ReadFile(filepath.Join(path, "invocation"))
		if err == nil {
			var i invocation
			must(t, json.Unmarshal(b, &i))
			return i
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fake opencode did not launch")
	return invocation{}
}

func TestServeAgentRunsUseSeparateAssertionsAndPinnedCheckouts(t *testing.T) {
	opts, a, b := runOptions(t)
	key, prf := registerFixturePasskey(t, opts.StateDir)
	web, stop := signInBrowser(t, opts)
	prompt := "  keep \"quotes\", --flags and 👍\n\n"
	first := proposeRun(t, opts.StateDir, "a", prompt)
	second := proposeRun(t, opts.StateDir, "b", "different run")
	alias := proposeRun(t, opts.StateDir, "alias", "must not overlap")
	page := web.runCSRF(first)
	if !strings.Contains(page, `<option value="test/one" selected>`) || !strings.Contains(page, `value="test/two"`) {
		t.Fatal("model picker did not start at the proposed model")
	}
	web.request(http.MethodPost, "/freeze", url.Values{"csrf": {web.csrf}, "id": {first.ID, second.ID}}, 400)
	previous := web.form(key, prf, web.freezeRun(first, "", 200))
	page = web.freezeRun(first, "test/two", 200)
	if !strings.Contains(page, "Originally proposed model") || !strings.Contains(page, a) || !strings.Contains(page, "vol:") || !strings.Contains(page, "--auto") {
		t.Fatal("frozen page did not show the changed model, pinned checkout and permission warning")
	}
	web.request(http.MethodPost, "/approve", previous, 409)
	assertion := web.form(key, prf, page)
	assertion.Set("signature", "AA")
	web.request(http.MethodPost, "/approve", assertion, 401)
	page = web.freezeRun(first, "test/two", 200)
	web.request(http.MethodPost, "/approve", web.form(key, make([]byte, 32), page), 401)
	if _, err := os.Stat(filepath.Join(opts.StateDir, "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused assertion prepared a run")
	}
	page = web.freezeRun(first, "test/two", 200)
	assertion = web.form(key, prf, page)
	assertion.Set("model", "test/one") // the submitted form cannot change a frozen parameter
	web.request(http.MethodPost, "/approve", assertion, 200)
	web.request(http.MethodPost, "/approve", assertion, 409)
	firstCall := waitInvocation(t, a)
	web.freezeRun(alias, "", 409)
	page = web.freezeRun(second, "", 200)
	web.request(http.MethodPost, "/approve", web.form(key, prf, page), 200)
	secondCall := waitInvocation(t, b)
	for i, tc := range []struct {
		path, prompt, model string
		call                invocation
	}{{a, prompt, "test/two", firstCall}, {b, "different run", "test/one", secondCall}} {
		want := []string{"run", "--standalone", "--format", "json", "--auto", "--model", tc.model}
		if tc.call.Cwd != tc.path || tc.call.PWD != tc.path || !reflect.DeepEqual(want, tc.call.Args) || tc.call.Prompt != tc.prompt ||
			tc.call.OLDPWD != "" || tc.call.ServiceOnly != "" {
			t.Fatalf("run %d did not use the frozen invocation and clean environment: %+v", i, tc.call)
		}
		answer, err := actsocket.Call(t.Context(), opts.StateDir, approval.Object{"op": "get", "id": tc.call.NestedProposal}, 0)
		must(t, err)
		if string(answer) != `{"error":"not_found"}` {
			t.Fatal("the child's first proposal was not attributed to its run")
		}
	}
	if firstCall.PID == secondCall.PID || unix.Kill(firstCall.PID, 0) != nil || unix.Kill(secondCall.PID, 0) != nil {
		t.Fatal("the two checkouts were not active at the same time")
	}
	must(t, os.WriteFile(filepath.Join(a, "finish"), nil, 0o600))
	web.waitFor(first.ID, "runner_closed")
	_, page = web.request(http.MethodGet, "/p/"+first.ID, nil, 200)
	if !strings.Contains(page, "Done in the approved checkout.") {
		t.Fatal("the final report was not shown")
	}
	web.freezeRun(alias, "", 200) // closed releases the inode claim
	stop()                        // the second run is still ignoring TERM
	if !errors.Is(unix.Kill(secondCall.PID, 0), unix.ESRCH) {
		t.Fatal("service stop returned with a live supervised leader")
	}
	j, err := approval.OpenJournal(opts.StateDir, opts.Now(), "")
	must(t, err)
	defer j.Close()
	for _, p := range []runProposal{first, second} {
		v, _, err := j.Get(p.ID, opts.Now())
		must(t, err)
		if v.Status != "executed" || v.RunPGID <= 1 || v.RunnerState != approval.RunnerClosed ||
			(p == second && v.RunOutcome.Error != "stopped_at_quit") {
			t.Fatalf("wrong recorded lifecycle: %s, %s, %+v", v.Status, v.RunnerState, v.RunOutcome)
		}
		if (p == first && v.RunPGID != firstCall.PID) || (p == second && v.RunPGID != secondCall.PID) {
			t.Fatal("durable launch permit did not record the runner's group")
		}
		dir := filepath.Join(opts.StateDir, "runs", p.Runner)
		_, err = approval.CheckPrivate(dir, os.ModeDir|0o700)
		must(t, err)
		_, err = approval.CheckPrivate(filepath.Join(dir, "output"), 0o600)
		must(t, err)
		if _, err := os.Lstat(filepath.Join(dir, "prompt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the duplicate prompt was kept")
		}
	}
}

func TestServeStopsAnActiveRunWithoutStartingAnything(t *testing.T) {
	opts, a, _ := runOptions(t)
	key, prf := registerFixturePasskey(t, opts.StateDir)
	web, _ := signInBrowser(t, opts)
	p := proposeRun(t, opts.StateDir, "a", "stop fixture")
	web.runCSRF(p)
	page := web.freezeRun(p, "", 200)
	web.request(http.MethodPost, "/approve", web.form(key, prf, page), 200)
	call := waitInvocation(t, a)
	web.waitFor(p.ID, "Stop run")
	web.request(http.MethodPost, "/stop-run", url.Values{"id": {p.ID}}, 403)
	web.request(http.MethodPost, "/stop-run", url.Values{"id": {p.ID}, "csrf": {web.csrf}}, 303)
	web.waitFor(p.ID, "runner_closed")
	if !errors.Is(unix.Kill(call.PID, 0), unix.ESRCH) {
		t.Fatal("run closed without reaping its leader")
	}
	_, page = web.request(http.MethodGet, "/p/"+p.ID, nil, 200)
	if !strings.Contains(page, "<code>stopped</code>") {
		t.Fatal("manual stop reason not recorded")
	}
}

func TestServeAgentRunRefusesReplacementCheckout(t *testing.T) {
	opts, a, _ := runOptions(t)
	key, prf := registerFixturePasskey(t, opts.StateDir)
	web, _ := signInBrowser(t, opts)
	p := proposeRun(t, opts.StateDir, "a", "never run outside the approved checkout")
	web.runCSRF(p)
	page := web.freezeRun(p, "", 200)
	must(t, os.Rename(a, a+"-old"))
	must(t, os.Mkdir(a, 0o700))
	must(t, os.WriteFile(filepath.Join(a, "state"), []byte(opts.StateDir), 0o600))
	web.request(http.MethodPost, "/approve", web.form(key, prf, page), 200)
	web.waitFor(p.ID, "checkout_changed")
	if _, err := os.Stat(filepath.Join(opts.StateDir, "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a swapped checkout prepared an execution")
	}
}

func TestServeAgentRunDeliversReportToSignal(t *testing.T) {
	opts, dir, _ := runOptions(t)
	key, prf := registerFixturePasskey(t, opts.StateDir)
	const person = "act_01m6a6ft00e40r2081040g2081"
	const actor = "act_01m6a6ft00e40r2081040g2082"
	const token = "sig_fixture_record_key"
	posts := make(chan approval.Object, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/app-records" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("wrong record endpoint or credential")
			w.WriteHeader(400)
			return
		}
		b, err := io.ReadAll(r.Body)
		o, parseErr := approval.ParseObject(b)
		if err != nil || parseErr != nil {
			t.Error("invalid record body")
			w.WriteHeader(400)
			return
		}
		select {
		case posts <- o:
		default:
			t.Error("too many record attempts")
		}
		w.WriteHeader(201)
		io.WriteString(w, `{"event_id":"evt_01m6a6ft00e81840g2081040g2","replayed":false}`)
	}))
	t.Cleanup(server.Close)
	settings, err := json.Marshal(actionexec.Settings{Signal: &actionexec.SignalSettings{Origin: server.URL, Actor: actor, Person: person, Handle: "fixture"}})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(opts.StateDir, "action-providers.json"), settings, 0o600))
	must(t, vault.Create(opts.StateDir))
	must(t, vault.Put(opts.StateDir, vault.Binding{Service: "signal", Origin: server.URL, Account: actor, Use: "records"}, token))
	web, _ := signInBrowser(t, opts)
	p := proposeRun(t, opts.StateDir, "a", "report fixture")
	web.runCSRF(p)
	page := web.freezeRun(p, "", 200)
	web.request(http.MethodPost, "/approve", web.form(key, prf, page), 200)
	waitInvocation(t, dir)
	must(t, os.WriteFile(filepath.Join(dir, "finish"), nil, 0o600))
	var seen []string
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			t.Fatalf("did not deliver ordered run records: %v", seen)
		case o := <-posts:
			m := o["metadata"].(approval.Object)
			if m["proposal"] != p.ID {
				continue // includes the child's first proposal
			}
			kind := o["type"].(string)
			seen = append(seen, kind)
			if o["subject"] != "agent-run:fixture:"+strings.TrimPrefix(p.Runner, "run_") || m["person"] != person {
				t.Fatal("wrong Signal run subject")
			}
			if kind == "app.runner.finished" {
				s := o["sealed"].(approval.Object)
				text := s["metadata"].(approval.Object)["report"]
				if m["report"] != nil || m["from"] != "runner" || m["runner_id"] != p.Runner ||
					s["level"] != "owner+agents" || text != "Done in the approved checkout." {
					t.Fatal("the final report was not sent sealed and attributed")
				}
			}
			if kind == "app.runner.closed" {
				want := []string{"app.action.proposed", "app.action.approved", "app.action.started", "app.action.executed",
					"app.runner.working", "app.runner.finished", "app.runner.closed"}
				if !reflect.DeepEqual(seen, want) {
					t.Fatalf("runner outbox order: %v", seen)
				}
				return
			}
		}
	}
}
