package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/cursor"
	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/egress/egresstest"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
	"github.com/CrossTerra/beacon/internal/slack/slacktest"
)

const slackConfig = `signal:
  url: https://signal.example.com
person:
  handle: "@winton"
  identities:
    slack: slack:user:U0WINTON
rules: rules.yaml
state: state
mode: dry-run
sources:
`

const slackSource = "  - kind: slack\n    workspace: T0ACME\n    user: U0WINTON\n    channels: [C0REVIEW]\n"

// slackApp is an app whose Slack is the fake, and whose transport fails the test on any host not on the egress list.
func slackApp(t *testing.T, srv *slacktest.Server, store keychain.Store, rec **egresstest.Recorder, stdin string, out, errOut *strings.Builder) *app {
	return &app{
		stdin: strings.NewReader(stdin), stdout: out, stderr: errOut,
		now:      func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
		keychain: func() (keychain.Store, error) { return store, nil },
		transport: func(allowed []string) http.RoundTripper {
			*rec = egresstest.New(t, allowed, nil)
			return (*rec).Wrap(egress.Guard(allowed, http.DefaultTransport))
		},
		slackBase: srv.URL,
		sleep:     func(context.Context, time.Duration) error { return nil },
	}
}

func TestSourceAddSlack(t *testing.T) {
	srv := slacktest.New(t)
	srv.Scopes = slacktest.AllScopes + ",chat:write"
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte(strings.Replace(slackConfig, "  identities:\n    slack: slack:user:U0WINTON\n", "", 1)+"  []\n"), 0o600))
	store := keychain.NewMemory()
	var out, errOut strings.Builder
	var rec *egresstest.Recorder
	add := func(token string, args ...string) int {
		out.Reset()
		errOut.Reset()
		a := slackApp(t, srv, store, &rec, token+"\n", &out, &errOut)
		return a.main(context.Background(), append([]string{"source", "add", "slack", "--config", cfg}, args...))
	}

	// A bot token is refused before Slack is asked, and nothing is stored.
	if code := add("xoxb-1-2-bot"); code == 0 || !strings.Contains(errOut.String(), "not a Slack user token") || len(srv.Requests()) != 0 {
		t.Fatalf("bot token: exit %d\n%s", code, errOut.String())
	}
	// A token Slack refuses.
	if code := add("xoxp-wrong"); code == 0 || !strings.Contains(errOut.String(), "nothing was stored") {
		t.Fatalf("refused token: exit %d\n%s", code, errOut.String())
	}
	if _, err := store.Get(keychain.SlackAccount(slacktest.Workspace, slacktest.User)); err == nil {
		t.Fatal("a refused token was stored")
	}

	if code := add(slacktest.Token, "--channel", "C0REVIEW"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
	}
	text := out.String() + errOut.String()
	if strings.Contains(text, slacktest.Token) || strings.Contains(text, "tokenvalue") {
		t.Fatalf("the token was printed:\n%s", text)
	}
	for _, want := range []string{"slack:T0ACME:U0WINTON", "chat:write", "person.identities.slack: slack:user:U0WINTON"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output misses %q:\n%s", want, text)
		}
	}
	tok, err := store.Get("slack:T0ACME:U0WINTON")
	if err != nil {
		t.Fatal("token not stored under the workspace and user")
	}
	h := http.Header{}
	tok.Authorize(h)
	if h.Get("Authorization") != "Bearer "+slacktest.Token {
		t.Fatal("stored the wrong token")
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "xoxp") {
		t.Fatalf("config holds the token:\n%s", data)
	}
	for _, want := range []string{"kind: slack", "workspace: T0ACME", "user: U0WINTON", "- C0REVIEW"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("config misses %s:\n%s", want, data)
		}
	}
	for _, seen := range rec.Seen() {
		if seen != strings.TrimPrefix(srv.URL, "http://") {
			t.Fatalf("contacted %s", seen)
		}
	}
	for _, r := range srv.Requests() {
		if r.URL.Path != "/api/auth.test" {
			t.Fatalf("source add called %s", r.URL.Path)
		}
	}

	// A token missing a read scope is refused, and the scopes are named.
	srv.Scopes = "search:read,channels:history"
	store2 := keychain.NewMemory()
	store = store2
	if code := add(slacktest.Token); code == 0 || !strings.Contains(errOut.String(), "groups:history, im:history, mpim:history") {
		t.Fatalf("missing scope: exit %d\n%s", code, errOut.String())
	}
	if _, err := store2.Get("slack:T0ACME:U0WINTON"); err == nil {
		t.Fatal("a token missing scopes was stored")
	}
}

