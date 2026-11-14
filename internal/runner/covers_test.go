package runner

import (
	"context"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/source"
)

// coverItems is a change and the snapshot beside it. The snapshot sorts before its change, so the runner must not
// rely on collect order to know whether the change is sent.
func coverItems(label string) []source.Item {
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	change := source.Item{Source: "fx", Subject: "fx:obj:9", Key: "fx:obj:9:z-change", Type: "merged", OccurredAt: at,
		Fields: map[string]string{"title": "T", "label": label}}
	snap := source.Item{Source: "fx", Subject: "fx:obj:9", Key: "fx:obj:9:a-snapshot", Type: "snapshot", OccurredAt: at,
		Fields: map[string]string{"title": "T"}, Covers: change.Key}
	return []source.Item{snap, change}
}

const coverRules = `rules:
  - name: changes
    match: {type: merged, fields: {label: send}}
    tier: send
    fields: [title]
  - name: held-changes
    match: {type: merged, fields: {label: hold}}
    tier: hold
    fields: [title]
  - name: snapshots
    match: {type: snapshot}
    tier: send
    fields: [title]
`

func (f *fixture) coverRunner(send bool, label string) (*Runner, func()) {
	f.t.Helper()
	f.src.items = coverItems(label)
	r, done := f.runner(send, nil)
	rs, err := rules.Parse([]byte(coverRules))
	if err != nil {
		f.t.Fatal(err)
	}
	r.Rules = rs
	return r, done
}

func plannedByKey(rep *Report) map[string]Planned {
	out := map[string]Planned{}
	for _, p := range rep.Items {
		out[p.Item.Key] = p
	}
	return out
}

// A snapshot is not sent beside a change that is sent, in this pass or a later one.
func TestSnapshotSkippedWhenItsChangeIsSent(t *testing.T) {
	f := newFixture(t)
	for pass := 0; pass < 2; pass++ {
		r, done := f.coverRunner(true, "send")
		rep, err := r.Run(context.Background())
		done()
		if err != nil {
			t.Fatal(err)
		}
		got := plannedByKey(rep)
		if p := got["fx:obj:9:a-snapshot"]; p.Skipped == "" || p.Body != nil {
			t.Fatalf("pass %d: snapshot %+v", pass, p)
		}
	}
	if n := len(f.server.RequestsFor("fx:obj:9:a-snapshot")); n != 0 {
		t.Fatalf("snapshot sent %d times", n)
	}
	if n := len(f.server.RequestsFor("fx:obj:9:z-change")); n != 1 {
		t.Fatalf("change sent %d times", n)
	}
}

// When no rule sends the change, or it is held, the snapshot is sent, once.
func TestSnapshotSentWhenItsChangeIsNot(t *testing.T) {
	for _, label := range []string{"noise", "hold"} {
		f := newFixture(t)
		for pass := 0; pass < 2; pass++ {
			r, done := f.coverRunner(true, label)
			if _, err := r.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			done()
		}
		if n := len(f.server.RequestsFor("fx:obj:9:a-snapshot")); n != 1 {
			t.Fatalf("%s: snapshot sent %d times", label, n)
		}
		if n := len(f.server.RequestsFor("fx:obj:9:z-change")); n != 0 {
			t.Fatalf("%s: change sent %d times", label, n)
		}
	}
}

// A dry run records nothing, and still says the snapshot would not be sent beside a change it would send.
func TestSnapshotDryRun(t *testing.T) {
	for label, sent := range map[string]bool{"send": false, "noise": true} {
		f := newFixture(t)
		r, done := f.coverRunner(false, label)
		rep, err := r.Run(context.Background())
		done()
		if err != nil {
			t.Fatal(err)
		}
		p := plannedByKey(rep)["fx:obj:9:a-snapshot"]
		if got := p.Skipped == "" && p.Tier == rules.Send; got != sent {
			t.Fatalf("%s: snapshot %+v", label, p)
		}
	}
}

func TestValidateCovers(t *testing.T) {
	it := coverItems("send")[0]
	if err := source.Validate(it, []string{"title", "label"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"other:obj:9:1", it.Key, "fx:has space"} {
		it.Covers = bad
		if source.Validate(it, []string{"title", "label"}, nil) == nil {
			t.Errorf("covers %q accepted", bad)
		}
	}
}
