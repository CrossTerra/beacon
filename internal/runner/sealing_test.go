package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/lease"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/queue"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/seal"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
	"github.com/CrossTerra/beacon/internal/source"
	"go.yaml.in/yaml/v3"
)

// sealSource yields personal and shared items with the fields the raise reads.
type sealSource struct{ items []source.Item }

func (s *sealSource) Name() string { return "fx" }
func (s *sealSource) Fields() []string {
	return []string{"subject", "body", "from", "labels", "thread_participants", "summary", "title", "meeting", "channel_type"}
}
func (s *sealSource) StateFields() []string { return []string{"unread", "labels"} }
func (s *sealSource) Hosts() []string       { return nil }
func (s *sealSource) Collect(context.Context, []byte) (source.Collected, error) {
	return source.Collected{Items: append([]source.Item(nil), s.items...)}, nil
}

const sealRules = `rules:
  - name: never-bulk
    match: {fields: {labels: bulk}}
    tier: never
  - name: owner-mail
    match: {fields: {from: boss@rippling.com}}
    tier: send
    seal: owner
    fields: [subject, body, from]
  - name: plain-own
    match: {own: true}
    tier: send
    fields: [subject]
  - name: plain-body
    match: {fields: {labels: plainbody}}
    tier: send
    fields: [subject, body]
  - name: plain-subject
    match: {fields: {labels: plainsubject}}
    tier: send
    seal: owner+agents
    plain: [subject]
    fields: [subject, from]
  - name: public
    match: {fields: {channel_type: channel}}
    tier: send
    fields: [body]
  - name: mail
    match: {}
    tier: send
    seal: owner+agents
    fields: [subject, body, from, actor]
    state: [unread, labels]
`

const sealOwner = `{words: [salary, offer, {exact: PIP}], senders: [remotely.works], labels: [Confidential]}`

var sealAt = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

func mail(n int, subject string, mod func(*source.Item)) source.Item {
	obs := sealAt
	it := source.Item{Source: "fx", Subject: "gmail:winton:thread:t" + subject, Key: "fx:t" + subject + ":m" + string(rune('a'+n)),
		Type: "received", EventTitle: "New message", OccurredAt: sealAt.Add(time.Duration(n) * time.Minute), Actor: "email:dana@vendor.example",
		Native: "m" + string(rune('a'+n)),
		Fields: map[string]string{"subject": "CANARY-SUBJECT-" + subject, "body": "CANARY-BODY", "from": "dana@vendor.example", "labels": "INBOX"},
		State:  map[string]string{"unread": "true", "labels": "INBOX"}, ObservedAt: &obs, Version: "m" + string(rune('a'+n)),
		Local: &source.Local{Text: map[string]string{"subject": "CANARY-SUBJECT-" + subject, "body": "CANARY-BODY"},
			Addresses: map[string][]string{"thread_participants": {"dana@vendor.example", "winton@crossterra.co"}}, Labels: []string{"INBOX"}},
	}
	if mod != nil {
		mod(&it)
	}
	return it
}

type sealFixture struct {
	*fixture
	src *sealSource
}

func newSealFixture(t *testing.T, items ...source.Item) *sealFixture {
	f := &sealFixture{fixture: newFixture(t), src: &sealSource{items: items}}
	f.server.Restrict("gmail:*", "calendar:*", "drive:*", "slack:dm:*")
	return f
}

// runner opens the state directory, as `beacon run` does after a restart: the floor is read from disk.
func (f *sealFixture) runner(send bool) (*Runner, func()) {
	f.t.Helper()
	rs, err := rules.Parse([]byte(sealRules))
	if err != nil {
		f.t.Fatal(err)
	}
	var owner seal.OwnerList
	if err := yaml.Unmarshal([]byte(sealOwner), &owner); err != nil {
		f.t.Fatal(err)
	}
	r := &Runner{Sources: []source.Source{f.src}, Rules: rs, Person: project.Person{Handle: "@winton"}, Send: send,
		Log: logs.New(&f.log), Now: f.clock.now, SealOwner: &owner, Personal: map[string]string{"fx": "gmail:"}, Restrictions: f.client}
	var ob *outbox.Outbox
	var q *queue.Queue
	if send {
		ob, err = outbox.Open(f.dir, f.clock.now)
		if err == nil {
			q, err = queue.Open(f.dir, ob)
		}
		if err == nil {
			r.Floor, err = seal.OpenFloor(f.dir)
		}
	} else {
		ob, err = outbox.Read(f.dir)
		if err == nil {
			q, err = queue.Read(f.dir)
		}
		if err == nil {
			r.Floor, err = seal.ReadFloor(f.dir)
		}
	}
	if err != nil {
		f.t.Fatal(err)
	}
	r.Outbox, r.Queue, r.Writer = ob, q, f.client
	r.Lease = &lease.Lease{Outbox: ob, Writer: f.client, Person: "@winton", Run: "test:lease", Period: 5 * time.Minute, Now: f.clock.now}
	return r, func() { ob.Close() }
}

