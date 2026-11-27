package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/localstate"
)

const (
	checkOrigin     = "https://signal.fixture.test"
	checkPerson     = "act_01m6a6ft00e40r2081040g2081"
	checkApp        = "act_01m6a6ft00e40r2081040g2082"
	checkRelay      = "act_01m6a6ft00e40r2081040g2083"
	checkOther      = "act_01m6a6ft00e40r2081040g2084"
	checkAppKeyTag  = "01m6a6ft00e40r2081040g2088"
	checkRelayTag   = "01m6a6ft00e40r2081040g2085"
	checkRelayKeyID = "key_" + checkRelayTag
	checkIdentity   = "email:person@example.test"
)

// Deterministic dummy bytes in Signal's wire format, not issued credentials.
var (
	checkAppKey   = "sig_" + checkAppKeyTag + "_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	checkRelayKey = "sig_" + checkRelayTag + "_" + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("R", 32)))
)

const checkGrantPath = "/api/api-keys?limit=100"

type verificationFixture struct {
	t       *testing.T
	appMe   approval.Object
	relayMe approval.Object
	app     approval.Object
	relay   approval.Object
	person  approval.Object
	grant   approval.Object
	calls   []string
	opened  bool
	refuse  string
	status  int
	before  func(string)
}

func signalVerificationFixture(t *testing.T) *verificationFixture {
	t.Helper()
	actor := func(id string, app bool) approval.Object {
		return approval.Object{"id": id, "type": "integration", "works_for": checkPerson, "approval_app": app}
	}
	me := func(id string, app bool, scopes ...string) approval.Object {
		keyID := checkRelayKeyID
		if app {
			keyID = "key_" + checkAppKeyTag
		}
		return approval.Object{"actor": actor(id, app), "scopes": scopes, "curator": false,
			"key": approval.Object{"id": keyID}}
	}
	detail := func(id string, app bool) approval.Object {
		o := actor(id, app)
		o["deactivated_at"], o["directory"], o["executes"], o["reconciles"] = nil, false, []string{}, []string{}
		return o
	}
	return &verificationFixture{t: t, appMe: me(checkApp, true, "write", "read"), relayMe: me(checkRelay, false, "write", "relay", "read"),
		app: detail(checkApp, true), relay: detail(checkRelay, false),
		person: approval.Object{"id": checkPerson,
			"type": "human", "handle": "fixture", "deactivated_at": nil, "identities": []string{checkIdentity, "zitadel:example.test:user:fixture"}},
		grant: approval.Object{"id": checkRelayKeyID, "actor_id": checkRelay,
			"scopes": []string{"read", "write", "relay"}, "identity_prefixes": []string{checkIdentity}, "revoked_at": nil, "signed_in": false}}
}

type signalCheckTransport func(*http.Request) (*http.Response, error)

func (f signalCheckTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (f *verificationFixture) transport(allowed []string) http.RoundTripper {
	f.t.Helper()
	f.opened = true
	if !reflect.DeepEqual(allowed, []string{"signal.fixture.test:443"}) {
		f.t.Fatal("unexpected Signal egress list")
	}
	return signalCheckTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.Body != nil || r.URL.Scheme+"://"+r.URL.Host != checkOrigin || r.URL.User != nil {
			f.t.Fatal("verification did not stay read-only at the pinned origin")
		}
		role := "app"
		switch r.Header.Get("Authorization") {
		case "Bearer " + checkAppKey:
		case "Bearer " + checkRelayKey:
			role = "collector"
		default:
			f.t.Fatal("verification did not use a fixture credential")
		}
		path := r.URL.RequestURI()
		call := role + " " + path
		f.calls = append(f.calls, call)
		if f.before != nil {
			f.before(call)
		}
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		if call == f.refuse {
			return &http.Response{StatusCode: f.status, Header: http.Header{"Location": {"/unexpected"}},
				Body: io.NopCloser(strings.NewReader(checkAppKey + checkRelayKey))}, nil
		}
		var body approval.Object
		switch call {
		case "app /api/me":
			body = f.appMe
		case "app /api/actors/" + checkApp:
			body = f.app
		case "app /api/actors/" + checkPerson, "app /api/actors/" + checkOther:
			body = f.person
		case "collector /api/me":
			body = f.relayMe
		case "collector /api/actors/" + checkRelay, "collector /api/actors/" + checkApp:
			body = f.relay
		case "collector " + checkGrantPath:
			// Another key for the same actor is not evidence of the supplied key's grant.
			other := approval.Object{"id": "key_01m6a6ft00e40r2081040g2086", "actor_id": checkRelay,
				"scopes": []string{"read", "write", "relay"}, "identity_prefixes": []string{checkIdentity}, "revoked_at": nil, "signed_in": false}
			body = approval.Object{"data": []any{other, f.grant}, "next_cursor": nil}
		default:
			f.t.Fatal("verification requested an unexpected endpoint or mixed the credential slots")
		}
		b, err := json.Marshal(body)
		must(f.t, err)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})
}

