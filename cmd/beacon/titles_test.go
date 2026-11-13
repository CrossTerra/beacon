package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/slack/slacktest"
	"github.com/CrossTerra/beacon/internal/titled"
)

// openTitle is a sent body's open.title and event title.
func openTitle(t *testing.T, body []byte) (thread, event string) {
	t.Helper()
	var b struct {
		Title string `json:"title"`
		Open  struct {
			Title string `json:"title"`
		} `json:"open"`
		Metadata struct {
			Fields map[string]string `json:"fields"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Metadata.Fields["title"]; ok {
		t.Fatalf("the title left as a field too: %s", body)
	}
	return b.Open.Title, b.Title
}

// The starter rules list title: each thread is named for who and where, and each event keeps its short title.
func TestSlackThreadTitles(t *testing.T) {
	_, sig, dir, run := slackSetup(t, true)
	run("run", "--first", "--once")
	run("accept-first")
	sig.Restrict("slack:dm:*")
	report := run("run", "--once")
	if strings.Contains(report, "thread titles stay generic") {
		t.Fatalf("report:\n%s", report)
	}
	for key, want := range map[string][2]string{
		"slack:D0DANA:1794132000.000100":   {"DM with Dana Scully · Sun 8 Nov", "Direct message"},
		"slack:D0DANA:1794133200.000300":   {"DM with Dana Scully · Sun 8 Nov", "Sent"},
		"slack:G0GROUP:1794137500.001400":  {"Group DM: Carol Danvers, Dana Scully · Sun 8 Nov", "Direct message"},
		"slack:C0ENG:1794133800.000400":    {"#eng: Bob Ross mentioned you", "Mentioned"},
		"slack:C0ENG:1794130000.000600":    {"#eng thread", "Replied"},
		"slack:C0REVIEW:1794136200.001100": {"You in #review", "Sent"},
		"slack:C0SECRET:1794134000.000450": {"#secret: Hana Reyes mentioned you", "Mentioned"},
	} {
		e, ok := sig.Event(key)
		if !ok {
			t.Fatalf("%s not sent", key)
		}
		thread, event := openTitle(t, e.Body)
		if thread != want[0] || event != want[1] {
			t.Errorf("%s: thread %q, event %q", key, thread, event)
		}
		for _, w := range []string{"deploy", "budget", "offsite", "alerts", "Merged", "salary", "Lunch", "pager"} {
			if strings.Contains(string(e.Body), w) {
				t.Fatalf("%s: message text left: %s", key, e.Body)
			}
		}
	}
	if th, _ := sig.ThreadFor("slack:dm:winton:D0DANA:2026-11-08"); th.Title != "DM with Dana Scully · Sun 8 Nov" {
		t.Fatalf("thread %+v", th)
	}
	// Only a send pass writes the name cache; no name is in the log.
	if _, err := os.Stat(filepath.Join(dir, "state", "slack-names-slack.json")); err != nil {
		t.Fatalf("no name cache: %v", err)
	}
	// Threads opened with a title are not retitled.
	for _, e := range sig.Events() {
		if strings.Contains(string(e.Body), "thread.titled") {
			t.Fatalf("retitled a thread opened with its title: %s", e.Body)
		}
	}
}

// Without users:read and im:read a pass still collects and sends, with today's titles, and says so once.
func TestSlackTitlesWithoutScopes(t *testing.T) {
	srv, sig, dir, run := slackSetup(t, true)
	srv.Scopes = slacktest.PassScopes
	first := run("run", "--first", "--once")
	if n := strings.Count(first, "thread titles stay generic and items carry no names: the token lacks users:read, im:read"); n != 1 {
		t.Fatalf("warned %d times:\n%s", n, first)
	}
	run("accept-first")
	sig.Restrict("slack:dm:*")
	run("run", "--once")
	e, ok := sig.Event("slack:D0DANA:1794132000.000100")
	if !ok {
		t.Fatal("not sent")
	}
	if thread, event := openTitle(t, e.Body); thread != "" || event != "Direct message" {
		t.Fatalf("thread %q, event %q", thread, event)
	}
	if th, _ := sig.ThreadFor("slack:dm:winton:D0DANA:2026-11-08"); th.Title != "Direct message" {
		t.Fatalf("thread %+v", th)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "state", "outbox.jsonl"))
	if strings.Contains(string(log), "Dana Scully") {
		t.Fatal("a name was recorded without the scopes")
	}
}

// Threads Beacon opened before the rules listed title are renamed by a later send pass: only those Beacon's own
// event opened, not one someone renamed since, not one another writer opened, and each once.
func TestSlackRetitlesThreadsBeaconOpened(t *testing.T) {
	srv, sig, dir, run, advance := slackSetupClock(t, true)
	rulesPath := filepath.Join(dir, "rules.yaml")
	starter, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatal(err)
	}
	// Before: the same rules without title.
	must(t, os.WriteFile(rulesPath, []byte(strings.ReplaceAll(string(starter), "fields: [title, ", "fields: [")), 0o600))
	// Someone else's writer already has a thread for Frank's @here.
	sig.OpenAs("slack:C0REVIEW:1794135700.001050", "Deploy freeze")
	run("run", "--first", "--once")
	run("accept-first")
	sig.Restrict("slack:dm:*")
	run("run", "--once")
	for subject, want := range map[string]string{
		"slack:dm:winton:D0DANA:2026-11-08": "Direct message",
		"slack:C0ENG:1794133800.000400":     "Mentioned",
		"slack:C0REVIEW:1794136200.001100":  "Sent",
		"slack:C0REVIEW:1794135800.001060":  "Mentioned",
	} {
		if th, ok := sig.ThreadFor(subject); !ok || th.Title != want {
			t.Fatalf("%s: %+v", subject, th)
		}
	}
	// A person renames one thread.
	sig.Rename("slack:C0REVIEW:1794135800.001060", "Oncall noise")
	reads := len(sig.Requests())

	// A pass with the old rules retitles nothing and reads no thread.
	advance(16 * time.Minute)
	run("run", "--once")
	for _, r := range sig.Requests()[reads:] {
		if strings.HasPrefix(r.Path, "/api/events/") || strings.HasPrefix(r.Path, "/api/threads/") {
			t.Fatalf("read %s though no rule lists title", r.Path)
		}
	}

	// After: the rules list title.
	must(t, os.WriteFile(rulesPath, starter, 0o600))
	advance(16 * time.Minute)
	report := run("run", "--once")
	for subject, want := range map[string]string{
		"slack:dm:winton:D0DANA:2026-11-08":          "DM with Dana Scully · Sun 8 Nov",
		"slack:dm:winton:G0GROUP:2026-11-08":         "Group DM: Carol Danvers, Dana Scully · Sun 8 Nov",
		"slack:C0ENG:1794133800.000400":              "#eng: Bob Ross mentioned you",
		"slack:C0ENG:1794120000.000500":              "#eng thread",
		"slack:C0REVIEW:1794136200.001100":           "You in #review",
		"slack:dm:winton:C0SECRET:1794134000.000450": "#secret: Hana Reyes mentioned you",
		// Renamed by a person, and opened by someone else: left alone.
		"slack:C0REVIEW:1794135800.001060": "Oncall noise",
		"slack:C0REVIEW:1794135700.001050": "Deploy freeze",
	} {
		if th, ok := sig.ThreadFor(subject); !ok || th.Title != want {
			t.Errorf("%s: %+v, want %q", subject, th, want)
		}
	}
	if !strings.Contains(report, "Retitled: ") {
		t.Fatalf("report:\n%s", report)
	}
	var changes int
	for _, e := range sig.Events() {
		if !strings.Contains(string(e.Body), `"type":"thread.titled"`) {
			continue
		}
		changes++
		var b map[string]any
		must(t, json.Unmarshal(e.Body, &b))
		meta, _ := b["metadata"].(map[string]any)
		if !strings.HasPrefix(b["subject"].(string), "signal:thread:") || len(meta) != 1 || b["open"] != nil ||
			!strings.HasPrefix(e.Key, "slack:retitle:") {
			t.Fatalf("thread change %s", e.Body)
		}
	}
	// Every thread change went through the outbox first.
	ob, err := outbox.Read(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	inOutbox := 0
	for _, e := range ob.Entries() {
		if strings.Contains(e.Key, ":retitle:") {
			inOutbox++
			if e.State != outbox.Sent {
				t.Errorf("%s: %s", e.Key, e.State)
			}
		}
	}
	if changes == 0 || inOutbox != changes {
		t.Fatalf("%d thread changes, %d in the outbox", changes, inOutbox)
	}
	rec, err := titled.Read(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Checked("slack:C0REVIEW:1794135800.001060") || !rec.Checked("slack:C0REVIEW:1794135700.001050") {
		t.Fatal("the threads left alone were not recorded")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "state", titled.FileName))
	if strings.Contains(string(data), "Oncall") || strings.Contains(string(data), "Dana") {
		t.Fatalf("the record holds a title: %s", data)
	}

	// Each thread once: the next pass writes no thread change and reads no thread it already checked.
	reads = len(sig.Requests())
	advance(16 * time.Minute)
	srv.Scopes = slacktest.AllScopes
	run("run", "--once")
	for _, r := range sig.Requests()[reads:] {
		if strings.Contains(string(r.Body), "thread.titled") || strings.HasPrefix(r.Path, "/api/threads/") {
			t.Fatalf("a second pass retitled again: %s %s", r.Path, r.Body)
		}
	}
}

// Without the name scopes, threads wait: nothing is read from Signal and nothing is recorded, so they are retitled
// once the token has the scopes.
func TestSlackRetitleWaitsForScopes(t *testing.T) {
	srv, sig, dir, run, advance := slackSetupClock(t, true)
	srv.Scopes = slacktest.PassScopes
	run("run", "--first", "--once")
	run("accept-first")
	sig.Restrict("slack:dm:*")
	run("run", "--once")
	advance(16 * time.Minute)
	run("run", "--once")
	if th, _ := sig.ThreadFor("slack:C0ENG:1794133800.000400"); th.Title != "Mentioned" {
		t.Fatalf("thread %+v", th)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", titled.FileName)); err == nil {
		t.Fatal("threads were recorded as checked without the scopes")
	}
	srv.Scopes = slacktest.AllScopes
	advance(16 * time.Minute)
	run("run", "--once")
	if th, _ := sig.ThreadFor("slack:C0ENG:1794133800.000400"); th.Title != "#eng: Bob Ross mentioned you" {
		t.Fatalf("thread %+v", th)
	}
}
