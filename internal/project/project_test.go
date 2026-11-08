package project

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/source"
)

var person = Person{Handle: "@winton", Identities: map[string]string{"github": "github:user:winton"}}

func baseItem() source.Item {
	due := time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC)
	return source.Item{
		Source: "github", Subject: "github:pr:acme/app#7", Key: "github:pr:acme/app#7:42", Type: "review_requested",
		OccurredAt: time.Date(2026, 11, 7, 12, 0, 0, 123456789, time.FixedZone("x", 3600)),
		Actor:      "github:user:dana", Addressed: true, DueAt: &due,
		Fields: map[string]string{
			"title": "SECRET-TITLE", "body": "SECRET-BODY", "repository": "SECRET-REPO",
			"url": "SECRET-URL", "label": "SECRET-LABEL",
		},
		References: []string{"SECRET-REF"},
	}
}

func rule(fields ...string) *rules.Rule {
	return &rules.Rule{Name: "r", Tier: rules.Send, Fields: fields}
}

// Every subset of the fields: a field not listed never appears in the bytes.
func TestUnlistedFieldsNeverAppear(t *testing.T) {
	all := []string{"title", "body", "repository", "url", "label", rules.FieldActor, rules.FieldReferences}
	secret := map[string]string{
		"title": "SECRET-TITLE", "body": "SECRET-BODY", "repository": "SECRET-REPO", "url": "SECRET-URL",
		"label": "SECRET-LABEL", rules.FieldActor: "github:user:dana", rules.FieldReferences: "SECRET-REF",
	}
	for mask := 0; mask < 1<<len(all); mask++ {
		var listed []string
		in := map[string]bool{}
		for i, f := range all {
			if mask&(1<<i) != 0 {
				listed = append(listed, f)
				in[f] = true
			}
		}
		_, data, err := Project(Input{Item: baseItem(), Rule: rule(listed...), Person: person})
		if err != nil {
			t.Fatal(err)
		}
		for f, v := range secret {
			if got := bytes.Contains(data, []byte(v)); got != in[f] {
				t.Fatalf("fields %v: %s present=%v in %s", listed, f, got, data)
			}
		}
	}
}