func verifyInput(t *testing.T, origin, key string) string {
	t.Helper()
	b, err := json.Marshal(approval.Object{"origin": origin, "key": key})
	must(t, err)
	return "beacon-setup:v1:" + base64.RawURLEncoding.EncodeToString(b)
}

func verificationArgs(dir, config, person string) []string {
	return []string{"setup", "verify-signal", "--state-dir", dir, "--config", config, "--signal-origin", checkOrigin, "--person", person}
}

func runSignalCheck(ctx context.Context, t *testing.T, f *verificationFixture, inputs string, args []string) (int, string, string) {
	t.Helper()
	a, stdout, stderr := previewCLI()
	a.euid = func() int { return 501 }
	a.stdin = strings.NewReader(inputs)
	a.transport = f.transport
	code := a.main(ctx, args)
	all := stdout.String() + stderr.String()
	for _, secret := range []string{checkAppKey, checkRelayKey, "beacon-setup:v1:", checkIdentity} {
		if strings.Contains(all, secret) {
			t.Fatal("verification printed credential or API response content")
		}
	}
	return code, stdout.String(), stderr.String()
}

func fixtureTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	must(t, filepath.WalkDir(root, func(path string, ent fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ent.IsDir() {
			out[path] = "<dir>"
			return nil
		}
		b, err := os.ReadFile(path)
		out[path] = string(b)
		return err
	}))
	return out
}

func fixtureLocalSignalPins(t *testing.T, dir, config string) {
	t.Helper()
	must(t, os.Mkdir(dir, 0o700))
	o, err := approval.ParseOrigin(approval.LocalOrigin, "")
	must(t, err)
	must(t, localstate.Save(dir, o))
	f := `{"signal":{"origin":"` + checkOrigin + `","actor_id":"` + checkApp + `","person_id":"` + checkPerson + `","handle":"fixture"},"github":{"origin":"https://github.fixture.test","user_id":1}}`
	must(t, os.WriteFile(filepath.Join(dir, "action-providers.json"), []byte(f), 0o600))
	must(t, os.WriteFile(config, []byte("signal:\n  url: "+checkOrigin+"/\nperson:\n  handle: '@fixture'\nmode: send\nsources: []\nstate: collector\nrules: rules.yaml\n"), 0o600))
	for _, file := range []string{"passkeys.json", "write-vault.json", "unattended.kdbx", "unattended.keyx", "journal.log", "rules.yaml", "cursors.json"} {
		must(t, os.WriteFile(filepath.Join(dir, file), []byte("synthetic untouched "+file), 0o600))
	}
}

func TestSignalOnboardingVerifiesWithoutLocalChanges(t *testing.T) {
	for _, install := range []string{"fresh", "enrolled"} {
		t.Run(install, func(t *testing.T) {
			root := t.TempDir()
			dir, config := filepath.Join(root, "state"), filepath.Join(root, "config.yaml")
			if install == "enrolled" {
				fixtureLocalSignalPins(t, dir, config)
				// Verification is also usable when the service owns the lock. Never open the synthetic vault or journal.
				lock, err := approval.LockInstance(dir)
				must(t, err)
				defer approval.CloseInstance(dir, lock)
			}
			before := fixtureTree(t, root)
			input := verifyInput(t, checkOrigin, checkAppKey) + "\n" + checkRelayKey + "\n"
			for _, selector := range []string{"@fixture", checkPerson} {
				f := signalVerificationFixture(t)
				code, stdout, stderr := runSignalCheck(t.Context(), t, f, input, verificationArgs(dir, config, selector))
				if code != 0 || !strings.Contains(stdout, "Nothing was saved or activated") || !strings.Contains(stderr, "(input hidden)") {
					t.Fatal("a good verification did not succeed in the terminal")
				}
				want := []string{"app /api/me", "app /api/actors/" + checkApp, "app /api/actors/" + checkPerson,
					"collector /api/me", "collector /api/actors/" + checkRelay, "collector " + checkGrantPath}
				if !reflect.DeepEqual(want, f.calls) || !reflect.DeepEqual(before, fixtureTree(t, root)) {
					t.Fatal("verification changed state or did not check both keys")
				}
			}
		})
	}
}

