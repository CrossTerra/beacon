package rules

import (
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

func item(mod func(*source.Item)) source.Item {
	it := source.Item{
		Source: "github", Subject: "github:pr:acme/app#1", Key: "github:pr:acme/app#1:1", Type: "review_requested",
		OccurredAt: time.Unix(1, 0), Actor: "github:user:dana", Addressed: true,
		Fields: map[string]string{"repository": "acme/app", "title": "Fix the build", "label": "bug"},
	}
	if mod != nil {
		mod(&it)
	}
	return it
}

func parse(t *testing.T, y string) *File {
	t.Helper()
	f, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

func decide(t *testing.T, y string, it source.Item) Decision {
	t.Helper()
	return parse(t, y).Decide(it)
}

func TestDefaultNever(t *testing.T) {
	for _, y := range []string{"", "rules: []", "rules:\n  - name: other\n    match: {source: jira}\n    tier: send\n"} {
		d := decide(t, y, item(nil))
		if d.Tier != Never || d.Rule != nil {
			t.Errorf("%q: got %v %v, want never by default", y, d.Tier, d.Rule)
		}
	}
}

func TestMatchTypes(t *testing.T) {
	cases := []struct {
		name  string
		match string
		item  func(*source.Item)
		want  bool
	}{
		{"exact string", `{fields: {repository: acme/app}}`, nil, true},
		{"exact string miss", `{fields: {repository: acme}}`, nil, false},
		{"exact mapping", `{fields: {repository: {exact: acme/app}}}`, nil, true},
		{"glob", `{fields: {repository: {glob: "acme/*"}}}`, nil, true},
		{"glob whole value", `{fields: {repository: {glob: "acme"}}}`, nil, false},
		{"glob question", `{fields: {label: {glob: "b?g"}}}`, nil, true},
		{"glob metachars literal", `{fields: {repository: {glob: "acme.app"}}}`, nil, false},
		{"regex", `{fields: {title: {regex: "Fix .*"}}}`, nil, true},
		{"regex anchored", `{fields: {title: {regex: "build"}}}`, nil, false},
		{"regex alternation anchored", `{fields: {title: {regex: "x|Fix the build"}}}`, nil, true},
		{"missing field never matches", `{fields: {channel: {glob: "*"}}}`, nil, false},
		{"source", `{source: github}`, nil, true},
		{"source glob", `{source: {glob: "git*"}}`, nil, true},
		{"source miss", `{source: jira}`, nil, false},
		{"type", `{type: review_requested}`, nil, true},
		{"type regex", `{type: {regex: "review_.*"}}`, nil, true},
		{"actor", `{actor: {glob: "github:user:*"}}`, nil, true},
		{"own true on own", `{own: true}`, func(i *source.Item) { i.Own = true }, true},
		{"own true on other", `{own: true}`, nil, false},
		{"own false", `{own: false}`, nil, true},
		{"addressed", `{addressed: true}`, nil, true},
		{"addressed false", `{addressed: false}`, nil, false},
		{"all conditions", `{source: github, type: review_requested, own: false, addressed: true, fields: {repository: {glob: "acme/*"}, label: bug}}`, nil, true},
		{"one condition fails", `{source: github, type: review_requested, fields: {label: feature}}`, nil, false},
		{"empty match is every item", `{}`, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			y := "rules:\n  - name: r\n    match: " + c.match + "\n    tier: send\n"
			d := decide(t, y, item(c.item))
			if got := d.Tier == Send; got != c.want {
				t.Errorf("match %s: got %v, want match=%v", c.match, d.Tier, c.want)
			}
		})
	}
}

func TestFirstMatchWins(t *testing.T) {
	y := `
rules:
  - name: first
    match: {fields: {repository: {glob: "acme/*"}}}
    tier: hold
  - name: second
    match: {fields: {repository: acme/app}}
    tier: send
  - name: third
    match: {}
    tier: never
`
	d := decide(t, y, item(nil))
	if d.Tier != Hold || d.Rule.Name != "first" || d.Rule.Index != 1 {
		t.Fatalf("got %v %+v, want hold by first", d.Tier, d.Rule)
	}
	d = decide(t, y, item(func(i *source.Item) { i.Fields["repository"] = "other/x" }))
	if d.Tier != Never || d.Rule.Name != "third" {
		t.Fatalf("got %v %+v, want never by third", d.Tier, d.Rule)
	}
}

func TestRejected(t *testing.T) {
	cases := map[string]string{
		"unknown top-level key":   "rule: []",
		"unknown rule key":        "rules:\n  - name: r\n    match: {}\n    tier: send\n    feilds: [title]\n",
		"unknown match key":       "rules:\n  - name: r\n    match: {repo: x}\n    tier: send\n",
		"unknown matcher":         "rules:\n  - name: r\n    match: {source: {prefix: x}}\n    tier: send\n",
		"two matchers":            "rules:\n  - name: r\n    match: {source: {exact: a, glob: b}}\n    tier: send\n",
		"blank match value":       "rules:\n  - name: r\n    match: {source: }\n    tier: send\n",
		"blank field value":       "rules:\n  - name: r\n    match: {fields: {repository: }}\n    tier: send\n",
		"missing match":           "rules:\n  - name: r\n    tier: send\n",
		"null match":              "rules:\n  - name: r\n    match:\n    tier: send\n",
		"own not bool":            "rules:\n  - name: r\n    match: {own: yes}\n    tier: send\n",
		"number as value":         "rules:\n  - name: r\n    match: {fields: {n: 3}}\n    tier: send\n",
		"bad regex":               "rules:\n  - name: r\n    match: {type: {regex: \"(\"}}\n    tier: send\n",
		"missing tier":            "rules:\n  - name: r\n    match: {}\n",
		"unknown tier":            "rules:\n  - name: r\n    match: {}\n    tier: maybe\n",
		"missing name":            "rules:\n  - match: {}\n    tier: send\n",
		"duplicate name":          "rules:\n  - name: r\n    match: {}\n    tier: send\n  - name: r\n    match: {}\n    tier: send\n",
		"never with fields":       "rules:\n  - name: r\n    match: {}\n    tier: never\n    fields: [title]\n",
		"ask someone else":        "rules:\n  - name: r\n    match: {}\n    tier: send\n    ask: '@fern'\n",
		"priority without ask":    "rules:\n  - name: r\n    match: {}\n    tier: send\n    priority: high\n",
		"bad priority":            "rules:\n  - name: r\n    match: {}\n    tier: send\n    ask: me\n    priority: asap\n",
		"field listed twice":      "rules:\n  - name: r\n    match: {}\n    tier: send\n    fields: [title, title]\n",
		"duplicate key in match":  "rules:\n  - name: r\n    match: {source: a, source: b}\n    tier: send\n",
		"duplicate field matcher": "rules:\n  - name: r\n    match: {fields: {a: x, a: y}}\n    tier: send\n",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(y)); err == nil {
				t.Fatalf("parsed %q, want an error", y)
			}
		})
	}
}

