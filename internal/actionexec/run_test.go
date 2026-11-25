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
	if got, err := pin.Branch(); err != nil || got != "none" {
		t.Fatalf("branch = %q: %v", got, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "worktree"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: worktree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range [][2]string{{"ref: refs/heads/review/part-4\n", "review/part-4"}, {strings.Repeat("a", 40), "detached:" + strings.Repeat("a", 40)}} {
		if err := os.WriteFile(filepath.Join(dir, "worktree", "HEAD"), []byte(tc[0]), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := pin.Branch(); err != nil || got != tc[1] {
			t.Fatalf("branch = %q, want %q: %v", got, tc[1], err)
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
	if branch, err := pin.Branch(); err != nil || pin.MatchesPath() || branch != "approved" {
		t.Fatal("the held directory followed the replacement instead of the approved branch")
	}
}

func TestSuccessfulExitCannotReportAnOversizePrefix(t *testing.T) {
	for _, name := range []string{"output", "errors"} {
		t.Run(name, func(t *testing.T) {
			files := fixtureOutputs(t)
			if _, err := files.Stdout.WriteString(`{"type":"text","part":{"messageID":"m1","id":"p1","text":"Not the final report."}}` + "\n"); err != nil {
				t.Fatal(err)
			}
			f := files.Stdout
			if name == "errors" {
				f = files.Stderr
			}
			if err := f.Truncate(runOutputLimit + 1); err != nil {
				t.Fatal(err)
			}
			event := runOutcome(files, supervisor.LeaderExit{Code: 0}) // no periodic output check ran
			if event.Kind != approval.RunnerFailed || event.Error != "output_too_large" || event.Text != "" {
				t.Fatal("the first part of oversized output was reported as a successful result")
			}
		})
	}
}

func fixtureOutputs(t *testing.T) *supervisor.OutputFiles {
	t.Helper()
	dir := t.TempDir()
	open := func(name string) *os.File {
		f, err := approval.OpenPrivate(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE|os.O_EXCL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	return &supervisor.OutputFiles{Stdout: open("output"), Stderr: open("errors")}
}

func TestRunOutcomeMeasuresAndReadsThePinnedOutput(t *testing.T) {
	f := fixtureOutputs(t)
	path := f.Stdout.Name()
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	written, err := f.Stdout.WriteString(`{"type":"text","part":{"messageID":"m1","id":"p1","text":"The pinned report."}}` + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := runOutcome(f, supervisor.LeaderExit{}); got.Kind != approval.RunnerFinished || got.Text != "The pinned report." {
		t.Fatal("report followed the path instead of its pinned descriptor")
	}
	if at, err := f.Stdout.Seek(0, 1); err != nil || at != int64(written) {
		t.Fatal("a report read changed the writer's offset")
	}
	if err := f.Stdout.Truncate(runOutputLimit + 1); err != nil {
		t.Fatal(err)
	}
	if got := runOutcome(f, supervisor.LeaderExit{}); got.Error != "output_too_large" {
		t.Fatal("output size followed the replacement path")
	}
	if err := f.Stdout.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runOutcome(f, supervisor.LeaderExit{}); got.Error != "output_unavailable" {
		t.Fatal("an unreadable output descriptor was reported as success")
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