func (f *sealFixture) pass(send bool) *Report {
	f.t.Helper()
	r, done := f.runner(send)
	defer done()
	rep, err := r.Run(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return rep
}

// level is the level Signal received for key, "" for a plain event, "-" when nothing was written.
func (f *sealFixture) level(key string) string {
	f.t.Helper()
	ev, ok := f.server.Event(key)
	if !ok {
		return "-"
	}
	if p := signaltest.SealedProblem(ev.Body); p != "" {
		f.t.Fatalf("%s: Signal would refuse %s", key, p)
	}
	var b struct {
		Sealed *struct {
			Level string `json:"level"`
		} `json:"sealed"`
	}
	if err := json.Unmarshal(ev.Body, &b); err != nil {
		f.t.Fatal(err)
	}
	if b.Sealed == nil {
		return ""
	}
	return b.Sealed.Level
}

func planned(rep *Report, key string) Planned {
	for _, p := range rep.Items {
		if p.Item.Key == key {
			return p
		}
	}
	return Planned{}
}

func TestRaiseKinds(t *testing.T) {
	items := []source.Item{
		mail(0, "plain", nil),
		mail(1, "word", func(it *source.Item) { it.Local.Text["subject"] = "Your Salary review" }),
		mail(2, "bodyword", func(it *source.Item) { it.Local.Text["body"] = "the offer letter" }),
		mail(3, "sender", func(it *source.Item) {
			it.Local.Addresses["thread_participants"] = []string{"pay@eu.remotely.works", "winton@crossterra.co"}
		}),
		mail(4, "label", func(it *source.Item) { it.Local.Labels = []string{"CATEGORY_UPDATES", "Confidential"} }),
		mail(5, "exact", func(it *source.Item) { it.Local.Text["subject"] = "pip install" }),
		// From plain to owner: the raise seals a rule that did not.
		mail(6, "own", func(it *source.Item) { it.Own = true; it.Local.Text["subject"] = "salary" }),
		// Never stays never.
		mail(7, "never", func(it *source.Item) { it.Fields["labels"] = "bulk"; it.Local.Text["subject"] = "salary" }),
		// Owner stays owner.
		mail(8, "owner", func(it *source.Item) { it.Fields["from"] = "boss@rippling.com" }),
	}
	f := newSealFixture(t, items...)
	rep := f.pass(true)
	want := map[string]string{
		"plain": "owner+agents", "word": "owner", "bodyword": "owner", "sender": "owner", "label": "owner",
		"exact": "owner+agents", "own": "owner", "never": "-", "owner": "owner",
	}
	for i, it := range items {
		name := strings.TrimPrefix(it.Subject, "gmail:winton:thread:t")
		if got := f.level(it.Key); got != want[name] {
			t.Errorf("%s (%d): level %q, want %q", name, i, got, want[name])
		}
	}
	if p := planned(rep, items[1].Key); !p.Raised || p.Level != seal.Owner {
		t.Errorf("raised item: %+v", p)
	}
	if p := planned(rep, items[0].Key); p.Raised {
		t.Errorf("an item at its rule's level is not raised")
	}
	if p := planned(rep, items[7].Key); p.Tier != rules.Never || p.Level != seal.None {
		t.Errorf("never revived: %+v", p)
	}
	// The raise adds no field: the own act's rule lists only subject.
	ev, _ := f.server.Event(items[6].Key)
	if strings.Contains(string(ev.Body), "CANARY-BODY") {
		t.Errorf("the raise added a field: %s", ev.Body)
	}
	log := f.log.String()
	for _, banned := range []string{"CANARY", "salary", "Salary", "offer", "remotely", "Confidential", "word "} {
		if strings.Contains(log, banned) {
			t.Fatalf("log holds %q:\n%s", banned, log)
		}
	}
	if !strings.Contains(log, `"msg":"sealed","owner":6,"owner_agents":2`) || !strings.Contains(log, `"msg":"raised","items":5`) {
		t.Fatalf("log counts:\n%s", log)
	}
}

func TestSensitiveSharedAndBodyNeedsSeal(t *testing.T) {
	shared := mail(0, "shared", func(it *source.Item) {
		it.Subject, it.Fields["channel_type"] = "slack:C1:1794132000.000100", "channel"
		it.Local.Text["body"] = "who knows the salary bands"
	})
	quiet := mail(1, "quiet", func(it *source.Item) { it.Subject, it.Fields["channel_type"] = "slack:C1:1794132000.000200", "channel" })
	body := mail(2, "body", func(it *source.Item) { it.Fields["labels"] = "plainbody" })
	f := newSealFixture(t, shared, quiet, body)
	rep := f.pass(true)
	if p := planned(rep, shared.Key); p.Tier != rules.Hold || p.SealHeld != logs.ReasonSensitiveShared || p.Body != nil {
		t.Fatalf("shared: %+v", p)
	}
	if p := planned(rep, body.Key); p.Tier != rules.Hold || p.SealHeld != logs.ReasonBodyNeedsSeal || p.Body != nil {
		t.Fatalf("body: %+v", p)
	}
	if f.level(shared.Key) != "-" || f.level(body.Key) != "-" {
		t.Fatal("a held item was sent")
	}
	if f.level(quiet.Key) != "" {
		t.Fatal("a public channel body without a match leaves plain")
	}
	q, _ := queue.Read(f.dir)
	reasons := map[string]string{}
	for _, h := range q.HeldItems() {
		reasons[h.Key] = h.Reason
	}
	if reasons[shared.Key] != "sensitive_shared" || reasons[body.Key] != "body_needs_seal" {
		t.Fatalf("held reasons %v", reasons)
	}
	log := f.log.String()
	if !strings.Contains(log, `"msg":"seal_held","reason":"body_needs_seal","items":1`) ||
		!strings.Contains(log, `"msg":"seal_held","reason":"sensitive_shared","items":1`) || strings.Contains(log, "salary") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestSubjectFloorAcrossPassesAndRestarts(t *testing.T) {
	first := mail(0, "x", func(it *source.Item) { it.Local.Text["subject"] = "offer" })
	f := newSealFixture(t, first)
	f.pass(true)
	if f.level(first.Key) != "owner" {
		t.Fatal("first item not raised")
	}
	// A later pass, from a new process: a reply on the same subject with nothing to match, under owner+agents, and an
	// own act under a plain rule, both go at owner.
	reply := mail(1, "x", nil)
	own := mail(2, "x", func(it *source.Item) { it.Own = true })
	other := mail(3, "y", nil)
	f.src.items = []source.Item{reply, own, other}
	f.clock.t = f.clock.t.Add(time.Minute)
	f.pass(true)
	if f.level(reply.Key) != "owner" || f.level(own.Key) != "owner" || f.level(other.Key) != "owner+agents" {
		t.Fatalf("floor: reply %q own %q other %q", f.level(reply.Key), f.level(own.Key), f.level(other.Key))
	}
	// A dry run reads the floor too.
	dry := mail(4, "x", nil)
	f.src.items = []source.Item{dry}
	rep := f.pass(false)
	if p := planned(rep, dry.Key); p.Level != seal.Owner || !p.Raised {
		t.Fatalf("dry run: %+v", p)
	}
}

func TestFloorWithinOnePass(t *testing.T) {
	// The raised item comes first, the plain reply after it in the same pass.
	a := mail(0, "z", func(it *source.Item) { it.Local.Text["body"] = "PIP" })
	b := mail(1, "z", nil)
	f := newSealFixture(t, a, b)
	rep := f.pass(false)
	if planned(rep, a.Key).Level != seal.Owner || planned(rep, b.Key).Level != seal.Owner {
		t.Fatalf("%+v %+v", planned(rep, a.Key), planned(rep, b.Key))
	}
}

func TestColleagueReplyInARemotelyThreadOnAFirstRun(t *testing.T) {
	// Only the colleague's reply is seen; the thread's participants include the Remotely sender.
	reply := mail(0, "remotely", func(it *source.Item) {
		it.Fields["from"] = "lee@crossterra.co"
		it.Local.Addresses["thread_participants"] = []string{"lee@crossterra.co", "payroll@remotely.works", "winton@crossterra.co"}
	})
	f := newSealFixture(t, reply)
	f.pass(true)
	if f.level(reply.Key) != "owner" {
		t.Fatalf("level %q", f.level(reply.Key))
	}
}

func TestCalendarAndNotesTakeTheirLevel(t *testing.T) {
	event := func(n int, id string, mod func(*source.Local)) source.Item {
		it := mail(n, id, nil)
		it.Subject = "calendar:winton:" + id
		it.Fields = map[string]string{"summary": "Weekly sync"}
		it.Local = &source.Local{Text: map[string]string{"summary": "Weekly sync"}, Addresses: map[string][]string{"attendee_addresses": {"lee@crossterra.co"}}}
		mod(it.Local)
		return it
	}
	bySummary := event(0, "e1", func(l *source.Local) { l.Text["summary"] = "Offer discussion" })
	byAttendee := event(1, "e2", func(l *source.Local) {
		l.Addresses["attendee_addresses"] = []string{"a@remotely.works", "lee@crossterra.co"}
	})
	quiet := event(2, "e3", func(*source.Local) {})
	note := func(n int, id, meeting string, mod func(*source.Local)) source.Item {
		it := mail(n, id, nil)
		it.Subject = "drive:winton:" + id
		it.Fields = map[string]string{"title": "Notes by Gemini", "meeting": "Weekly sync"}
		it.Local = &source.Local{Text: map[string]string{"title": "Notes by Gemini", "meeting": "Weekly sync"}, Meeting: "calendar:winton:" + meeting}
		if mod != nil {
			mod(it.Local)
		}
		return it
	}
	// Notes for e1, which was sealed at owner, take owner; notes for e3 stay at their rule's level; notes for a
	// meeting whose attendees match are raised themselves.
	n1 := note(5, "f1", "e1", nil)
	n3 := note(6, "f3", "e3", nil)
	n4 := note(7, "f4", "e9", func(l *source.Local) { l.Addresses = map[string][]string{"meeting_attendees": {"x@remotely.works"}} })
	f := newSealFixture(t, bySummary, byAttendee, quiet, n1, n3, n4)
	f.pass(true)
	for key, want := range map[string]string{bySummary.Key: "owner", byAttendee.Key: "owner", quiet.Key: "owner+agents",
		n1.Key: "owner", n3.Key: "owner+agents", n4.Key: "owner"} {
		if got := f.level(key); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
}

func TestRetryNeverSealsLower(t *testing.T) {
	it := mail(0, "r", func(it *source.Item) { it.Local.Text["subject"] = "salary" })
	f := newSealFixture(t, it)
	f.server.Script = func(n int, req signaltest.Request) signaltest.Reply {
		if req.Key == it.Key {
			return signaltest.Reply{Status: 422, Body: `{"error":{"code":"subject_not_found","message":"No."}}`}
		}
		return signaltest.Reply{}
	}
	f.pass(true)
	if f.level(it.Key) != "-" {
		t.Fatal("expected a refusal")
	}
	r, done := f.runner(true)
	defer done()
	// The rules now send it plain, without body; the stored level holds.
	plain, err := rules.Parse([]byte("rules:\n  - name: mail\n    match: {}\n    tier: send\n    fields: [subject, from]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r.Rules = plain
	f.server.Script = nil
	rep, err := r.RetryRejected(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sent != 1 || f.level(it.Key) != "owner" {
		t.Fatalf("retry: %+v, level %q", rep, f.level(it.Key))
	}
	ev, _ := f.server.Event(it.Key)
	if strings.Contains(string(ev.Body), "CANARY-BODY") || strings.Contains(string(ev.Body[:strings.Index(string(ev.Body), `"sealed"`)]), "CANARY-SUBJECT") {
		t.Fatalf("retry widened: %s", ev.Body)
	}
}

// A raised item leaves nothing plain: its plain subject could hold the word that raised it.
func TestRaisedItemLeavesNothingPlain(t *testing.T) {
	quiet := mail(0, "q", func(it *source.Item) { it.Fields["labels"] = "plainsubject" })
	loud := mail(1, "l", func(it *source.Item) {
		it.Fields["labels"] = "plainsubject"
		it.Fields["subject"] = "CANARY salary review"
		it.Local.Text["subject"] = it.Fields["subject"]
	})
	f := newSealFixture(t, quiet, loud)
	f.pass(true)
	outside := func(key string) string {
		ev, _ := f.server.Event(key)
		b := string(ev.Body)
		return b[:strings.Index(b, `"sealed"`)]
	}
	if f.level(quiet.Key) != "owner+agents" || !strings.Contains(outside(quiet.Key), "CANARY-SUBJECT-q") {
		t.Fatalf("quiet: %s", outside(quiet.Key))
	}
	if f.level(loud.Key) != "owner" || strings.Contains(outside(loud.Key), "salary") {
		t.Fatalf("loud: %s", outside(loud.Key))
	}
}