func TestUnreachable(t *testing.T) {
	y := `
rules:
  - name: all-acme
    match: {fields: {repository: {glob: "acme/*"}}}
    tier: hold
  - name: acme-app
    match: {source: github, fields: {repository: acme/app}}
    tier: send
  - name: jira-only
    match: {source: jira}
    tier: send
  - name: no-such-field
    match: {source: github, fields: {channel: general}}
    tier: send
  - name: own
    match: {own: true}
    tier: send
  - name: own-reviews
    match: {own: true, type: review_submitted}
    tier: send
  - name: other-reviews
    match: {own: false, type: review_submitted}
    tier: send
  - name: sends-unknown-field
    match: {type: y}
    tier: send
    fields: [title, colour, actor, references]
  - name: catch-all
    match: {}
    tier: never
  - name: after-catch-all
    match: {type: x}
    tier: send
`
	f := parse(t, y)
	problems := f.Check(map[string][]string{"github": {"repository", "title", "label"}})
	got := map[string]string{}
	for _, p := range problems {
		got[p.Rule.Name] = p.String()
		if p.Rule.Name == "sends-unknown-field" {
			if p.Unreachable || !strings.Contains(p.Message, `"colour"`) {
				t.Errorf("sends-unknown-field: %s", p)
			}
		}
	}
	for _, want := range []string{"acme-app", "jira-only", "no-such-field", "own-reviews", "after-catch-all"} {
		if _, ok := got[want]; !ok {
			t.Errorf("rule %s not reported unreachable; got %v", want, got)
		}
	}
	for _, reachable := range []string{"all-acme", "own", "other-reviews", "catch-all"} {
		if s, ok := got[reachable]; ok {
			t.Errorf("rule %s reported: %s", reachable, s)
		}
	}
	if !strings.Contains(got["acme-app"], `rule 1 "all-acme"`) {
		t.Errorf("acme-app: %s", got["acme-app"])
	}
	if !strings.Contains(got["after-catch-all"], `"catch-all"`) {
		t.Errorf("after-catch-all: %s", got["after-catch-all"])
	}
}

func TestLineNumbers(t *testing.T) {
	f := parse(t, "rules:\n  - name: a\n    match: {}\n    tier: send\n  - name: b\n    match: {}\n    tier: send\n")
	if f.Rules[0].Line != 2 || f.Rules[1].Line != 5 {
		t.Fatalf("lines %d %d", f.Rules[0].Line, f.Rules[1].Line)
	}
}
