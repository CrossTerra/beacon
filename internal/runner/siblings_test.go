package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/source"
)

const askRules = `rules:
  - name: own
    match: {own: true}
    tier: send
  - name: asks
    match: {own: false}
    tier: send
    ask: me
`

// dmItem is a message in conversation conv at ts, in the thread rooted at root (root == ts for a top-level message).
func dmItem(conv, root, ts string, own bool, at time.Time) source.Item {
	it := source.Item{Source: "fx", Subject: "fx:dm:winton:" + conv + ":" + root, Key: "fx:" + conv + ":" + ts, Type: "message",
		OccurredAt: at, Own: own}
	if !own {
		it.Actor = "fx:user:dana"
	}
	return it
}

func closesOf(body []byte) string {
	s := string(body)
	i := strings.Index(s, `"closes":`)
	if i < 0 {
		return ""
	}
	return s[i : i+strings.Index(s[i:], "]")+1]
}

// An own top-level reply bounded by close_siblings_before closes the earlier top-level asks of its conversation, from
// this pass and from the outbox. Not an ask posted after it (decided earlier because search indexed it late), not a
// question inside a thread, and not another conversation's.
func TestCloseSiblingsBefore(t *testing.T) {
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	asks := []source.Item{
		dmItem("D1", "100.000000", "100.000000", false, at),
		dmItem("D1", "200.000000", "200.000000", false, at.Add(time.Minute)),
		// A question in the thread under 100: on 100's subject, keyed by its own ts.
		dmItem("D1", "100.000000", "250.000000", false, at.Add(2*time.Minute)),
		// Posted after the reply, collected before it.
		dmItem("D1", "400.000000", "400.000000", false, at.Add(3*time.Minute)),
		dmItem("D2", "150.000000", "150.000000", false, at.Add(4*time.Minute)),
	}
	reply := dmItem("D1", "300.000000", "300.000000", true, at.Add(5*time.Minute))
	reply.CloseSiblingsBefore = "300.000000"
	plain := dmItem("D1", "500.000000", "500.000000", true, at.Add(6*time.Minute))
	const want = `"closes":[{"key":"fx:D1:100.000000"},{"key":"fx:D1:200.000000"}]`

	// One dry pass: the asks are added in this pass.
	f := newFixture(t)
	f.src.items = append(append([]source.Item{}, asks...), reply, plain)
	r, done := f.runner(false, nil)
	r.Rules = mustRules(t, askRules)
	rep, err := r.Run(context.Background())
	done()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range rep.Items {
		got[p.Item.Key] = closesOf(p.Body)
	}
	if got["fx:D1:300.000000"] != want {
		t.Fatalf("reply closes %s", got["fx:D1:300.000000"])
	}
	if got["fx:D1:500.000000"] != "" {
		t.Fatalf("an own act without a bound closed %s", got["fx:D1:500.000000"])
	}

	// Two send passes: the asks are in the outbox when the reply arrives.
	f = newFixture(t)
	f.src.items = asks
	r, done = f.runner(true, nil)
	r.Rules = mustRules(t, askRules)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	done()
	f.src.items = []source.Item{reply}
	r, done = f.runner(true, nil)
	r.Rules = mustRules(t, askRules)
	defer done()
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, ok := r.Outbox.Get("fx:D1:300.000000")
	if !ok || closesOf(e.Body) != want {
		t.Fatalf("reply body %s", e.Body)
	}
	for subject, n := range map[string]int{"fx:dm:winton:D1:100.000000": 1, "fx:dm:winton:D1:400.000000": 1, "fx:dm:winton:D2:150.000000": 1, "fx:dm:winton:D1:200.000000": 0} {
		if open := r.Outbox.OpenAsks(subject); len(open) != n {
			t.Fatalf("%s: open %v", subject, open)
		}
	}
	// The stored decision keeps the bound, so a retry closes the same asks.
	var d project.Decision
	if err := json.Unmarshal(e.Decision, &d); err != nil || d.Item.CloseSiblingsBefore != "300.000000" {
		t.Fatalf("decision %s", e.Decision)
	}
}

func mustRules(t *testing.T, y string) *rules.File {
	t.Helper()
	f, err := rules.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

const deletionRules = `rules:
  - name: deleted
    match: {type: message_deleted}
    tier: send
    close_by_others: true
  - name: asks
    match: {own: false}
    tier: send
    ask: me
`

// A deletion that names its ask with close_only closes that ask only, though another ask is open on the same
// subject (two asks in one thread), in a dry pass and from the outbox; the stored decision keeps the names.
func TestCloseOnly(t *testing.T) {
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	thread := "fx:dm:winton:C1:100.000000"
	first := source.Item{Source: "fx", Subject: thread, Key: "fx:C1:100.000000", Type: "message", OccurredAt: at, Actor: "fx:user:dana"}
	second := source.Item{Source: "fx", Subject: thread, Key: "fx:C1:200.000000", Type: "message", OccurredAt: at.Add(time.Minute), Actor: "fx:user:bob"}
	deleted := source.Item{Source: "fx", Subject: thread, Key: "fx:C1:200.000000:deleted", Type: "message_deleted",
		OccurredAt: at.Add(2 * time.Minute), Version: "deleted", CloseOnly: []string{"fx:C1:200.000000"}}
	const want = `"closes":[{"key":"fx:C1:200.000000"}]`

	f := newFixture(t)
	f.src.items = []source.Item{first, second, deleted}
	r, done := f.runner(false, nil)
	r.Rules = mustRules(t, deletionRules)
	rep, err := r.Run(context.Background())
	done()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rep.Items {
		if p.Item.Key == deleted.Key && closesOf(p.Body) != want {
			t.Fatalf("dry pass closes %s", closesOf(p.Body))
		}
	}

	f = newFixture(t)
	f.src.items = []source.Item{first, second}
	r, done = f.runner(true, nil)
	r.Rules = mustRules(t, deletionRules)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	done()
	f.src.items = []source.Item{deleted}
	r, done = f.runner(true, nil)
	r.Rules = mustRules(t, deletionRules)
	defer done()
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, ok := r.Outbox.Get(deleted.Key)
	if !ok || closesOf(e.Body) != want {
		t.Fatalf("deletion body %s", e.Body)
	}
	if open := r.Outbox.OpenAsks(thread); len(open) != 1 || open[0] != first.Key {
		t.Fatalf("open after the deletion: %v", open)
	}
	var d project.Decision
	if err := json.Unmarshal(e.Decision, &d); err != nil || len(d.Item.CloseOnly) != 1 || d.Item.CloseOnly[0] != second.Key {
		t.Fatalf("decision %s", e.Decision)
	}
}