func TestEgressListsSlack(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte(slackConfig+slackSource), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte("rules: []\n"), 0o600))
	var out strings.Builder
	a := &app{stdout: &out, stderr: &out}
	if code := a.main(context.Background(), []string{"egress", "--config", cfg}); code != 0 {
		t.Fatal(out.String())
	}
	if got := out.String(); !strings.Contains(got, "slack.com:443") || !strings.Contains(got, "source slack") {
		t.Fatalf("egress:\n%s", got)
	}
}

// slackSetup writes a config with the Slack source and the starter rules, against the fake Slack and a fake Signal.
func slackSetup(t *testing.T, send bool) (srv *slacktest.Server, sig *signaltest.Server, dir string, run func(args ...string) string) {
	t.Helper()
	srv = slacktest.New(t)
	sig = signaltest.New("sig-key")
	t.Cleanup(sig.Close)
	dir = t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	conf := strings.Replace(slackConfig, "https://signal.example.com", sig.URL+"\n  keychain_account: test", 1)
	if send {
		conf = strings.Replace(conf, "mode: dry-run", "mode: send", 1)
	}
	must(t, os.WriteFile(cfg, []byte(conf+slackSource), 0o600))
	rulesFile, err := os.ReadFile(filepath.Join("..", "..", "examples", "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), rulesFile, 0o600))
	store := keychain.NewMemory()
	store.Put("test", "sig-key")
	store.Put(keychain.SlackAccount(slacktest.Workspace, slacktest.User), slacktest.Token)
	var out, errOut strings.Builder
	var rec *egresstest.Recorder
	a := slackApp(t, srv, store, &rec, "", &out, &errOut)
	a.configureClient = func(c *signal.Client) { c.Attempts = 1 }
	run = func(args ...string) string {
		t.Helper()
		out.Reset()
		errOut.Reset()
		if code := a.main(context.Background(), append(args, "--config", cfg)); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
		}
		text := out.String() + errOut.String()
		if strings.Contains(text, slacktest.Token) {
			t.Fatal("the token was printed")
		}
		for _, words := range []string{"deploy", "budget", "offsite", "alerts", "PTAL"} {
			if strings.Contains(errOut.String(), words) {
				t.Fatalf("the log holds message text:\n%s", errOut.String())
			}
		}
		return out.String()
	}
	return srv, sig, dir, run
}

func TestSlackStarterRules(t *testing.T) {
	_, sig, _, run := slackSetup(t, false)
	report := run("run", "--first", "--once")
	want := map[string]string{
		"slack:D0DANA:1794132000.000100":   "send",
		"slack:D0DANA:1794132600.000200":   "send",
		"slack:D0DANA:1794133200.000300":   "send",
		"slack:D0BOT:1794131000.000050":    "never",
		"slack:G0GROUP:1794136800.001200":  "send",
		"slack:C0ENG:1794133800.000400":    "send",
		"slack:C0ENG:1794130000.000600":    "send",
		"slack:C0ENG:1794134400.000700":    "never",
		"slack:C0REVIEW:1794136200.001100": "send",
		"slack:C0REVIEW:1794135000.000900": "never",
	}
	got := map[string]string{}
	bodies := map[string]string{}
	lines := strings.Split(report, "\n")
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) >= 2 && (f[0] == "send" || f[0] == "hold" || f[0] == "never") {
			got[f[1]] = f[0]
			for _, next := range lines[i+1:] {
				if !strings.HasPrefix(next, "       ") {
					break
				}
				bodies[f[1]] += next
			}
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %v\n%s", got, report)
	}
	for _, words := range []string{`"body"`, "deploy", "budget", "offsite", "alerts", "PTAL", "Merged"} {
		if strings.Contains(report, words) {
			t.Fatalf("message text would leave (%s):\n%s", words, report)
		}
	}
	dm := bodies["slack:D0DANA:1794132000.000100"]
	for _, want := range []string{`"subject":"slack:dm:winton:D0DANA:1794132000.000100"`, `"to":["@winton"]`, `"priority":"high"`, `"title":"Direct message"`, `"actor":"slack:user:U0DANA"`} {
		if !strings.Contains(dm, want) {
			t.Fatalf("direct message misses %s: %s", want, dm)
		}
	}
	// Winton's top-level reply to Dana closes both of her asks, and leaves as its link only.
	own := bodies["slack:D0DANA:1794133200.000300"]
	if !strings.Contains(own, `"closes":[{"key":"slack:D0DANA:1794132000.000100"},{"key":"slack:D0DANA:1794132600.000200"}]`) ||
		!strings.Contains(own, `"by":{"identity":"slack:user:U0WINTON"}`) || !strings.Contains(own, `"fields":{"permalink":`) ||
		strings.Contains(own, `"channel"`) || strings.Contains(own, `"to"`) {
		t.Fatalf("own reply: %s", own)
	}
	if mention := bodies["slack:C0ENG:1794133800.000400"]; !strings.Contains(mention, `"subject":"slack:C0ENG:1794133800.000400"`) || !strings.Contains(mention, `"priority":"medium"`) {
		t.Fatalf("mention: %s", mention)
	}
	if !strings.Contains(report, "slack: dm_received 3 mentioned 2 own_message 3 review_request 1 thread_reply 1") {
		t.Fatalf("summary:\n%s", report)
	}
	if len(sig.Requests()) != 0 {
		t.Fatal("a dry run contacted Signal")
	}
}

