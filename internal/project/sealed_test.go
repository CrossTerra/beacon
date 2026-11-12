package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/seal"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
	"github.com/CrossTerra/beacon/internal/source"
)

func mailItem() source.Item {
	obs := time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)
	return source.Item{
		Source: "gmail", Subject: "gmail:winton:thread:18c2", Key: "gmail:18c2:18c9", Type: "received",
		EventTitle: "Reply received", OccurredAt: time.Date(2026, 11, 11, 17, 0, 0, 0, time.UTC),
		Actor: "email:rahul@crossterra.co", Addressed: true, Native: "18c9",
		Fields: map[string]string{
			"subject": "CANARY-SUBJECT", "body": "CANARY-BODY", "from": "rahul@crossterra.co",
			"to": "winton@crossterra.co", "labels": "CANARY-LABEL", "link": "https://mail.google.com/mail/u/0/#all/18c2",
		},
		References: []string{"jira:CANARY-1"},
		State:      map[string]string{"unread": "true", "starred": "false", "labels": "INBOX, CANARY-STATE", "last_from": "rahul@crossterra.co"},
		ObservedAt: &obs, Version: "18c9",
	}
}

func sealedRule(level string, plain []string, fields ...string) *rules.Rule {
	return &rules.Rule{Name: "r", Tier: rules.Send, Seal: level, Plain: plain, Fields: fields, Ask: "me", Priority: "high",
		State: []string{"unread", "starred", "labels", "last_from"}}
}

func TestSealedGolden(t *testing.T) {
	_, data, err := Project(Input{Item: mailItem(), Rule: sealedRule("owner+agents", []string{"subject"}, "subject", "body", "actor", "from", "references"),
		Person: person, Level: seal.Agents, OpenAsks: nil})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"subject":"gmail:winton:thread:18c2","type":"gmail.received","key":"gmail:18c2:18c9","occurred_at":"2026-11-11T17:00:00.000Z","open":{"title":"CANARY-SUBJECT"},"to":["@winton"],"priority":"high","metadata":{"fields":{"subject":"CANARY-SUBJECT"},"native":"18c9"},"state":{"fields":{"starred":false,"unread":true},"observed_at":"2026-11-11T18:00:00.000Z","version":"18c9"},"sealed":{"level":"owner+agents","title":"Reply received","body":"CANARY-BODY","metadata":{"actor":"email:rahul@crossterra.co","fields":{"from":"rahul@crossterra.co"},"references":["jira:CANARY-1"]},"state":{"labels":"INBOX, CANARY-STATE","last_from":"rahul@crossterra.co"}}}`
	if string(data) != want {
		t.Fatalf("got  %s\nwant %s", data, want)
	}
	if p := signaltest.SealedProblem(data); p != "" {
		t.Fatalf("Signal would refuse %s", p)
	}
	// Owner, nothing plain: the thread is not titled, and only the envelope is outside sealed.
	_, data, err = Project(Input{Item: mailItem(), Rule: sealedRule("owner", nil, "subject", "body", "actor", "from", "to"), Person: person, Level: seal.Owner})
	if err != nil {
		t.Fatal(err)
	}
	want = `{"subject":"gmail:winton:thread:18c2","type":"gmail.received","key":"gmail:18c2:18c9","occurred_at":"2026-11-11T17:00:00.000Z","open":{},"to":["@winton"],"priority":"high","metadata":{"native":"18c9"},"state":{"fields":{"starred":false,"unread":true},"observed_at":"2026-11-11T18:00:00.000Z","version":"18c9"},"sealed":{"level":"owner","title":"Reply received","body":"CANARY-BODY","metadata":{"actor":"email:rahul@crossterra.co","fields":{"from":"rahul@crossterra.co","subject":"CANARY-SUBJECT","to":"winton@crossterra.co"}},"state":{"labels":"INBOX, CANARY-STATE","last_from":"rahul@crossterra.co"}}}`
	if string(data) != want {
		t.Fatalf("got  %s\nwant %s", data, want)
	}
}

// envelopeKeys are the only keys DEC-31 lets outside sealed.
var envelopeKeys = map[string]bool{"subject": true, "type": true, "key": true, "occurred_at": true, "open": true, "by": true,
	"to": true, "priority": true, "due_at": true, "closes": true, "outcome": true, "metadata": true, "state": true, "sealed": true}

// For every subset of fields and of plain, at both levels: no listed field is outside sealed unless plain, the
// envelope has only DEC-31's keys, and Signal's checks pass.
func TestSealedEnvelope(t *testing.T) {
	all := []string{"subject", "body", "from", "to", "labels", "link", rules.FieldActor, rules.FieldReferences}
	canary := map[string]string{
		"subject": "CANARY-SUBJECT", "body": "CANARY-BODY", "from": "rahul@crossterra.co", "to": "winton@crossterra.co",
		"labels": "CANARY-LABEL", "link": "https://mail.google.com/mail/u/0/#all/18c2", rules.FieldActor: "email:rahul@crossterra.co", rules.FieldReferences: "jira:CANARY-1",
	}
	plainable := map[string]bool{"subject": true, "from": true, "to": true, "labels": true, "link": true}
	for mask := 0; mask < 1<<len(all); mask++ {
		var listed []string
		for i, f := range all {
			if mask&(1<<i) != 0 {
				listed = append(listed, f)
			}
		}
		var candidates []string
		for _, f := range listed {
			if plainable[f] {
				candidates = append(candidates, f)
			}
		}
		for pm := 0; pm < 1<<len(candidates); pm++ {
			var plain []string
			isPlain := map[string]bool{}
			for i, f := range candidates {
				if pm&(1<<i) != 0 {
					plain = append(plain, f)
					isPlain[f] = true
				}
			}
			for _, level := range []seal.Level{seal.Agents, seal.Owner} {
				it := mailItem()
				_, data, err := Project(Input{Item: it, Rule: sealedRule(string(level), plain, listed...), Person: person, Level: level})
				if err != nil {
					t.Fatal(err)
				}
				if p := signaltest.SealedProblem(data); p != "" {
					t.Fatalf("fields %v plain %v: Signal would refuse %s: %s", listed, plain, p, data)
				}
				var b map[string]json.RawMessage
				if err := json.Unmarshal(data, &b); err != nil {
					t.Fatal(err)
				}
				for k := range b {
					if !envelopeKeys[k] {
						t.Fatalf("key %q outside sealed: %s", k, data)
					}
				}
				outside := append([]byte(nil), data...)
				if i := bytes.Index(outside, []byte(`"sealed":`)); i >= 0 {
					outside = outside[:i]
				}
				for _, f := range listed {
					found := bytes.Contains(outside, []byte(`"`+f+`":"`+canary[f]+`"`))
					if f == rules.FieldActor || f == rules.FieldReferences {
						found = bytes.Contains(outside, []byte(canary[f]))
					}
					if found != isPlain[f] {
						t.Fatalf("fields %v plain %v: %s outside sealed=%v: %s", listed, plain, f, found, data)
					}
				}
				for _, v := range []string{"CANARY-BODY", "CANARY-STATE", "Reply received", "jira:CANARY-1"} {
					if bytes.Contains(outside, []byte(v)) {
						t.Fatalf("%s outside sealed: %s", v, data)
					}
				}
				var meta map[string]json.RawMessage
				_ = json.Unmarshal(b["metadata"], &meta)
				for k := range meta {
					if k != "fields" && k != "native" {
						t.Fatalf("metadata.%s outside sealed", k)
					}
				}
			}
		}
	}
}

// metadata.native is never anything with @, for any source.
func TestNativeNeverHoldsAnAddress(t *testing.T) {
	for _, native := range []string{"18c9", "C123:1794132000.000100", "1AbC-file", "<CAF=x@mail.gmail.com>", "x@y"} {
		it := mailItem()
		it.Native = native
		_, data, err := Project(Input{Item: it, Rule: sealedRule("owner", nil, "body"), Person: person, Level: seal.Owner})
		if err != nil {
			t.Fatal(err)
		}
		var b struct {
			Metadata struct {
				Native string `json:"native"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(data, &b)
		if strings.Contains(b.Metadata.Native, "@") || (strings.Contains(native, "@") && b.Metadata.Native != "") {
			t.Fatalf("native %q left as %q", native, b.Metadata.Native)
		}
		if p := signaltest.SealedProblem(data); p != "" {
			t.Fatalf("%q: Signal would refuse %s", native, p)
		}
	}
}

