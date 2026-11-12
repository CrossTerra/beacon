package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/google"
	"github.com/CrossTerra/beacon/internal/slack"
)

const sealedConfig = `signal:
  url: https://signal.example.com
person:
  handle: "@winton"
rules: rules.yaml
state: state
mode: dry-run
seal:
  owner:
    words: [salary, comp, compensation, offer, termination, "background check", incident, intrusion, legal,
            confidential, payroll, equity, vesting, {exact: PIP}]
    senders: [remotely.works, rippling.com, carta.com, counsel.example, accounting.example, payroll.example]
    labels: [Confidential]
sources:
  - {kind: fixture, name: gmail, path: gmail.json}
  - {kind: fixture, name: drive, path: drive.json}
  - {kind: fixture, name: slack, path: slack.json}
`

type sealedFixture struct {
	Subject    string            `json:"subject"`
	Key        string            `json:"key"`
	Type       string            `json:"type"`
	OccurredAt time.Time         `json:"occurred_at"`
	Actor      string            `json:"actor,omitempty"`
	Addressed  bool              `json:"addressed,omitempty"`
	Native     string            `json:"native,omitempty"`
	Fields     map[string]string `json:"fields"`
	Local      map[string]any    `json:"local,omitempty"`
}

func writeFixture(t *testing.T, dir, name string, fields []string, items []sealedFixture) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"source": name, "fields": fields, "state_fields": []string{}, "items": items})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, name+".json"), data, 0o600))
}