// Direct messages are personal: a send pass holds them until Signal restricts slack:dm:*, and sends channel items.
func TestSlackDirectMessagesWaitForRestriction(t *testing.T) {
	_, sig, dir, run := slackSetup(t, true)
	run("run", "--first", "--once")
	if st, _ := cursor.Read(filepath.Join(dir, "state")); st != nil {
		if _, ok := st.Get("slack"); ok {
			t.Fatal("a first run stored a cursor")
		}
	}
	run("accept-first")
	sig.Restrict("gmail:*")
	report := run("run", "--once")
	if !strings.Contains(report, "source slack: 4 items held, not sent: slack:dm:* is not restricted in Signal") {
		t.Fatalf("report:\n%s", report)
	}
	sent := map[string]bool{}
	for _, e := range sig.Events() {
		sent[e.Key] = true
		if strings.Contains(string(e.Body), `"subject":"slack:dm:`) {
			t.Fatalf("%s was sent under an unrestricted prefix", e.Key)
		}
	}
	for _, k := range []string{"slack:C0ENG:1794133800.000400", "slack:C0ENG:1794130000.000600", "slack:C0REVIEW:1794136200.001100"} {
		if !sent[k] {
			t.Fatalf("channel item %s not sent; sent %v", k, sent)
		}
	}
	held, err := os.ReadFile(filepath.Join(dir, "state", "held.jsonl"))
	if err != nil || !strings.Contains(string(held), "slack:D0DANA:1794132000.000100") {
		t.Fatalf("not held:\n%s", held)
	}
	st, err := cursor.Read(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := st.Get("slack"); !ok || !strings.Contains(string(c), `"since":"2026-11-08T12:00:00Z"`) {
		t.Fatalf("slack cursor %q", c)
	}
}

func TestSlackDirectMessagesSentWhenRestricted(t *testing.T) {
	_, sig, _, run := slackSetup(t, true)
	run("run", "--first", "--once")
	run("accept-first")
	sig.Restrict("slack:dm:*")
	run("run", "--once")
	e, ok := sig.Event("slack:D0DANA:1794133200.000300")
	if !ok || !strings.Contains(string(e.Body), `"closes":[{"key":"slack:D0DANA:1794132000.000100"},{"key":"slack:D0DANA:1794132600.000200"}]`) {
		t.Fatalf("own reply %s", e.Body)
	}
	for _, e := range sig.Events() {
		if strings.Contains(string(e.Body), `"body"`) {
			t.Fatalf("a body left: %s", e.Body)
		}
	}
	if _, ok := sig.Event("slack:G0GROUP:1794136800.001200"); !ok {
		t.Fatal("the group mention was not sent")
	}
	if _, ok := sig.Event("slack:D0BOT:1794131000.000050"); ok {
		t.Fatal("a bot message was sent")
	}
	// A second pass sends nothing again.
	n := len(sig.Events())
	run("run", "--once")
	if len(sig.Events()) != n {
		t.Fatal("a second pass sent again")
	}
}
