package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/seal"
	"github.com/CrossTerra/beacon/internal/slack/slacktest"
	"github.com/CrossTerra/beacon/internal/source"
)

// Replies are read in channel threads the person was mentioned or posted in, public or private, whether or not the
// config names the channel. A reply before the person joined the thread is not an item.
func TestThreadRepliesInChannels(t *testing.T) {
	srv := slacktest.New(t)
	recorded := srv.Routes["/api/conversations.replies"]
	srv.Routes["/api/conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("inclusive") == "" && q.Get("channel") == "C0SECRET" && q.Get("ts") == "1794134000.000450" {
			io.WriteString(w, `{"ok":true,"messages":[
  {"type":"message","user":"U0HR","text":"<@U0WINTON> the salary numbers","ts":"1794134000.000450","thread_ts":"1794134000.000450"},
  {"type":"message","user":"U0DANA","text":"sent them","ts":"1794134100.000460","thread_ts":"1794134000.000450"}
]}`)
			return
		}
		recorded(w, r)
	}
	s, _ := newSource(t, srv)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items)
	for key, want := range map[string]struct{ subject, kind string }{
		// Private channel, followed from a mention at its root.
		"slack:C0SECRET:1794134100.000460": {"slack:dm:winton:C0SECRET:1794134000.000450", "private"},
		// Public channel the config does not name, followed from a mention at its root.
		"slack:C0ENG:1794134500.000750": {"slack:C0ENG:1794133800.000400", "channel"},
		// Public channel, followed from the person's own reply; Carol's bump before it is not an item.
		"slack:C0ENG:1794134400.000700": {"slack:C0ENG:1794120000.000500", "channel"},
	} {
		it, ok := m[key]
		if !ok || it.Type != TypeThreadReply || it.Subject != want.subject || it.Fields["channel_type"] != want.kind || it.Addressed || it.Own {
			t.Errorf("%s: %+v", key, it)
		}
	}
	if _, ok := m["slack:C0ENG:1794125000.000800"]; ok {
		t.Error("a reply before the person joined the thread is an item")
	}
}

// threadReplyRules are the rules sources.md gives for thread replies: recorded without an ask, and closing the
// mention the thread holds. In a private channel, sealed with the text.
const threadReplyRules = `rules:
  - name: slack-thread-replies-private
    match:
      source: slack
      type: thread_reply
      fields:
        channel_type: private
    tier: send
    close_by_others: true
    seal: owner+agents
    plain: [title]
    fields: [title, channel, channel_type, permalink, author, actor, body, author_name, channel_name]

  - name: slack-thread-replies
    match: {source: slack, type: thread_reply, fields: {channel_type: channel}}
    tier: send
    close_by_others: true
    fields: [title, channel, channel_type, permalink, author, actor, author_name, channel_name]
`

func TestThreadReplyRuleClosesTheMention(t *testing.T) {
	f, err := rules.Parse([]byte(threadReplyRules))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range f.Check(map[string]rules.Source{"slack": {Fields: Fields, StateFields: StateFields, Identity: true,
		Personal: rules.PersonalSome, PersonalField: "channel_type", PersonalValues: PersonalKinds}}) {
		// The private rule leaves the conversation's title plain, as the starter mention rules do: a warning.
		if !p.Warning {
			t.Errorf("rules check: %s", p)
		}
	}
	person := project.Person{Handle: "@winton", Identities: map[string]string{"slack": "slack:user:" + slacktest.User}}
	for _, tc := range []struct {
		subject, kind, ask string
		level              seal.Level
	}{
		{"slack:C0ENG:1794133800.000400", "channel", "slack:C0ENG:1794133800.000400", seal.None},
		{"slack:dm:winton:C0SECRET:1794134000.000450", "private", "slack:C0SECRET:1794134000.000450", seal.Agents},
	} {
		channel := "C0ENG"
		if tc.kind == "private" {
			channel = "C0SECRET"
		}
		it := source.Item{
			Source: "slack", Subject: tc.subject, Key: "slack:" + channel + ":1794134500.000750", Type: TypeThreadReply,
			EventTitle: "Thread reply", OccurredAt: tsTime("1794134500.000750"), Actor: "slack:user:U0CAROL",
			Fields:  map[string]string{"channel": channel, "channel_type": tc.kind, "ts": "1794134500.000750", "author": "U0CAROL", "body": "done"},
			Version: "1794134500.000750",
		}
		d := f.Decide(it)
		if d.Tier != rules.Send {
			t.Fatalf("%s: tier %s", tc.kind, d.Tier)
		}
		_, body, err := project.Project(project.Input{Item: it, Rule: d.Rule, Person: person, OpenAsks: []string{tc.ask}, Level: tc.level})
		if err != nil {
			t.Fatal(err)
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if _, asks := req["to"]; asks {
			t.Errorf("%s: a thread reply asks: %s", tc.kind, body)
		}
		closes, _ := req["closes"].([]any)
		var first map[string]any
		if len(closes) == 1 {
			first, _ = closes[0].(map[string]any)
		}
		if len(closes) != 1 || first["key"] != tc.ask {
			t.Errorf("%s: closes %v", tc.kind, req["closes"])
		}
	}
}