func TestSignalOnboardingRefusesBeforeSendingAnyKey(t *testing.T) {
	for _, scenario := range []string{"wrong code origin", "wrong pinned origin", "wrong collector origin", "wrong pinned person", "nonlocal review", "missing intended person", "same key"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			dir, config := filepath.Join(root, "state"), filepath.Join(root, "config.yaml")
			fixtureLocalSignalPins(t, dir, config)
			codeOrigin, collector := checkOrigin, checkRelayKey
			args := verificationArgs(dir, config, "@fixture")
			switch scenario {
			case "wrong code origin":
				codeOrigin = "https://elsewhere.fixture.test"
			case "wrong pinned origin":
				args[len(args)-3] = "https://elsewhere.fixture.test"
			case "wrong collector origin":
				body, err := os.ReadFile(config)
				must(t, err)
				must(t, os.WriteFile(config, []byte(strings.Replace(string(body), checkOrigin, "https://elsewhere.fixture.test", 1)), 0o600))
			case "wrong pinned person":
				args[len(args)-1] = "@unintended"
			case "nonlocal review":
				must(t, os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"origin":"https://fixture.tail.ts.net","tailscale_login":"fixture@example.test"}`), 0o600))
			case "missing intended person":
				args = args[:len(args)-2]
			case "same key":
				collector = checkAppKey
			}
			before := fixtureTree(t, root)
			f := signalVerificationFixture(t)
			code, stdout, _ := runSignalCheck(t.Context(), t, f, verifyInput(t, codeOrigin, checkAppKey)+"\n"+collector+"\n", args)
			if code == 0 || stdout != "" || f.opened || len(f.calls) != 0 || !reflect.DeepEqual(before, fixtureTree(t, root)) {
				t.Fatal("invalid input reached Signal or modified local state")
			}
		})
	}
}

func TestSignalOnboardingRejectsForeignOrMalformedKeysBeforeHTTP(t *testing.T) {
	const foreign = "xoxp-synthetic-not-a-signal-key"
	badKeys := []struct{ name, key string }{
		{"another provider", foreign},
		{"legacy-length Signal key", "sig_synthetic-short"},
		{"bad key prefix alphabet", strings.Replace(checkRelayKey, checkRelayTag, strings.Repeat("u", 26), 1)},
		{"padded secret", checkRelayKey + "="},
	}
	for _, bad := range badKeys {
		for _, slot := range []string{"approval", "collector"} {
			t.Run(bad.name+"/"+slot, func(t *testing.T) {
				root := t.TempDir()
				dir, config := filepath.Join(root, "state"), filepath.Join(root, "config.yaml")
				fixtureLocalSignalPins(t, dir, config)
				before := fixtureTree(t, root)
				app, relay := checkAppKey, checkRelayKey
				if slot == "approval" {
					app = bad.key
				} else {
					relay = bad.key
				}
				setup := verifyInput(t, checkOrigin, app)
				f := signalVerificationFixture(t)
				code, stdout, stderr := runSignalCheck(t.Context(), t, f, setup+"\n"+relay+"\n", verificationArgs(dir, config, "@fixture"))
				want := "well-formed Signal API keys; no credentials were sent"
				if slot == "approval" && bad.key == foreign {
					want = "expected the new setup code" // the legacy envelope reader already refuses this
				}
				if code == 0 || stdout != "" || !strings.Contains(stderr, want) || strings.Contains(stderr, bad.key) ||
					f.opened || len(f.calls) != 0 || !reflect.DeepEqual(before, fixtureTree(t, root)) {
					t.Fatal("bad credential was reflected, reached the HTTP client, or changed state")
				}
			})
		}
	}
}

func TestSignalOnboardingChecksOwnActivityByConfiguredSourceName(t *testing.T) {
	const oldSlack = "slack:user:UOLD"
	const newSlack = "slack:user:UNEW"
	const configYAML = `signal:
  url: ` + checkOrigin + `
person:
  handle: '@fixture'
  identities:
    team_messages: ` + newSlack + `
    slack: ` + oldSlack + `
    mail: email:PERSON@Example.Test
    dormant: github:user:unused
mode: dry-run
state: collector
rules: rules.yaml
sources:
  - kind: slack
    name: team_messages
    workspace: TTEAM
    user: UNEW
  - kind: gmail
    name: mail
    account: person@example.test
  - kind: jira
    site: fixture
    projects: [ENG]
`
	root := t.TempDir()
	dir, config := filepath.Join(root, "state"), filepath.Join(root, "config.yaml")
	fixtureLocalSignalPins(t, dir, config)
	must(t, os.WriteFile(config, []byte(configYAML), 0o600))
	before := fixtureTree(t, root)
	for _, tt := range []struct {
		name  string
		grant []string
		ok    bool
	}{
		{"attached but obsolete Slack identity", []string{oldSlack, checkIdentity}, false},
		{"second enabled binding missing", []string{newSlack}, false},
		{"exact grants for renamed sources", []string{newSlack, checkIdentity}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := signalVerificationFixture(t)
			f.person["identities"] = []string{oldSlack, newSlack, checkIdentity, "github:user:unused"}
			f.grant["identity_prefixes"] = tt.grant
			input := verifyInput(t, checkOrigin, checkAppKey) + "\n" + checkRelayKey + "\n"
			code, stdout, stderr := runSignalCheck(t.Context(), t, f, input, verificationArgs(dir, config, "@fixture"))
			if tt.ok {
				if code != 0 || !strings.Contains(stdout, "Nothing was saved or activated") {
					t.Fatal("verification refused exact grants for enabled bindings")
				}
			} else if code == 0 || stdout != "" || !strings.Contains(stderr, "does not cover all configured own-activity identities") {
				t.Fatal("verification accepted or misdiagnosed a grant missing an enabled binding")
			}
			for _, identity := range []string{oldSlack, newSlack, "github:user:unused", "email:PERSON@Example.Test"} {
				if strings.Contains(stdout+stderr, identity) {
					t.Fatal("verification printed a source identity")
				}
			}
			if len(f.calls) != 6 || f.calls[5] != "collector "+checkGrantPath || !reflect.DeepEqual(before, fixtureTree(t, root)) {
				t.Fatal("verification did not check the grant or modified existing state")
			}
		})
	}
}

func TestSignalOnboardingRejectsWrongRolesPeopleAndGrants(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*verificationFixture)
	}{
		{"swapped roles", nil},
		{"collector belongs to another person", func(f *verificationFixture) {
			f.relayMe["actor"].(approval.Object)["works_for"] = checkOther
		}},
		{"two matching but unintended people", func(f *verificationFixture) {
			f.appMe["actor"].(approval.Object)["works_for"], f.relayMe["actor"].(approval.Object)["works_for"] = checkOther, checkOther
			f.app["works_for"], f.relay["works_for"], f.person["id"], f.person["handle"] = checkOther, checkOther, checkOther, "unintended"
		}},
		{"disabled app", func(f *verificationFixture) { f.app["approval_app"] = false }},
		{"inactive human", func(f *verificationFixture) { f.person["deactivated_at"] = "2026-11-01T00:00:00Z" }},
		{"inactive collector", func(f *verificationFixture) { f.relay["deactivated_at"] = "2026-11-01T00:00:00Z" }},
		{"same integration with conflicting role claims", func(f *verificationFixture) {
			f.relayMe["actor"].(approval.Object)["id"], f.relay["id"] = checkApp, checkApp
		}},
		{"unrelated actor capability", func(f *verificationFixture) { f.relay["executes"] = []string{"github.merge"} }},
		{"absent actor capability evidence", func(f *verificationFixture) { delete(f.relay, "directory") }},
		{"admin scope", func(f *verificationFixture) { f.appMe["scopes"] = []string{"read", "write", "admin"} }},
		{"curator", func(f *verificationFixture) { f.relayMe["curator"] = true }},
		{"collector without read", func(f *verificationFixture) { f.relayMe["scopes"] = []string{"relay", "write"} }},
		{"wrong key grant", func(f *verificationFixture) { f.grant["id"] = "key_01m6a6ft00e40r2081040g2087" }},
		{"revoked key grant", func(f *verificationFixture) { f.grant["revoked_at"] = "2026-11-01T00:00:00Z" }},
		{"empty relay grant", func(f *verificationFixture) { f.grant["identity_prefixes"] = []string{} }},
		{"wildcard grant", func(f *verificationFixture) { f.grant["identity_prefixes"] = []string{"email:*"} }},
		{"unrelated identity grant", func(f *verificationFixture) {
			f.grant["identity_prefixes"] = []string{"zitadel:example.test:user:fixture"}
		}},
		{"another person's identity grant", func(f *verificationFixture) { f.grant["identity_prefixes"] = []string{"email:another@example.test"} }},
		{"grant cannot be read", func(f *verificationFixture) { f.refuse, f.status = "collector "+checkGrantPath, http.StatusForbidden }},
		{"refused credential", func(f *verificationFixture) { f.refuse, f.status = "app /api/me", http.StatusUnauthorized }},
		{"redirect", func(f *verificationFixture) { f.refuse, f.status = "app /api/me", http.StatusFound }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir, config := filepath.Join(root, "state"), filepath.Join(root, "config.yaml")
			f := signalVerificationFixture(t)
			app, relay := checkAppKey, checkRelayKey
			if tt.change == nil {
				app, relay = relay, app
			} else {
				tt.change(f)
			}
			input := verifyInput(t, checkOrigin, app) + "\n" + relay + "\n"
			code, stdout, _ := runSignalCheck(t.Context(), t, f, input, verificationArgs(dir, config, "@fixture"))
			if code == 0 || stdout != "" || len(f.calls) == 0 || len(fixtureTree(t, root)) != 1 {
				t.Fatal("invalid Signal authority appeared to be verified or changed local state")
			}
		})
	}
}

func TestSignalOnboardingInterruptedWithoutChanges(t *testing.T) {
	root := t.TempDir()
	dir, config := filepath.Join(root, "state"), filepath.Join(root, "config.yaml")
	fixtureLocalSignalPins(t, dir, config)
	before := fixtureTree(t, root)
	f := signalVerificationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.before = func(call string) {
		if call == "collector "+checkGrantPath {
			cancel()
		}
	}
	input := verifyInput(t, checkOrigin, checkAppKey) + "\n" + checkRelayKey + "\n"
	code, stdout, stderr := runSignalCheck(ctx, t, f, input, verificationArgs(dir, config, checkPerson))
	if code == 0 || stdout != "" || !errors.Is(ctx.Err(), context.Canceled) || !strings.Contains(stderr, "context canceled") ||
		!reflect.DeepEqual(before, fixtureTree(t, root)) {
		t.Fatal("interrupted verification did not fail without changing state")
	}
}

func TestSignalOnboardingRefusesANewActorForAnExistingApprovalBinding(t *testing.T) {
	root := t.TempDir()
	dir, config := filepath.Join(root, "state"), filepath.Join(root, "config.yaml")
	fixtureLocalSignalPins(t, dir, config)
	before := fixtureTree(t, root)
	f := signalVerificationFixture(t)
	f.appMe["actor"].(approval.Object)["id"] = checkOther
	input := verifyInput(t, checkOrigin, checkAppKey) + "\n" + checkRelayKey + "\n"
	code, stdout, stderr := runSignalCheck(t.Context(), t, f, input, verificationArgs(dir, config, "@fixture"))
	if code == 0 || stdout != "" || !strings.Contains(stderr, "existing actor pin") ||
		!reflect.DeepEqual(f.calls, []string{"app /api/me"}) || !reflect.DeepEqual(before, fixtureTree(t, root)) {
		t.Fatal("verification did not preserve the approval actor pin")
	}
}

func TestSignalVerificationEgressUsesTheTrustedOrigin(t *testing.T) {
	a, stdout, stderr := previewCLI() // transport panics if listing egress makes a request
	if code := a.main(t.Context(), []string{"egress", "--signal-origin", checkOrigin}); code != 0 || stderr.Len() != 0 ||
		!strings.Contains(stdout.String(), "signal.fixture.test:443") || !strings.Contains(stdout.String(), "/api/api-keys") {
		t.Fatal("verification egress was not listed")
	}
}

func TestSignalOnboardingRequiresHiddenTerminalInput(t *testing.T) {
	root := t.TempDir()
	a, stdout, stderr := previewCLI() // transport panics if the verifier reaches Signal
	a.euid = func() int { return 501 }
	r, w, err := os.Pipe()
	must(t, err)
	defer r.Close()
	must(t, w.Close())
	a.stdin = r
	if code := a.main(t.Context(), verificationArgs(filepath.Join(root, "state"), filepath.Join(root, "config.yaml"), "@fixture")); code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "hidden input is required") {
		t.Fatal("the verifier did not refuse nonterminal credential input")
	}
}
