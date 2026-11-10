package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/cursor"
	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/egress/egresstest"
	"github.com/CrossTerra/beacon/internal/google/googletest"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/service"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
)

const googleConfig = `signal:
  url: https://signal.example.com
person:
  handle: "@winton"
  identities:
    gmail: email:winton@example.com
    calendar: email:winton@example.com
    drive: email:winton@example.com
rules: rules.yaml
state: state
mode: dry-run
sources:
`

// googleSources are the three Google sources for the fixture's account.
const googleSources = "  - kind: gmail\n    account: winton@example.com\n  - kind: calendar\n    account: winton@example.com\n  - kind: drive\n    account: winton@example.com\n"

// googleApp is an app whose Google endpoints are the fake, and whose transport fails the test on any host not on
// the egress list.
func googleApp(t *testing.T, srv *googletest.Server, store keychain.Store, rec **egresstest.Recorder, out, errOut *strings.Builder) *app {
	return &app{
		stdin: strings.NewReader(""), stdout: out, stderr: errOut,
		now:      func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
		keychain: func() (keychain.Store, error) { return store, nil },
		transport: func(allowed []string) http.RoundTripper {
			*rec = egresstest.New(t, allowed, nil)
			return (*rec).Wrap(egress.Guard(allowed, http.DefaultTransport))
		},
		google: googleEndpoints{
			tokenURL: srv.URL + "/token", authURL: "https://accounts.google.com/o/oauth2/v2/auth",
			gmailBase: srv.URL, apiBase: srv.URL,
		},
	}
}

func TestSourceAddGoogle(t *testing.T) {
	srv := googletest.New(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte(googleConfig+"  []\n"), 0o600))
	clientFile := filepath.Join(dir, "client.json")
	must(t, os.WriteFile(clientFile, []byte(googletest.ClientFile), 0o600))
	store := keychain.NewMemory()
	var out, errOut strings.Builder
	var rec *egresstest.Recorder
	a := googleApp(t, srv, store, &rec, &out, &errOut)
	var asked []string
	a.google.open = func(consent string) error {
		u, _ := url.Parse(consent)
		q := u.Query()
		asked = strings.Fields(q.Get("scope"))
		if q.Get("login_hint") != googletest.Email {
			t.Errorf("login_hint %q", q.Get("login_hint"))
		}
		srv.Challenge = q.Get("code_challenge")
		go func() {
			resp, err := http.Get(fmt.Sprintf("%s?code=%s&state=%s", q.Get("redirect_uri"), srv.Code, url.QueryEscape(q.Get("state"))))
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	if code := a.main(context.Background(), []string{"source", "add", "google", "--config", cfg, "--client-file", clientFile, "--open"}); code == 0 || !strings.Contains(errOut.String(), "--account") {
		t.Fatalf("no --account: exit %d\n%s", code, errOut.String())
	}
	errOut.Reset()
	if code := a.main(context.Background(), []string{"source", "add", "google", "--config", cfg, "--account", "Winton@Example.com", "--client-file", clientFile, "--open"}); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
	}
	if strings.Join(asked, " ") != googletest.AllScopes {
		t.Fatalf("asked for %v", asked)
	}
	text := out.String() + errOut.String()
	for _, secret := range []string{googletest.ClientSecret, googletest.Refresh, googletest.Access, srv.Code} {
		if strings.Contains(text, secret) {
			t.Fatalf("a credential was printed:\n%s", text)
		}
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{googletest.ClientSecret, googletest.Refresh, googletest.ClientID} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("config holds a credential:\n%s", data)
		}
	}
	for _, kind := range []string{"kind: gmail", "kind: calendar", "kind: drive", "account: winton@example.com"} {
		if !strings.Contains(string(data), kind) {
			t.Fatalf("missing %s:\n%s", kind, data)
		}
	}
	if _, err := store.Get("google:winton@example.com:client"); err != nil {
		t.Fatal("client not stored")
	}
	refresh, err := store.Get("google:winton@example.com:refresh")
	if err != nil {
		t.Fatal("refresh token not stored")
	}
	h := http.Header{}
	refresh.Authorize(h)
	if h.Get("Authorization") != "Bearer "+googletest.Refresh {
		t.Fatal("stored the wrong refresh token")
	}
	for _, seen := range rec.Seen() {
		if seen != strings.TrimPrefix(srv.URL, "http://") {
			t.Fatalf("contacted %s", seen)
		}
	}

	// Again with only --gmail and no client file: the stored client is used, and the grant still covers every
	// Google source in the config.
	out.Reset()
	errOut.Reset()
	if code := a.main(context.Background(), []string{"source", "add", "google", "--config", cfg, "--account", googletest.Email, "--gmail", "--open"}); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
	}
	if strings.Join(asked, " ") != googletest.AllScopes {
		t.Fatalf("asked for %v", asked)
	}
}

