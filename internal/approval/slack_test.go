package approval

import (
	"bytes"
	"testing"
	"time"
)

func TestSlackRecordsKeepTheirIntakeSubject(t *testing.T) {
	const root = "1799999800.000001"
	const account = "slack:user:U0ACCOUNT"
	const channel = "C0FIXTURE"
	const native = "slack:message:C0FIXTURE:1800000000.000002"
	at := time.Date(2026, 11, 23, 23, 59, 0, 0, time.Local)
	for _, tc := range []struct {
		name       string
		visibility SlackVisibility
		thread     any
		subject    string
	}{
		{"public thread", SlackPublic, root, "slack:" + channel + ":" + root},
		{"public day", SlackPublic, nil, "slack:" + channel + ":2026-11-23"},
		{"private channel", SlackPrivate, root, "signal:actor:" + who.Person},
		{"direct message", SlackDM, nil, "signal:actor:" + who.Person},
		{"group direct message", SlackGroupDM, nil, "signal:actor:" + who.Person},
		{"no lookup", "", root, "signal:actor:" + who.Person},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := scratch(t)
			j := open(t, dir, at)
			dst := Object{"workspace": "T0WORKSPACE", "channel": channel, "thread_ts": tc.thread}
			r := Request{Type: Post, Agent: "test", Reason: "Synthetic", Context: "slack:C0UNVERIFIED:1799999801.000001",
				Params: Object{"workspace": dst["workspace"], "channel": channel, "thread_ts": tc.thread, "text": "Slack fixture text."}}
			var p Proposal
			var err error
			if tc.visibility == "" {
				p, err = j.Add(r, PeerLabel{}, at)
			} else {
				p, err = j.AddSlack(r, PeerLabel{}, at, SlackPlacement{Account: account, Visibility: tc.visibility,
					Conversation: "Fixture conversation name", WorkspaceLabel: "Fixture workspace name", PublicDay: "1990-01-01"})
			}
			must(t, err)
			bound := Binding{Origin: "https://slack.com", Account: account, Destination: dst}
			if p.Slack != nil {
				wrong := bound
				wrong.Account = "slack:user:U0OTHER"
				if _, err := Freeze(p, wrong, ""); err != Invalid("/account") {
					t.Fatal("freeze accepted a different Slack account")
				}
				p.Slack.Visibility = SlackPublic // caller's copy must not reroute later records
			}
			f, err := Freeze(p, bound, "")
			must(t, err)
			afterMidnight := at.Add(2 * time.Minute)
			must(t, j.ApproveAll([]Frozen{f}, []byte("fixture-credential"), nil, afterMidnight))
			v, err := j.StartAsserted(p.ID, afterMidnight)
			must(t, err)
			if !slackOperation.matches(v.SlackOperation) {
				t.Fatal("start did not allocate a v4 operation UUID")
			}
			must(t, j.Close())
			j = open(t, dir, afterMidnight.Add(time.Second))
			if status(t, j, p.ID, afterMidnight, "outcome_unknown").SlackOperation != v.SlackOperation {
				t.Fatal("operation did not survive restart")
			}
			must(t, j.SetResult(p.ID, Result{ConfirmedBy: "read_back", Native: native}, afterMidnight.Add(2*time.Second)))
			for _, kind := range []string{"proposed", "approved", "started", "outcome_unknown", "executed"} {
				b, err := j.beginDelivery(who)
				must(t, err)
				r, err := ParseObject(b)
				must(t, err)
				if r["subject"] != tc.subject || r["type"] != "app.action."+kind {
					t.Fatalf("%s record: got %v / %v; want subject %s", kind, r["type"], r["subject"], tc.subject)
				}
				for _, local := range []string{"Fixture conversation name", "Fixture workspace name", "slack_placement", v.SlackOperation, "fixture-credential", "C0UNVERIFIED"} {
					if bytes.Contains(b, []byte(local)) {
						t.Fatalf("%s record contained local-only data", kind)
					}
				}
				if m := obj(r["metadata"]); kind == "executed" && (m["native"] != native || m["confirmed_by"] != "read_back") {
					t.Fatal("record omitted native message id or read-back confirmation")
				}
				must(t, j.endDelivery("ack", "evt_01m6a6ft00e40r2081040g2081", false))
			}
		})
	}
}
