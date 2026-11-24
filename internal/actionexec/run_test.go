package actionexec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/supervisor"
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
	pin, err := supervisor.PinDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	if got := checkoutBranch(pin); got != "none" {
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
		if got := checkoutBranch(pin); got != tc[1] {
			t.Fatalf("branch = %q, want %q", got, tc[1])
		}
	}
}

func TestCheckoutFactsStayWithTheHeldDirectory(t *testing.T) {
	dir := t.TempDir()
	checkout := filepath.Join(dir, "checkout")
	moved := filepath.Join(dir, "renamed")
	setBranch := func(name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte("ref: refs/heads/"+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setBranch("approved")
	pin, err := supervisor.PinDirectory(checkout)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	if err := os.Rename(checkout, moved); err != nil {
		t.Fatal(err)
	}
	setBranch("replacement")
	if pin.MatchesPath() || checkoutBranch(pin) != "approved" {
		t.Fatal("the held directory followed the replacement instead of the approved branch")
	}
}

func TestSuccessfulExitCannotReportAnOversizePrefix(t *testing.T) {
	for _, name := range []string{"output", "errors"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cmd := supervisor.Command{Stdout: filepath.Join(dir, "output"), Stderr: filepath.Join(dir, "errors")}
			if err := os.WriteFile(cmd.Stdout, []byte(`{"type":"text","part":{"messageID":"m1","id":"p1","text":"Not the final report."}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(f.Truncate(runOutputLimit+1), f.Close()); err != nil {
				t.Fatal(err)
			}
			event := runOutcome(cmd, supervisor.LeaderExit{Code: 0}) // no periodic output check ran
			if event.Kind != approval.RunnerFailed || event.Error != "output_too_large" || event.Text != "" {
				t.Fatal("the first part of oversized output was reported as a successful result")
			}
		})
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
