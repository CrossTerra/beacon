package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
	"github.com/CrossTerra/beacon/internal/titled"
)

// sealingSource names a sealed thread without content, as a Google source does.
type sealingSource struct{ *fakeSource }

func (s sealingSource) SealedTitle(source.Item) string { return "Mail from a colleague" }

const plainTitleRules = `rules:
  - name: mail
    match: {fields: {label: send}}
    tier: send
    fields: [title, label]
`

const sealedTitleRules = `rules:
  - name: colleagues
    match: {fields: {label: send}}
    tier: send
    seal: owner+agents
    fields: [title]
`

const plainStillRules = `rules:
  - name: colleagues
    match: {fields: {label: send}}
    tier: send
    seal: owner+agents
    plain: [title]
    fields: [title]
`

// A thread Beacon opened with a plain object title that its current rules would seal is renamed to its source's
// neutral title, once, through the outbox. Not one someone renamed, not one whose current rule leaves the title plain.
func TestRetitleSealedThreads(t *testing.T) {
	f := newFixture(t)
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	mail := func(n string) source.Item {
		return source.Item{Source: "fx", Subject: "fx:mail:" + n, Key: "fx:mail:" + n + ":1", Type: "received", OccurredAt: at,
			Fields: map[string]string{"title": "Offsite budget " + n, "label": "send"}}
	}
	f.src.items = []source.Item{mail("a"), mail("b"), mail("c")}
	pass := func(y string) *Report {
		t.Helper()
		r, done := f.runner(true, nil)
		defer done()
		r.Sources = []source.Source{sealingSource{f.src}}
		r.Rules = mustRules(t, y)
		rec, err := titled.Read(f.dir)
		if err != nil {
			t.Fatal(err)
		}
		r.Threads, r.Retitled = f.client, rec
		rep, err := r.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	pass(plainTitleRules)
	if th, _ := f.server.ThreadFor("fx:mail:a"); th.Title != "Offsite budget a" {
		t.Fatalf("opened %+v", th)
	}
	// While the rules leave the title plain, nothing is renamed.
	f.src.items = nil
	pass(plainStillRules)
	if th, _ := f.server.ThreadFor("fx:mail:a"); th.Title != "Offsite budget a" {
		t.Fatalf("renamed under a plain rule: %+v", th)
	}
	f.server.Rename("fx:mail:b", "Budget, renamed by Winton")
	f.clock.t = f.clock.t.Add(time.Minute)
	rep := pass(sealedTitleRules)
	for subject, want := range map[string]string{
		"fx:mail:a": "Mail from a colleague",
		"fx:mail:b": "Budget, renamed by Winton",
		"fx:mail:c": "Mail from a colleague",
	} {
		if th, _ := f.server.ThreadFor(subject); th.Title != want {
			t.Errorf("%s: %+v, want %q", subject, th, want)
		}
	}
	if rep.Retitled != 2 {
		t.Fatalf("retitled %d\n%s", rep.Retitled, f.log.String())
	}
	n := 0
	for _, e := range f.server.Events() {
		if strings.Contains(string(e.Body), `"type":"thread.titled"`) {
			n++
			if strings.Contains(string(e.Body), "Offsite") || !strings.HasPrefix(e.Key, "fx:retitle:") {
				t.Fatalf("thread change %s", e.Body)
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d thread changes", n)
	}
	// Each thread once.
	f.clock.t = f.clock.t.Add(time.Minute)
	reads := len(f.server.Requests())
	pass(sealedTitleRules)
	for _, r := range f.server.Requests()[reads:] {
		if strings.Contains(string(r.Body), "thread.titled") || strings.HasPrefix(r.Path, "/api/threads/") {
			t.Fatalf("retitled again: %s %s", r.Path, r.Body)
		}
	}
}

func TestSealsTitle(t *testing.T) {
	for _, tc := range []struct {
		fields, plain []string
		want          bool
	}{
		{[]string{"subject", "body"}, nil, true},
		{[]string{"subject", "body"}, []string{"subject"}, false},
		{[]string{"summary", "subject"}, []string{"subject"}, true},
		{[]string{"title", "summary"}, []string{"title"}, false},
		{[]string{"link"}, nil, true},
	} {
		if got := sealsTitle(tc.fields, tc.plain); got != tc.want {
			t.Errorf("%v %v: %v", tc.fields, tc.plain, got)
		}
	}
}
