package actionexec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRunSettingsRefusePathsAndModelsOutsideTheSchema(t *testing.T) {
	dir := t.TempDir()
	if got, err := LoadRunSettings(dir); got != nil || err != nil {
		t.Fatalf("absent settings = %v, %v", got, err)
	}
	s := `{"opencode":"/program/opencode","models":["fixture/model"],"checkouts":[{"name":"test","path":"/checkouts/test"}]}`
	path := filepath.Join(dir, runSettingsName)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadRunSettings(dir); err != nil || got.Checkouts[0].Name != "test" {
		t.Fatalf("valid settings = %v, %v", got, err)
	}
	for _, change := range [][2]string{
		{`"/program/opencode"`, `"opencode"`}, {`"/checkouts/test"`, `"test"`},
		{`"fixture/model"`, `"--fixture/model"`}, {`"test"`, `"../test"`},
		{`"name":"test"`, `"name":"test","name":"another"`}, {`"models":`, `"unknown":`},
	} {
		if err := os.WriteFile(path, []byte(strings.Replace(s, change[0], change[1], 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := LoadRunSettings(dir); got != nil || !errors.Is(err, errRunSettings) {
			t.Errorf("accepted invalid %s: %v", change[0], err)
		}
	}
}

func TestCheckoutBranchReadsWorktreeAndDetachedHead(t *testing.T) {
	dir := t.TempDir()
	if got := checkoutBranch(dir); got != "none" {
		t.Fatal(got)
	}
	if err := os.Mkdir(filepath.Join(dir, "worktree"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: worktree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range [][2]string{{"ref: refs/heads/review/part-4\n", "review/part-4"}, {strings.Repeat("a", 40), "detached:aaaaaaa"}} {
		if err := os.WriteFile(filepath.Join(dir, "worktree", "HEAD"), []byte(tc[0]), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := checkoutBranch(dir); got != tc[1] {
			t.Fatalf("branch = %q, want %q", got, tc[1])
		}
	}
}

func TestRunReportFollowsLastAssistantMessageAndSkipsLongLines(t *testing.T) {
	body := `{"type":"text","part":{"messageID":"m1","id":"p1","text":"Not the report."}}
{"type":"text","part":{"messageID":"m2","id":"p2","text":"Old text"}}
{"type":"text","part":{"messageID":"m2","id":"p2","text":"The answer"}}
{"type":"text","part":{"messageID":"m2","id":"prt_m2_text-0","text":" continued."}}
{"type":"tool_use","part":{"text":"Not an assistant message."}}
` + strings.Repeat("x", maxReportLine+1) + `
{"type":"text","part":{"messageID":"m2","id":"p3","text":"Second paragraph."}}
{"type":"error","error":{"name":"APIError","data":{"message":"The last error."}}}
`
	r := parseRunReport(strings.NewReader(body))
	if r.text != "The answer continued.\n\nSecond paragraph." || r.problem != "The last error." {
		t.Fatalf("report: %+v", r)
	}
	text := "unwanted start" + strings.Repeat("👍", maxReportText) + "wanted end"
	got := capReport(text)
	if utf8.RuneCountInString(got) != maxReportText || !strings.HasPrefix(got, "[Beacon kept") || !strings.HasSuffix(got, "wanted end") {
		t.Fatal("long report did not preserve its end within the scalar limit")
	}
}
