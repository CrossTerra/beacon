package runner

import (
	"context"
	"strings"
	"testing"
	"time"

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

func dmItem(conv, ts string, own, siblings bool, at time.Time) source.Item {
	it := source.Item{Source: "fx", Subject: "fx:dm:winton:" + conv + ":" + ts, Key: "fx:" + conv + ":" + ts, Type: "message",
		OccurredAt: at, Own: own, CloseSiblings: siblings}
	if !own {
		it.Actor = "fx:user:dana"
	}
	return it
}

// An own act with close_siblings closes the open asks on the other subjects of its conversation, from this pass and
// from the outbox, and no others.
func TestCloseSiblings(t *testing.T) {
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	asksFirst := []source.Item{
		dmItem("D1", "1", false, false, at),
		dmItem("D1", "2", false, false, at.Add(time.Minute)),
		dmItem("D2", "3", false, false, at.Add(2*time.Minute)),
	}
	reply := dmItem("D1", "4", true, true, at.Add(3*time.Minute))
	plain := dmItem("D1", "5", true, false, at.Add(4*time.Minute))

	closesOf := func(body []byte) string {
		s := string(body)
		i := strings.Index(s, `"closes":`)
		if i < 0 {
			return ""
		}
		return s[i : i+strings.Index(s[i:], "]")+1]
	}

	// One dry pass: the asks are added in this pass.
	f := newFixture(t)
	f.src.items = append(append([]source.Item{}, asksFirst...), reply, plain)
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
	if got["fx:D1:4"] != `"closes":[{"key":"fx:D1:1"},{"key":"fx:D1:2"}]` {
		t.Fatalf("reply closes %s", got["fx:D1:4"])
	}
	if got["fx:D1:5"] != "" {
		t.Fatalf("an own act without close_siblings closed %s", got["fx:D1:5"])
	}

	// Two send passes: the asks are in the outbox when the reply arrives.
	f = newFixture(t)
	f.src.items = asksFirst
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
	e, ok := r.Outbox.Get("fx:D1:4")
	if !ok || closesOf(e.Body) != `"closes":[{"key":"fx:D1:1"},{"key":"fx:D1:2"}]` {
		t.Fatalf("reply body %s", e.Body)
	}
	if open := r.Outbox.OpenAsks("fx:dm:winton:D2:3"); len(open) != 1 {
		t.Fatalf("the other conversation's ask was closed: %v", open)
	}
	if open := r.Outbox.OpenAsksSiblings("fx:dm:winton:D1:9"); len(open) != 0 {
		t.Fatalf("still open: %v", open)
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