// The worked example (plans/phase-10.md) on fixtures lands in the levels of its table.
func TestWorkedExampleLevels(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(sealedConfig), 0o600))
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "rules-sealed.yaml"))
	must(t, err)
	rulesText := strings.NewReplacer("<counsel>", `counsel\.example`, "<accounting>", `accounting\.example`, "<payroll>", `payroll\.example`,
		"<rahul-address>", `rahul@crossterra\.co`).Replace(string(example))
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte(rulesText), 0o600))

	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	mail := func(n int, thread string, f map[string]string, local map[string]any) sealedFixture {
		fields := map[string]string{"subject": "Subject " + thread, "body": "body text", "from": "x@vendor.example", "to": "winton@crossterra.co",
			"labels": "INBOX", "bulk": "false"}
		for k, v := range f {
			fields[k] = v
		}
		fields["participants"] = fields["from"] + ", " + fields["to"]
		if fields["thread_participants"] == "" {
			fields["thread_participants"] = fields["participants"]
		}
		if local == nil {
			local = map[string]any{}
		}
		if local["text"] == nil {
			local["text"] = map[string]string{"subject": fields["subject"], "body": fields["body"]}
		}
		if local["addresses"] == nil {
			local["addresses"] = map[string][]string{"thread_participants": strings.Split(fields["thread_participants"], ", ")}
		}
		if local["labels"] == nil {
			local["labels"] = strings.Split(fields["labels"], ", ")
		}
		key := "gmail:" + thread + ":m" + string(rune('a'+n))
		return sealedFixture{Subject: "gmail:winton:thread:" + thread, Key: key, Type: "received", OccurredAt: at.Add(time.Duration(n) * time.Minute),
			Actor: "email:" + fields["from"], Native: "m" + string(rune('a'+n)), Fields: fields, Local: local}
	}
	gmail := []sealedFixture{
		mail(0, "stub", map[string]string{"from": "payroll@rippling.com", "bulk": "true", "labels": "CATEGORY_UPDATES, INBOX"}, nil),
		// A colleague's reply in that thread: the Rippling address is not first in the thread's list.
		mail(1, "stub", map[string]string{"from": "lee@crossterra.co", "thread_participants": "lee@crossterra.co, payroll@rippling.com, winton@crossterra.co"}, nil),
		mail(2, "deck", map[string]string{"from": "rahul@crossterra.co", "subject": "Board deck"}, nil),
		mail(3, "offer", map[string]string{"from": "rahul@crossterra.co", "subject": "The offer for Sam"}, nil),
		// Confidential sorts after a category label, and the list headers would make it bulk.
		mail(4, "conf", map[string]string{"from": "ops@vendor.example", "labels": "CATEGORY_UPDATES, Confidential", "bulk": "true"}, nil),
		mail(5, "vendor", map[string]string{"from": "bob@vendor.example", "subject": "Re: your order"}, nil),
		mail(6, "news", map[string]string{"from": "news@letters.example", "bulk": "true"}, nil),
	}
	writeFixture(t, dir, "gmail", google.GmailFields, gmail)
	drive := []sealedFixture{{
		Subject: "drive:winton:n1", Key: "drive:notes:n1", Type: "notes_created", OccurredAt: at.Add(10 * time.Minute), Native: "n1",
		Fields: map[string]string{"title": "1:1 - Notes by Gemini", "meeting": "1:1", "meeting_start": "2026-11-11T08:00:00Z",
			"meeting_organizer": "winton@crossterra.co", "meeting_attendees": "pat@remotely.works, winton@crossterra.co", "body": "notes text"},
		Local: map[string]any{"text": map[string]string{"title": "1:1 - Notes by Gemini", "meeting": "1:1", "body": "notes text"},
			"addresses": map[string][]string{"meeting_organizer": {"winton@crossterra.co"}, "meeting_attendees": {"pat@remotely.works", "winton@crossterra.co"}},
			"meeting":   "calendar:winton:ev1"},
	}}
	writeFixture(t, dir, "drive", google.DriveFields, drive)
	slackItems := []sealedFixture{{
		Subject: "slack:C0ENG:1794133800.000400", Key: "slack:C0ENG:1794133800.000400", Type: "mentioned", OccurredAt: at.Add(11 * time.Minute),
		Native: "C0ENG:1794133800.000400", Fields: map[string]string{"channel": "C0ENG", "channel_type": "channel", "body": "what are the salary bands?"},
		Local: map[string]any{"text": map[string]string{"body": "what are the salary bands?"}},
	}}
	writeFixture(t, dir, "slack", slack.Fields, slackItems)

	var out, errOut strings.Builder
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut, now: func() time.Time { return at.Add(time.Hour) }}
	if code := a.main(context.Background(), []string{"rules", "check", "--config", filepath.Join(dir, "config.yaml")}); code != 0 {
		t.Fatalf("rules check: %s%s", out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "0 warnings, no errors") {
		t.Fatalf("rules check: %s", out.String())
	}
	out.Reset()
	if code := a.main(context.Background(), []string{"run", "--dry-run", "--once", "--config", filepath.Join(dir, "config.yaml")}); code != 0 {
		t.Fatalf("run: %s%s", out.String(), errOut.String())
	}
	got := map[string]string{}
	lines := strings.Split(out.String(), "\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || (fields[0] != "send" && fields[0] != "hold" && fields[0] != "never") {
			continue
		}
		level := fields[0]
		for j := i + 1; j < len(lines) && strings.HasPrefix(lines[j], "       "); j++ {
			if l, ok := strings.CutPrefix(strings.TrimSpace(lines[j]), "sealed at "); ok {
				level += " " + strings.Fields(l)[0]
			}
			if strings.Contains(lines[j], "(sensitive_shared)") {
				level += " sensitive_shared"
			}
		}
		got[fields[1]] = level
	}
	want := map[string]string{
		"gmail:stub:ma":                 "send owner",            // Rippling pay stub: owner-senders
		"gmail:stub:mb":                 "send owner",            // a colleague's reply in that thread
		"gmail:deck:mc":                 "send owner+agents",     // Rahul's mail about the board deck
		"gmail:offer:md":                "send owner",            // Rahul's mail with "offer" in the subject, raised
		"gmail:conf:me":                 "send owner",            // labelled Confidential, with list headers
		"gmail:vendor:mf":               "hold owner+agents",     // a vendor's personal reply: other-mail, held
		"gmail:news:mg":                 "never",                 // a newsletter
		"drive:notes:n1":                "send owner",            // notes for a 1:1 with a remotely.works attendee, raised
		"slack:C0ENG:1794133800.000400": "hold sensitive_shared", // "salary" in a public channel
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %v", got)
	}
	// What raised it is shown locally by explain, and the sealed parts only with --bodies.
	out.Reset()
	if code := a.main(context.Background(), []string{"explain", "--config", filepath.Join(dir, "config.yaml"), "gmail:offer:md"}); code != 0 {
		t.Fatalf("explain: %s%s", out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "Level:      owner") || !strings.Contains(out.String(), `seal.owner: word "offer" in subject`) ||
		strings.Contains(out.String(), "body text") || !strings.Contains(out.String(), "[sealed:") {
		t.Fatalf("explain:\n%s", out.String())
	}
	out.Reset()
	if code := a.main(context.Background(), []string{"explain", "--bodies", "--config", filepath.Join(dir, "config.yaml"), "gmail:offer:md"}); code != 0 {
		t.Fatalf("explain: %s%s", out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "body text") {
		t.Fatalf("explain --bodies:\n%s", out.String())
	}
	if strings.Contains(errOut.String(), "salary") || strings.Contains(errOut.String(), "offer") {
		t.Fatalf("the log holds content:\n%s", errOut.String())
	}
}