func TestSealedRefusals(t *testing.T) {
	it := mailItem()
	it.Subject = "slack:C1:1794132000.000100"
	if _, _, err := Project(Input{Item: it, Rule: sealedRule("owner", nil, "body"), Person: person, Level: seal.Owner}); !errors.Is(err, ErrSealShared) {
		t.Fatalf("shared subject: %v", err)
	}
	it = mailItem()
	it.Key = "gmail:<x@y>:1"
	if _, _, err := Project(Input{Item: it, Rule: sealedRule("owner", nil, "body"), Person: person, Level: seal.Owner}); err == nil {
		t.Fatal("a key with @ was sealed")
	}
	// DEC-32: a plain projection of a personal body is refused.
	if _, _, err := Project(Input{Item: mailItem(), Rule: rule("subject", "body"), Person: person}); !errors.Is(err, ErrBodyNeedsSeal) {
		t.Fatalf("plain personal body: %v", err)
	}
}

func TestSealedLongBodyIsCut(t *testing.T) {
	it := mailItem()
	it.Fields["body"] = strings.Repeat("é", 70*1024)
	_, data, err := Project(Input{Item: it, Rule: sealedRule("owner", nil, "body"), Person: person, Level: seal.Owner})
	if err != nil {
		t.Fatal(err)
	}
	if p := signaltest.SealedProblem(data); p != "" {
		t.Fatalf("Signal would refuse %s", p)
	}
	var b Body
	_ = json.Unmarshal(data, &b)
	if b.Sealed.Metadata["truncated"] != true || len(b.Sealed.Body) == 0 || !strings.HasPrefix(it.Fields["body"], b.Sealed.Body) {
		t.Fatalf("not cut: %d bytes, %v", len(b.Sealed.Body), b.Sealed.Metadata)
	}
}

func TestNarrowNeverLowers(t *testing.T) {
	d := Decide(mailItem(), sealedRule("owner+agents", []string{"subject"}, "subject", "body"), false, seal.Owner)
	if d.Seal != seal.Owner || len(d.Plain) != 1 || d.Item.Native != "18c9" {
		t.Fatalf("%+v", d)
	}
	cur := sealedRule("", nil, "subject", "body")
	r := d.Narrow(cur)
	if r.Seal != "owner" || len(r.Plain) != 0 {
		t.Fatalf("plain rule now: %+v", r)
	}
	cur = sealedRule("owner+agents", []string{"subject"}, "subject", "body")
	r = d.Narrow(cur)
	if r.Seal != "owner" || len(r.Plain) != 1 {
		t.Fatalf("lower rule now: %+v", r)
	}
	plain := Decide(mailItem(), rule("subject"), false, seal.None)
	r = plain.Narrow(sealedRule("owner", []string{"subject", "from"}, "subject", "from"))
	got := append([]string(nil), r.Plain...)
	sort.Strings(got)
	if r.Seal != "owner" || strings.Join(got, ",") != "subject" {
		t.Fatalf("sealed now: %+v", r)
	}
}
