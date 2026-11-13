package slack

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/slack/slacktest"
	"github.com/CrossTerra/beacon/internal/source"
)

// A direct message's subject is the conversation and the local day of its root: top-level messages that day share
// it, a reply in a thread keeps its root's, and a public or private channel's thread is still its own.
func TestDaySubjects(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := newSource(t, srv)
	s.loc = time.FixedZone("PST", -8*3600)
	// 2026-11-08T06:00Z is 7 November in PST.
	early := formatTS(time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC))
	late := formatTS(time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC))
	for _, tc := range []struct {
		channel, root, kind, want string
	}{
		{"D0DANA", early, kindIM, "slack:dm:winton:D0DANA:2026-11-07"},
		{"D0DANA", late, kindIM, "slack:dm:winton:D0DANA:2026-11-08"},
		{"G0GROUP", late, kindMPIM, "slack:dm:winton:G0GROUP:2026-11-08"},
		{"C0SECRET", late, kindPrivate, "slack:dm:winton:C0SECRET:" + late},
		{"C0ENG", late, kindChannel, "slack:C0ENG:" + late},
	} {
		if got := s.subject(tc.channel, tc.root, tc.kind); got != tc.want {
			t.Errorf("%s %s: %s", tc.channel, tc.kind, got)
		}
	}
	reply := &message{channel: "D0DANA", ts: late, threadTS: early, user: "U0DANA", kind: kindIM}
	it, ok := s.item(reply, TypeDM, "")
	if !ok || it.Subject != "slack:dm:winton:D0DANA:2026-11-07" {
		t.Fatalf("a reply keeps its root's day: %+v", it)
	}
}

// The person's own message names the asks it answers on its day: a top-level message, the top-level messages before
// it; a reply in a thread, that thread and its root. Never a question in another thread or a message after it.
func TestOwnMessageClosesThatDay(t *testing.T) {
	s, _ := newSource(t, slacktest.New(t))
	day := "slack:dm:winton:D1:2026-11-08"
	p := &pass{cur: cursorData{}}
	p.cur.init()
	ask := func(ts, root string) {
		p.cur.Asked["D1/"+ts] = &asked{Subject: day, Root: root, Kind: kindIM}
	}
	ask("100.000001", "")           // top-level, before
	ask("150.000001", "100.000001") // a question in the thread under 100
	ask("160.000001", "")           // top-level, before
	ask("300.000001", "")           // top-level, after
	ask("170.000001", "")
	p.cur.Asked["D1/170.000001"].Subject = "slack:dm:winton:D1:1794100000.000000" // decided before day subjects
	p.cur.Asked["D2/120.000001"] = &asked{Subject: "slack:dm:winton:D2:2026-11-08", Kind: kindIM}
	own := func(ts, root string) source.Item {
		f := map[string]string{"channel": "D1", "channel_type": kindIM, "ts": ts}
		if root != "" {
			f["thread_ts"] = root
		}
		return source.Item{Source: "slack", Subject: day, Key: "slack:D1:" + ts, Own: true, Fields: f}
	}
	items := []source.Item{own("200.000001", ""), own("210.000001", "100.000001"), own("220.000001", "900.000001"),
		{Source: "slack", Subject: day, Key: "slack:D1:230.000001", Fields: map[string]string{"channel": "D1", "channel_type": kindIM, "ts": "230.000001"}}}
	s.closes(p, items)
	for i, want := range []string{
		"slack:D1:100.000001,slack:D1:160.000001",
		"slack:D1:100.000001,slack:D1:150.000001",
		"slack:D1:220.000001", // answers nothing: its own key, which is never an ask
		"",                    // not the person's
	} {
		if got := strings.Join(items[i].CloseOnly, ","); got != want {
			t.Errorf("%s: %s, want %s", items[i].Key, got, want)
		}
	}
}

// A day's thread is titled for the conversation and the day, and is retitled the same way.
func TestDayThreadTitle(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := namedSource(t, srv, nil)
	if _, err := s.Collect(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.ThreadTitle(context.Background(), "slack:dm:winton:D0DANA:2026-11-08")
	if err != nil || got != "DM with Dana Scully · Sun 8 Nov" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := s.ThreadTitle(context.Background(), "slack:C0ENG:2026-11-08"); err == nil {
		t.Fatal("a channel subject with a day")
	}
}