func TestAlwaysPresent(t *testing.T) {
	_, data, err := Project(Input{Item: baseItem(), Rule: rule(), Person: person})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"subject":"github:pr:acme/app#7","type":"github.review_requested","key":"github:pr:acme/app#7:42","occurred_at":"2026-11-07T11:00:00.123Z","open":{}}`
	if string(data) != want {
		t.Fatalf("got  %s\nwant %s", data, want)
	}
}

func TestPlacement(t *testing.T) {
	b, _, _ := Project(Input{Item: baseItem(), Rule: rule("title", "body", "url", "actor", "references"), Person: person})
	if b.Title != "SECRET-TITLE" || b.Open.Title != "SECRET-TITLE" || b.Body != "SECRET-BODY" {
		t.Fatalf("title/body: %+v", b)
	}
	if b.Metadata["actor"] != "github:user:dana" {
		t.Fatalf("metadata.actor: %v", b.Metadata)
	}
	if f := b.Metadata["fields"].(map[string]string); len(f) != 1 || f["url"] != "SECRET-URL" {
		t.Fatalf("metadata.fields: %v", f)
	}
}

// DEC-21, DEC-27: an ask names the person's handle, never "me", with priority and the source's due time.
func TestAskMe(t *testing.T) {
	r := rule("title")
	r.Ask, r.Priority = "me", "high"
	b, data, err := Project(Input{Item: baseItem(), Rule: r, Person: person})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.To) != 1 || b.To[0] != "@winton" || b.Priority != "high" || b.DueAt != "2026-11-09T17:00:00.000Z" {
		t.Fatalf("ask: %s", data)
	}
	if strings.Contains(string(data), `"me"`) {
		t.Fatalf("body names me: %s", data)
	}
	if b.By != nil {
		t.Fatalf("another person's act has by: %s", data)
	}
	// Without ask, nobody is asked.
	b, data, _ = Project(Input{Item: baseItem(), Rule: rule("title"), Person: person})
	if b.To != nil || b.Priority != "" || b.DueAt != "" {
		t.Fatalf("asked without ask: %s", data)
	}
}

// DEC-11 as narrowed by DEC-27: by only on the person's own acts, from the configured identity.
func TestBy(t *testing.T) {
	own := baseItem()
	own.Own, own.Actor, own.Type = true, "github:user:winton", "review_submitted"
	r := rule("title")
	r.Ask = "me"
	b, data, _ := Project(Input{Item: own, Rule: r, Person: person})
	if b.By == nil || b.By.Identity != "github:user:winton" {
		t.Fatalf("own act: %s", data)
	}
	if b.To != nil {
		t.Fatalf("own act asks: %s", data)
	}
	// No identity configured for the source: no by.
	own.Source, own.Key = "jira", "jira:X-1:1"
	b, data, _ = Project(Input{Item: own, Rule: rule(), Person: person})
	if b.By != nil {
		t.Fatalf("by without a configured identity: %s", data)
	}
	// Another person's act never carries by, even with actor listed.
	b, data, _ = Project(Input{Item: baseItem(), Rule: rule("actor"), Person: person})
	if b.By != nil || !strings.Contains(string(data), `"metadata":{"actor":"github:user:dana"}`) {
		t.Fatalf("other's act: %s", data)
	}
}

// DEC-20: own acts close open asks; others' acts only with close_by_others.
func TestCloses(t *testing.T) {
	open := []string{"github:pr:acme/app#7:1", "github:pr:acme/app#7:2"}
	own := baseItem()
	own.Own = true
	b, data, _ := Project(Input{Item: own, Rule: rule(), Person: person, OpenAsks: open})
	if len(b.Closes) != 2 || b.Closes[0].Key != open[0] || !strings.Contains(string(data), `"closes":[{"key":"github:pr:acme/app#7:1"},{"key":"github:pr:acme/app#7:2"}]`) {
		t.Fatalf("own act closes: %s", data)
	}
	b, _, _ = Project(Input{Item: baseItem(), Rule: rule(), Person: person, OpenAsks: open})
	if b.Closes != nil {
		t.Fatalf("other's act closed without close_by_others")
	}
	r := rule()
	r.CloseByOthers = true
	b, data, _ = Project(Input{Item: baseItem(), Rule: r, Person: person, OpenAsks: open})
	if len(b.Closes) != 2 || b.By != nil {
		t.Fatalf("close_by_others: %s", data)
	}
	many := make([]string, 25)
	for i := range many {
		many[i] = "k" + string(rune('a'+i))
	}
	b, _, _ = Project(Input{Item: own, Rule: rule(), Person: person, OpenAsks: many})
	if len(b.Closes) != 20 {
		t.Fatalf("closes %d, want Signal's limit 20", len(b.Closes))
	}
}

func TestDeterministic(t *testing.T) {
	r := rule("title", "url", "repository", "label", "actor")
	_, a, _ := Project(Input{Item: baseItem(), Rule: r, Person: person})
	for i := 0; i < 50; i++ {
		_, b, _ := Project(Input{Item: baseItem(), Rule: r, Person: person})
		if !bytes.Equal(a, b) {
			t.Fatalf("bytes differ:\n%s\n%s", a, b)
		}
	}
}

func TestRefusals(t *testing.T) {
	if _, _, err := Project(Input{Item: baseItem(), Rule: nil, Person: person}); err == nil {
		t.Error("projected with no rule")
	}
	if _, _, err := Project(Input{Item: baseItem(), Rule: &rules.Rule{Tier: rules.Never}, Person: person}); err == nil {
		t.Error("projected a never rule")
	}
	big := baseItem()
	big.Fields["body"] = strings.Repeat("x", 70*1024)
	if _, _, err := Project(Input{Item: big, Rule: rule("body"), Person: person}); err == nil {
		t.Error("projected a body over Signal's limit")
	}
}

func TestTitleLimit(t *testing.T) {
	it := baseItem()
	it.Fields["title"] = "  " + strings.Repeat("é", 298) + "😀😀" + "  "
	b, _, _ := Project(Input{Item: it, Rule: rule("title"), Person: person})
	if n := len([]rune(b.Title)); n != 299 {
		t.Fatalf("title has %d runes, want 299: 298 units and one surrogate pair make Signal's 300", n)
	}
}

func stateItem() source.Item {
	it := baseItem()
	at := time.Date(2026, 11, 7, 12, 30, 0, 0, time.UTC)
	it.Key, it.Version, it.ObservedAt = "github:pr:acme/app#7:v42", "v42", &at
	it.State = map[string]string{"state": "open", "head": "SECRET-HEAD", "mergeable": "SECRET-MERGEABLE"}
	return it
}

// DEC-28: the snapshot holds only the state fields the rule lists, with observed_at and the version marker.
func TestState(t *testing.T) {
	r := rule("title")
	r.State = []string{"state", "checks"}
	b, data, err := Project(Input{Item: stateItem(), Rule: r, Person: person})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"state": "open", "observed_at": "2026-11-07T12:30:00.000Z", "version": "v42"}
	if len(b.State) != len(want) {
		t.Fatalf("state %v", b.State)
	}
	for k, v := range want {
		if b.State[k] != v {
			t.Fatalf("state %v", b.State)
		}
	}
	if bytes.Contains(data, []byte("SECRET-HEAD")) || bytes.Contains(data, []byte("SECRET-MERGEABLE")) {
		t.Fatalf("unlisted state field sent: %s", data)
	}
	if !bytes.Contains(data, []byte(`"state":{"observed_at":"2026-11-07T12:30:00.000Z","state":"open","version":"v42"}`)) {
		t.Fatalf("%s", data)
	}
}

func TestNoStateUnlessListedAndPresent(t *testing.T) {
	for name, in := range map[string]Input{
		"rule lists none":           {Item: stateItem(), Rule: rule("title")},
		"item has no snapshot":      {Item: baseItem(), Rule: &rules.Rule{Tier: rules.Send, State: []string{"state"}}},
		"listed fields all missing": {Item: stateItem(), Rule: &rules.Rule{Tier: rules.Send, State: []string{"checks"}}},
	} {
		in.Person = person
		_, data, err := Project(in)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(`"state":{`)) || bytes.Contains(data, []byte("observed_at")) {
			t.Errorf("%s: %s", name, data)
		}
	}
}