func TestSourceAddGoogleNeedsClient(t *testing.T) {
	srv := googletest.New(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte(googleConfig+"  []\n"), 0o600))
	var out, errOut strings.Builder
	var rec *egresstest.Recorder
	a := googleApp(t, srv, keychain.NewMemory(), &rec, &out, &errOut)
	if code := a.main(context.Background(), []string{"source", "add", "google", "--config", cfg, "--account", googletest.Email}); code == 0 || !strings.Contains(errOut.String(), "--client-file") {
		t.Fatalf("exit %d\n%s", code, errOut.String())
	}
	web := filepath.Join(dir, "web.json")
	must(t, os.WriteFile(web, []byte(`{"web":{"client_id":"a","client_secret":"b"}}`), 0o600))
	errOut.Reset()
	if code := a.main(context.Background(), []string{"source", "add", "google", "--config", cfg, "--account", googletest.Email, "--client-file", web}); code == 0 || !strings.Contains(errOut.String(), "Desktop app") {
		t.Fatalf("exit %d\n%s", code, errOut.String())
	}
}

func TestGoogleStarterRules(t *testing.T) {
	srv := googletest.New(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte(googleConfig+googleSources), 0o600))
	rulesFile, err := os.ReadFile(filepath.Join("..", "..", "examples", "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), rulesFile, 0o600))
	store := keychain.NewMemory()
	store.Put(keychain.GoogleClientAccount(googletest.Email), `{"client_id":"`+googletest.ClientID+`","client_secret":"`+googletest.ClientSecret+`"}`)
	store.Put(keychain.GoogleRefreshAccount(googletest.Email), googletest.Refresh)
	var out, errOut strings.Builder
	var rec *egresstest.Recorder
	a := googleApp(t, srv, store, &rec, &out, &errOut)

	run := func(args ...string) string {
		t.Helper()
		out.Reset()
		errOut.Reset()
		if code := a.main(context.Background(), append(args, "--config", cfg)); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
		}
		for _, secret := range []string{googletest.ClientSecret, googletest.Refresh, googletest.Access} {
			if strings.Contains(out.String()+errOut.String(), secret) {
				t.Fatal("a credential was printed")
			}
		}
		if strings.Contains(errOut.String(), "Budget review") || strings.Contains(errOut.String(), "dana@example.com") {
			t.Fatal("the log holds item content")
		}
		for _, seen := range rec.Seen() {
			if strings.Contains(seen, "signal.example.com") {
				t.Fatal("a dry run contacted Signal")
			}
		}
		return out.String()
	}
	tiers := func(report string) map[string]string {
		got := map[string]string{}
		for _, line := range strings.Split(report, "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && (f[0] == "send" || f[0] == "hold" || f[0] == "never") {
				got[f[1]] = f[0]
			}
		}
		return got
	}
	check := func(report string, want map[string]string) {
		t.Helper()
		got := tiers(report)
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %q, want %q\n%s", k, got[k], v, report)
			}
		}
		if len(got) != len(want) {
			t.Errorf("got %v, want %v", got, want)
		}
		if strings.Contains(report, `"body"`) || strings.Contains(report, "Can you review") || strings.Contains(report, "please check") {
			t.Errorf("a body would leave:\n%s", report)
		}
	}

	first := run("run", "--first", "--once")
	check(first, map[string]string{
		"gmail:t1:m2":                       "send",
		"gmail:t1:starred:m1":               "send",
		"gmail:t2:m3":                       "never",
		"gmail:t4:m5":                       "never",
		"calendar:ev1:response_needed:3001": "send",
		"calendar:ev2:upcoming:3002":        "send",
		"calendar:ev4:upcoming:3004":        "send",
		"drive:f1:comment:c2":               "send",
		"drive:f2:comment:c7":               "send",
		"drive:notes:n1":                    "send",
	})
	for _, line := range strings.Split(first, "\n") {
		if strings.Contains(line, `"key":"gmail:t1:m2"`) && (strings.Contains(line, "Budget") || strings.Contains(line, "dana@") || !strings.Contains(line, `"link"`)) {
			t.Fatalf("an own sent mail leaves more than its link and ids: %s", line)
		}
	}
	if !strings.Contains(first, `"to":["@winton"]`) || !strings.Contains(first, `"subject":"calendar:winton:ev1"`) || !strings.Contains(first, `"due_at":"2026-11-09T17:00:00.000Z"`) {
		t.Fatalf("the response-needed ask is not in the report:\n%s", first)
	}
	if !strings.Contains(first, "gmail: received 2 sent 1 starred 1") {
		t.Fatalf("summary:\n%s", first)
	}
	for _, path := range []string{"/gmail/v1/users/me/messages/m1", "/drive/v3/files/n1/export"} {
		if srv.Called(path) {
			t.Fatalf("%s was read though no rule lists body", path)
		}
	}
	if q := srv.Query("/drive/v3/files/f1/comments"); strings.Contains(q.Get("fields"), "content") {
		t.Fatal("comment text was asked for though no rule lists body")
	}

	st, err := cursor.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, st.Commit("gmail", []byte("100")))
	must(t, st.Commit("drive", []byte("2026-11-08T10:00:00Z")))
	must(t, st.Commit("calendar", []byte(`{"since":"2026-11-08T11:00:00Z","events":{}}`)))
	later := run("run", "--dry-run", "--once")
	check(later, map[string]string{
		"gmail:t1:m1":                       "hold",
		"gmail:t1:m2":                       "send",
		"gmail:t2:m3":                       "never",
		"gmail:t4:m5":                       "never",
		"gmail:t1:starred:m1":               "send",
		"calendar:ev1:changed:3101":         "send",
		"calendar:ev2:changed:3102":         "send",
		"calendar:ev3:changed:3103":         "send",
		"calendar:ev4:cancelled:3104":       "send",
		"calendar:ev5:created:3105":         "send",
		"calendar:ev6:response_needed:3106": "send",
		"drive:f1:comment:c1":               "send",
		"drive:f1:comment:c2":               "send",
		"drive:f1:comment:c3":               "send",
		"drive:f1:reply:c1:r1":              "send",
		"drive:f1:reply:c1:r2":              "send",
		"drive:f2:comment:c7":               "send",
		"drive:notes:n1":                    "send",
		"gmail:t5:m6":                       "hold",
	})
	if !strings.Contains(later, `"by":{"identity":"email:winton@example.com"}`) {
		t.Fatalf("own acts carry no by:\n%s", later)
	}

	egressOut := run("egress")
	for _, host := range []string{strings.TrimPrefix(srv.URL, "http://"), "signal.example.com:443"} {
		if !strings.Contains(egressOut, host) {
			t.Fatalf("egress misses %s:\n%s", host, egressOut)
		}
	}
}

