package github

import (
	"testing"
	"time"
)

// Checks starting or running are not events; only a conclusion is. The snapshot still says pending.
func TestChecksOnlyConclusions(t *testing.T) {
	head := &struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}{Ref: "feature", SHA: "def456"}
	pr := pull{Number: 9, Title: "T", State: "open", User: &ghUser{Login: "winton"}, Head: head, owner: "acme", name: "app"}
	at := "2026-11-07T00:00:00Z"
	obs := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		suites []checkSuite
		key    string
	}{
		{"queued", []checkSuite{{Status: "queued", UpdatedAt: at}}, ""},
		{"running beside a pass", []checkSuite{{Status: "in_progress", UpdatedAt: at}, {Status: "completed", Conclusion: "success", UpdatedAt: at}}, ""},
		{"no conclusion yet", []checkSuite{{Status: "completed", Conclusion: "", UpdatedAt: at}}, ""},
		{"none", nil, ""},
		{"passing", []checkSuite{{Status: "completed", Conclusion: "success", UpdatedAt: at}}, "github:acme/app#9:checks:def456:pass"},
		{"failing", []checkSuite{{Status: "in_progress", UpdatedAt: at}, {Status: "completed", Conclusion: "failure", UpdatedAt: at}}, "github:acme/app#9:checks:def456:fail"},
	} {
		it, ok := checksItem(buildInput{PR: pr, Checks: tc.suites, Fetched: true, Login: "winton"}, obs)
		if tc.key == "" {
			if ok {
				t.Errorf("%s: emitted %s", tc.name, it.Key)
			}
			continue
		}
		if !ok || it.Key != tc.key {
			t.Errorf("%s: %v %s", tc.name, ok, it.Key)
		}
	}
	snap := snapshot(pr, nil, []checkSuite{{Status: "in_progress", UpdatedAt: at}}, true)
	if snap["checks"] != "pending" {
		t.Fatalf("snapshot %v", snap)
	}
}
