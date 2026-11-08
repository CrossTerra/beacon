package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/rules"
)

// The fixture every command test and the end-to-end test use. Each item has a tier it must land in. Content that
// must never reach the network carries a SECRET marker.

const testRules = `rules:
  - name: own-acts
    match: {own: true}
    tier: send
    fields: [title, url]
  - name: noise
    match: {fields: {label: noise}}
    tier: never
  - name: review-requests
    match:
      source: fixture
      type: review_requested
      addressed: true
      fields: {repository: {glob: "acme/*"}}
    tier: send
    fields: [title, url, repository, actor]
    state: [status]
    ask: me
    priority: high
  - name: assigned
    match: {type: {regex: "assigned|reassigned"}, addressed: true}
    tier: send
    fields: [title]
    ask: me
  - name: answers-close
    match: {type: commented, fields: {label: {exact: answered}}}
    tier: send
    fields: [title]
    close_by_others: true
  - name: other-repos-hold
    match: {type: review_requested, fields: {repository: {regex: "other/.+"}}}
    tier: hold
    fields: [title]
`

type fixtureItem struct {
	Subject    string            `json:"subject"`
	Key        string            `json:"key"`
	Type       string            `json:"type"`
	OccurredAt time.Time         `json:"occurred_at"`
	Actor      string            `json:"actor"`
	Own        bool              `json:"own"`
	Addressed  bool              `json:"addressed"`
	Fields     map[string]string `json:"fields"`
	DueAt      *time.Time        `json:"due_at,omitempty"`
	State      map[string]string `json:"state,omitempty"`
	ObservedAt *time.Time        `json:"observed_at,omitempty"`
	Version    string            `json:"version,omitempty"`
}

// expected is an item's key, its tier, and the rule that must decide it ("" for the default).
type expected struct {
	key  string
	tier rules.Tier
	rule string
}

// fixture returns the fixture file and the expected tiers. n makes subjects and keys unique per run, so the
// end-to-end test can run against one Signal many times.
func fixture(n string) ([]byte, []expected) {
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	due := time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC)
	observed := at.Add(90 * time.Second)
	pr1 := "fixture:pr:acme/app#1-" + n
	issue3 := "fixture:issue:3-" + n
	items := []fixtureItem{
		{Subject: pr1, Key: "fixture:pr1-" + n + ":1", Type: "review_requested", OccurredAt: at.Add(1 * time.Minute),
			Actor: "fixture:user:dana", Addressed: true,
			Fields:     map[string]string{"title": "Add retries", "body": "UNLISTED-SECRET-BODY", "repository": "acme/app", "url": "https://example.test/pr/1"},
			State:      map[string]string{"status": "open", "head": "UNLISTED-SECRET-HEAD"},
			ObservedAt: &observed, Version: "1"},
		{Subject: pr1, Key: "fixture:pr1-" + n + ":2", Type: "review_submitted", OccurredAt: at.Add(2 * time.Minute),
			Actor: "fixture:user:winton", Own: true,
			Fields: map[string]string{"title": "Add retries", "body": "UNLISTED-SECRET-REVIEW", "url": "https://example.test/pr/1#review"}},
		{Subject: issue3, Key: "fixture:issue3-" + n + ":1", Type: "assigned", OccurredAt: at.Add(3 * time.Minute),
			Actor: "fixture:user:fern", Addressed: true, DueAt: &due,
			Fields: map[string]string{"title": "Rotate the keys", "body": "UNLISTED-SECRET-ISSUE"}},
		{Subject: issue3, Key: "fixture:issue3-" + n + ":2", Type: "commented", OccurredAt: at.Add(4 * time.Minute),
			Actor:  "fixture:user:fern",
			Fields: map[string]string{"title": "Rotate the keys", "label": "answered", "body": "UNLISTED-SECRET-COMMENT"}},
		{Subject: "fixture:pr:other/lib#4-" + n, Key: "fixture:pr4-" + n + ":1", Type: "review_requested", OccurredAt: at.Add(5 * time.Minute),
			Actor: "fixture:user:dana", Addressed: true,
			Fields: map[string]string{"title": "HOLD-SECRET-TITLE", "repository": "other/lib"}},
		{Subject: "fixture:chat:5-" + n, Key: "fixture:chat5-" + n + ":1", Type: "message", OccurredAt: at.Add(6 * time.Minute),
			Actor: "fixture:user:dana", Addressed: true,
			Fields: map[string]string{"title": "NEVER-SECRET-TITLE", "body": "NEVER-SECRET-BODY", "label": "noise"}},
		{Subject: "fixture:misc:6-" + n, Key: "fixture:misc6-" + n + ":1", Type: "mentioned", OccurredAt: at.Add(7 * time.Minute),
			Actor:  "fixture:user:dana",
			Fields: map[string]string{"title": "DEFAULT-SECRET-TITLE", "body": "DEFAULT-SECRET-BODY"}},
	}
	want := []expected{
		{items[0].Key, rules.Send, "review-requests"},
		{items[1].Key, rules.Send, "own-acts"},
		{items[2].Key, rules.Send, "assigned"},
		{items[3].Key, rules.Send, "answers-close"},
		{items[4].Key, rules.Hold, "other-repos-hold"},
		{items[5].Key, rules.Never, "noise"},
		{items[6].Key, rules.Never, ""},
	}
	data, err := json.MarshalIndent(map[string]any{
		"source":       "fixture",
		"fields":       []string{"title", "body", "repository", "url", "label"},
		"state_fields": []string{"status", "head"},
		"items":        items,
	}, "", "  ")
	if err != nil {
		panic(err)
	}
	return data, want
}

// secrets are strings that must never reach the network.
var secrets = []string{
	"UNLISTED-SECRET", "HOLD-SECRET", "NEVER-SECRET", "DEFAULT-SECRET",
}

// writeSetup writes a config, rules and fixture into dir and returns the config path.
func writeSetup(t *testing.T, dir, signalURL, mode, n string) (string, []expected) {
	t.Helper()
	data, want := fixture(n)
	must(t, os.WriteFile(filepath.Join(dir, "fixture.json"), data, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte(testRules), 0o600))
	cfg := fmt.Sprintf(`signal:
  url: %s
  keychain_account: test
person:
  handle: "@winton"
  identities:
    fixture: fixture:user:winton
rules: rules.yaml
state: state
mode: %s
sources:
  - kind: fixture
    path: fixture.json
`, signalURL, mode)
	path := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(path, []byte(cfg), 0o600))
	return path, want
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