func TestEgressListsGoogleHosts(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte(googleConfig+googleSources), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte("rules: []\n"), 0o600))
	var out strings.Builder
	a := &app{stdout: &out, stderr: &out}
	if code := a.main(context.Background(), []string{"egress", "--config", cfg}); code != 0 {
		t.Fatal(out.String())
	}
	got := out.String()
	for _, host := range []string{"oauth2.googleapis.com:443", "gmail.googleapis.com:443", "www.googleapis.com:443"} {
		if !strings.Contains(got, host) {
			t.Fatalf("missing %s in\n%s", host, got)
		}
	}
	if strings.Contains(got, "accounts.google.com") {
		t.Fatalf("Beacon does not contact the consent page; the browser does:\n%s", got)
	}
}

func TestGoogleNeedsHandle(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte(strings.Replace(googleConfig, `"@winton"`, "act_123", 1)+"  - kind: gmail\n    account: winton@example.com\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte("rules: []\n"), 0o600))
	var out strings.Builder
	a := &app{stdout: &out, stderr: &out}
	if code := a.main(context.Background(), []string{"egress", "--config", cfg}); code == 0 || !strings.Contains(out.String(), "@handle") {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
}

// TestGoogleSendCommitsCursors makes a first run, accepts it, sends, and sends again: every Google cursor is stored
// only by the send pass, the second pass reads from it, and no key is sent twice.
func TestGoogleSendCommitsCursors(t *testing.T) {
	srv := googletest.New(t)
	sig := signaltest.New("sig-key")
	t.Cleanup(sig.Close)
	sig.Restrict("gmail:*", "calendar:*", "drive:*")
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	conf := strings.Replace(googleConfig, "https://signal.example.com", sig.URL+"\n  keychain_account: test", 1)
	conf = strings.Replace(conf, "mode: dry-run", "mode: send", 1)
	must(t, os.WriteFile(cfg, []byte(conf+googleSources), 0o600))
	rulesFile, err := os.ReadFile(filepath.Join("..", "..", "examples", "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), rulesFile, 0o600))
	store := keychain.NewMemory()
	store.Put("test", "sig-key")
	store.Put(keychain.GoogleClientAccount(googletest.Email), `{"client_id":"`+googletest.ClientID+`","client_secret":"`+googletest.ClientSecret+`"}`)
	store.Put(keychain.GoogleRefreshAccount(googletest.Email), googletest.Refresh)
	var out, errOut strings.Builder
	var rec *egresstest.Recorder
	a := googleApp(t, srv, store, &rec, &out, &errOut)
	run := func(args ...string) {
		t.Helper()
		out.Reset()
		errOut.Reset()
		if code := a.main(context.Background(), append(args, "--config", cfg)); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
		}
	}
	cursors := func() *cursor.Store {
		st, err := cursor.Read(filepath.Join(dir, "state"))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	run("run", "--first", "--once")
	if _, ok := cursors().Get("gmail"); ok || len(sig.Events()) != 0 {
		t.Fatal("a first run stored a cursor or sent")
	}
	run("accept-first")
	run("run", "--once")
	gm, ok := cursors().Get("gmail")
	if !ok || !strings.Contains(string(gm), `"history_id":"300"`) {
		t.Fatalf("gmail cursor %q", gm)
	}
	if d, ok := cursors().Get("drive"); !ok || string(d) != "2026-11-08T12:00:00Z" {
		t.Fatalf("drive cursor %q", d)
	}
	if c, ok := cursors().Get("calendar"); !ok || !strings.Contains(string(c), `"ev3"`) {
		t.Fatalf("calendar cursor %q", c)
	}
	sent := map[string]int{}
	for _, e := range sig.Events() {
		sent[e.Key]++
	}
	if sent["gmail:t1:m2"] != 1 || sent["calendar:ev1:response_needed:3001"] != 1 || sent["drive:f1:comment:c2"] != 1 {
		t.Fatalf("sent %v", sent)
	}
	run("run", "--once")
	if q := srv.Query("/gmail/v1/users/me/history"); q.Get("startHistoryId") != "300" {
		t.Fatalf("history read from %v", q)
	}
	sent = map[string]int{}
	for _, e := range sig.Events() {
		sent[e.Key]++
		if sent[e.Key] > 1 {
			t.Fatalf("%s sent twice", e.Key)
		}
		if strings.Contains(string(e.Body), `"body"`) {
			t.Fatalf("a body left: %s", e.Body)
		}
	}
	if sent["calendar:ev6:response_needed:3106"] != 1 || sent["calendar:ev1:responded:3101"] != 1 {
		t.Fatalf("second pass sent %v", sent)
	}
	if q := srv.Query("/drive/v3/files/f1/comments"); q.Get("startModifiedTime") != "2026-11-08T11:45:00Z" {
		t.Fatalf("drive read from %v", q)
	}
	if gm, _ := cursors().Get("gmail"); !strings.Contains(string(gm), `"history_id":"350"`) {
		t.Fatalf("gmail cursor %q", gm)
	}
}

// googleSendSetup is a send-mode config with the three Google sources and the starter rules, against the fake
// Google and the fake Signal. run fails the test on a non-zero exit and returns the report.
func googleSendSetup(t *testing.T) (sig *signaltest.Server, dir string, a *app, run func(args ...string) string) {
	t.Helper()
	srv := googletest.New(t)
	sig = signaltest.New("sig-key")
	t.Cleanup(sig.Close)
	dir = t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	conf := strings.Replace(googleConfig, "https://signal.example.com", sig.URL+"\n  keychain_account: test", 1)
	conf = strings.Replace(conf, "mode: dry-run", "mode: send", 1)
	must(t, os.WriteFile(cfg, []byte(conf+googleSources), 0o600))
	rulesFile, err := os.ReadFile(filepath.Join("..", "..", "examples", "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), rulesFile, 0o600))
	store := keychain.NewMemory()
	store.Put("test", "sig-key")
	store.Put(keychain.GoogleClientAccount(googletest.Email), `{"client_id":"`+googletest.ClientID+`","client_secret":"`+googletest.ClientSecret+`"}`)
	store.Put(keychain.GoogleRefreshAccount(googletest.Email), googletest.Refresh)
	var out, errOut strings.Builder
	var rec *egresstest.Recorder
	a = googleApp(t, srv, store, &rec, &out, &errOut)
	a.configureClient = func(c *signal.Client) { c.Attempts = 1 }
	run = func(args ...string) string {
		t.Helper()
		out.Reset()
		errOut.Reset()
		if code := a.main(context.Background(), append(args, "--config", cfg)); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
		}
		return out.String() + errOut.String()
	}
	run("run", "--first", "--once")
	run("accept-first")
	return sig, dir, a, run
}

func personalEvents(sig *signaltest.Server) []string {
	var out []string
	for _, e := range sig.Events() {
		for _, stem := range []string{`"subject":"gmail:`, `"subject":"calendar:`, `"subject":"drive:`} {
			if strings.Contains(string(e.Body), stem) {
				out = append(out, e.Key)
			}
		}
	}
	return out
}

func TestPersonalPrefixesMustBeRestricted(t *testing.T) {
	sig, dir, _, run := googleSendSetup(t)
	// Only calendar:* is restricted, and gmail has a longer stem under which Signal would read "thread" as the
	// owner. Gmail and Drive send items are held, and only Calendar's are sent.
	sig.Restrict("calendar:*", "gmail:winton:*")
	report := run("run", "--once")
	for _, want := range []string{
		"source gmail: 2 items held, not sent: gmail:* is not restricted in Signal",
		"source drive: 3 items held, not sent: drive:* is not restricted in Signal",
		"held, not sent: its subject prefix is not restricted in Signal",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report misses %q:\n%s", want, report)
		}
	}
	if !strings.Contains(report, `"msg":"withheld","source":"gmail","items":2,"reason":"prefix_unrestricted"`) {
		t.Fatalf("log:\n%s", report)
	}
	for _, k := range personalEvents(sig) {
		if !strings.HasPrefix(k, "calendar:") {
			t.Fatalf("%s was sent under an unrestricted prefix", k)
		}
	}
	if len(personalEvents(sig)) == 0 {
		t.Fatal("calendar, which is restricted, sent nothing")
	}
	// service status reads the same reasons from the pass record.
	pass, err := service.ReadPass(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	status := service.FormatStatus(service.Job{}, pass, nil, "none")
	if !strings.Contains(status, "withheld: gmail gmail:* prefix_unrestricted\n") || !strings.Contains(status, "withheld: drive drive:* prefix_unrestricted\n") || strings.Contains(status, "calendar") {
		t.Fatalf("status:\n%s", status)
	}
	held, err := os.ReadFile(filepath.Join(dir, "state", "held.jsonl"))
	if err != nil || !strings.Contains(string(held), "gmail:t1:starred:m1") || !strings.Contains(string(held), "drive:f1:comment:c2") {
		t.Fatalf("not held:\n%s", held)
	}
	// Held is a decision: the cursors move, and a later pass does not decide those keys again.
	st, err := cursor.Read(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("gmail"); !ok {
		t.Fatal("gmail cursor did not move")
	}
}

func TestUnreadableRestrictionsSendNothingPersonal(t *testing.T) {
	sig, dir, _, run := googleSendSetup(t)
	sig.NoRead = true
	report := run("run", "--once")
	if !strings.Contains(report, "Signal's restricted prefixes could not be read (no_read_scope). Beacon's key needs the read scope.") ||
		!strings.Contains(report, `"msg":"restrictions_unread","reason":"no_read_scope"`) {
		t.Fatalf("report:\n%s", report)
	}
	if k := personalEvents(sig); len(k) != 0 {
		t.Fatalf("sent %v without proof the prefixes are restricted", k)
	}
	if pass, err := service.ReadPass(filepath.Join(dir, "state")); err != nil || len(pass.Withheld) != 3 || pass.Withheld[0].Reason != service.WithheldUnknown {
		t.Fatalf("pass %+v %v", pass, err)
	}
	st, err := cursor.Read(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gmail", "calendar", "drive"} {
		if _, ok := st.Get(name); ok {
			t.Fatalf("%s cursor moved past items it did not record", name)
		}
	}
	if held, _ := os.ReadFile(filepath.Join(dir, "state", "held.jsonl")); strings.Contains(string(held), "gmail:t1:starred:m1") {
		t.Fatal("an unverified send was held for good; it should wait for a pass that can read the restrictions")
	}
	// Once Signal answers and the prefixes are restricted, the same items are decided and sent.
	sig.NoRead = false
	sig.Restrict("gmail:*", "calendar:*", "drive:*")
	run("run", "--once")
	if _, ok := sig.Event("gmail:t1:starred:m1"); !ok {
		t.Fatal("not sent once the prefixes were restricted")
	}
	st, _ = cursor.Read(filepath.Join(dir, "state"))
	if _, ok := st.Get("gmail"); !ok {
		t.Fatal("gmail cursor did not move")
	}
}

func TestPendingPersonalBodiesWaitForTheRestriction(t *testing.T) {
	sig, _, _, run := googleSendSetup(t)
	sig.Restrict("gmail:*", "calendar:*", "drive:*")
	// Signal is down for events: everything is appended and stays pending.
	sig.Script = func(_ int, req signaltest.Request) signaltest.Reply {
		if strings.HasPrefix(req.Key, "claim:") {
			return signaltest.Reply{}
		}
		return signaltest.Reply{Status: 503, Body: `{"error":{"code":"unavailable","message":"down"}}`}
	}
	run("run", "--once")
	if len(personalEvents(sig)) != 0 {
		t.Fatal("sent while Signal was down")
	}
	// The gmail restriction is lifted before the pending bodies go.
	sig.Script = nil
	sig.Restrict("calendar:*", "drive:*")
	report := run("run", "--once")
	if !strings.Contains(report, "Pending bodies on personal subjects not sent: 2.") {
		t.Fatalf("report:\n%s", report)
	}
	for _, k := range personalEvents(sig) {
		if strings.HasPrefix(k, "gmail:") {
			t.Fatalf("%s was sent after gmail:* was lifted", k)
		}
	}
	if _, ok := sig.Event("drive:f1:comment:c2"); !ok {
		t.Fatal("drive, still restricted, was not sent")
	}
}
