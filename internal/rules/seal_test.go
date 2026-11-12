package rules

import (
	"strings"
	"testing"
)

func TestSealParses(t *testing.T) {
	f := parse(t, `rules:
  - name: mail
    match: {source: gmail}
    tier: hold
    seal: owner+agents
    plain: [subject]
    fields: [subject, body, actor]
  - name: notes
    match: {source: drive}
    tier: send
    seal: owner
    fields: [title, body]
`)
	if f.Rules[0].Seal != "owner+agents" || len(f.Rules[0].Plain) != 1 || f.Rules[1].Seal != "owner" {
		t.Fatalf("%+v %+v", f.Rules[0], f.Rules[1])
	}
	if !f.ListsSealed("drive", "body") || !f.ListsSealed("gmail", "body") || f.ListsSealed("slack", "body") {
		t.Fatal("ListsSealed")
	}
}

func TestSealRefused(t *testing.T) {
	for name, y := range map[string]string{
		"seal on never":     "rules:\n  - name: a\n    match: {}\n    tier: never\n    seal: owner\n",
		"plain on never":    "rules:\n  - name: a\n    match: {}\n    tier: never\n    plain: [subject]\n",
		"unknown level":     "rules:\n  - name: a\n    match: {}\n    tier: send\n    seal: agents\n    fields: [subject]\n",
		"plain without":     "rules:\n  - name: a\n    match: {}\n    tier: send\n    plain: [subject]\n    fields: [subject]\n",
		"body plain":        "rules:\n  - name: a\n    match: {}\n    tier: send\n    seal: owner\n    plain: [body]\n    fields: [body]\n",
		"actor plain":       "rules:\n  - name: a\n    match: {}\n    tier: send\n    seal: owner\n    plain: [actor]\n    fields: [actor]\n",
		"references plain":  "rules:\n  - name: a\n    match: {}\n    tier: send\n    seal: owner\n    plain: [references]\n    fields: [references]\n",
		"plain not listed":  "rules:\n  - name: a\n    match: {}\n    tier: send\n    seal: owner\n    plain: [subject]\n    fields: [from]\n",
		"plain twice":       "rules:\n  - name: a\n    match: {}\n    tier: send\n    seal: owner\n    plain: [subject, subject]\n    fields: [subject]\n",
		"seal not a string": "rules:\n  - name: a\n    match: {}\n    tier: send\n    seal: [owner]\n",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

var sealSources = map[string]Source{
	"gmail":  {Fields: []string{"subject", "from", "body", "link"}, Personal: PersonalAll},
	"slack":  {Fields: []string{"channel_type", "body", "permalink"}, Personal: PersonalSome, PersonalField: "channel_type", PersonalValues: []string{"im", "mpim", "private"}},
	"github": {Fields: []string{"title", "body"}},
}

func problems(t *testing.T, y string) []Problem {
	t.Helper()
	return parse(t, y).Check(sealSources)
}

func TestCheckPersonalBody(t *testing.T) {
	// DEC-32: a personal body without seal is an error.
	ps := problems(t, "rules:\n  - name: a\n    match: {source: gmail}\n    tier: hold\n    fields: [subject, body]\n")
	if len(ps) == 0 || !ps[0].Error() || !strings.Contains(ps[0].Message, "DEC-32") {
		t.Fatalf("%v", ps)
	}
	// Slack: a rule that can match a direct message needs seal; a public channel body may stay plain.
	ps = problems(t, "rules:\n  - name: a\n    match: {source: slack}\n    tier: hold\n    fields: [body]\n")
	if len(ps) == 0 || !ps[0].Error() {
		t.Fatalf("slack any: %v", ps)
	}
	for _, p := range problems(t, "rules:\n  - name: a\n    match: {source: slack, fields: {channel_type: channel}}\n    tier: send\n    fields: [body, permalink]\n") {
		t.Errorf("public channel: %v", p)
	}
	for _, p := range problems(t, "rules:\n  - name: a\n    match: {source: github}\n    tier: send\n    fields: [body]\n") {
		t.Errorf("github: %v", p)
	}
	for _, p := range problems(t, "rules:\n  - name: a\n    match: {source: gmail}\n    tier: send\n    seal: owner\n    fields: [subject, body]\n") {
		t.Errorf("sealed: %v", p)
	}
}

func TestCheckSealShared(t *testing.T) {
	for _, y := range []string{
		"rules:\n  - name: a\n    match: {source: github}\n    tier: send\n    seal: owner\n    fields: [title]\n",
		"rules:\n  - name: a\n    match: {source: slack, fields: {channel_type: channel}}\n    tier: send\n    seal: owner\n    fields: [body]\n",
	} {
		ps := problems(t, y)
		if len(ps) != 1 || !ps[0].Error() || !strings.Contains(ps[0].Message, "seals only") {
			t.Errorf("%s: %v", y, ps)
		}
	}
	for _, p := range problems(t, "rules:\n  - name: a\n    match: {source: slack, fields: {channel_type: {regex: 'im|mpim|private'}}}\n    tier: send\n    seal: owner+agents\n    fields: [body]\n") {
		t.Errorf("slack private: %v", p)
	}
}

func TestCheckPlainPersonalSendWarns(t *testing.T) {
	// DEC-36: plaintext personal content in a send rule is a warning; hold, or sealed with nothing plain, is not.
	for _, y := range []string{
		"rules:\n  - name: a\n    match: {source: gmail}\n    tier: send\n    fields: [subject]\n",
		"rules:\n  - name: a\n    match: {source: gmail}\n    tier: send\n    seal: owner\n    plain: [subject]\n    fields: [subject, body]\n",
	} {
		ps := problems(t, y)
		if len(ps) != 1 || !ps[0].Warning || !strings.Contains(ps[0].Message, "start as hold") {
			t.Errorf("%s: %v", y, ps)
		}
	}
	for _, y := range []string{
		"rules:\n  - name: a\n    match: {source: gmail}\n    tier: hold\n    seal: owner\n    plain: [subject]\n    fields: [subject, body]\n",
		"rules:\n  - name: a\n    match: {source: gmail}\n    tier: send\n    seal: owner\n    fields: [subject, body]\n",
		"rules:\n  - name: a\n    match: {source: slack, fields: {channel_type: channel}}\n    tier: send\n    fields: [body]\n",
	} {
		for _, p := range problems(t, y) {
			t.Errorf("%s: %v", y, p)
		}
	}
}
