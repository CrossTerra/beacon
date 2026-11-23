package approval

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var t0 = time.Unix(1_800_000_000, 0).UTC()

func scratch(t *testing.T) string { return filepath.Join(t.TempDir(), "state") }

func open(t *testing.T, dir string, at time.Time) *Journal {
	t.Helper()
	j, err := OpenJournal(dir, at, "")
	must(t, err)
	t.Cleanup(func() { j.Close() })
	return j
}

func mergeRequest() Request {
	return Request{Type: Merge, Agent: "bosun", Session: "ses_1", Reason: "Because.", Params: Object{
		"repo": "CrossTerra/signal", "number": int64(140), "base": "main", "head": strings.Repeat("a", 40), "method": "squash",
	}}
}

func runRequest() Request {
	return Request{Type: Run, Agent: "bosun", Reason: "Because.", Params: Object{
		"checkout": "signal", "prompt": "Read it.", "model": "anthropic/claude-opus-5",
	}}
}

func put(t *testing.T, j *Journal, r Request) Proposal {
	t.Helper()
	p, err := j.Add(r, PeerLabel{}, t0)
	must(t, err)
	return p
}

func freeze(t *testing.T, p Proposal, model string) Frozen {
	t.Helper()
	b := Binding{"https://api.github.com", "github:user:123", Object{"repository_id": int64(456), "pr": p.Params["number"], "base": "main"}}
	if p.Type == Run {
		b = Binding{"", "linux:user:winton", Object{"checkout_id": "x", "branch": "main"}}
	}
	f, err := Freeze(p, b, model)
	must(t, err)
	return f
}

func status(t *testing.T, j *Journal, id string, at time.Time, want string) View {
	t.Helper()
	v, _, err := j.Get(id, at)
	must(t, err)
	if v.Status != want {
		t.Fatalf("proposal is %s, want %s", v.Status, want)
	}
	return v
}

func TestRecoveryNeverRestartsAnApproval(t *testing.T) {
	dir := scratch(t)
	j := open(t, dir, t0)
	a, b, c, d := put(t, j, mergeRequest()), put(t, j, mergeRequest()), put(t, j, mergeRequest()), put(t, j, mergeRequest())
	for _, p := range []Proposal{a, b, c} {
		must(t, j.Approve(freeze(t, p, ""), t0))
	}
	for _, p := range []Proposal{b, c} {
		f, err := j.Start(p.ID, t0)
		must(t, err)
		if f.Hash != freeze(t, p, "").Hash {
			t.Fatal("start returned another action")
		}
	}
	result := Result{ConfirmedBy: "reply", Native: "github:merge:" + strings.Repeat("b", 40)}
	must(t, j.SetResult(c.ID, result, t0))
	must(t, j.Close()) // no Shutdown: simulate process death after the last durable frame
	at := t0.Add(time.Minute)
	j = open(t, dir, at)
	status(t, j, a.ID, at, "expired")
	status(t, j, b.ID, at, "outcome_unknown")
	status(t, j, c.ID, at, "executed")
	status(t, j, d.ID, at, "pending")
	for _, p := range []Proposal{a, b, c, d} {
		if _, err := j.Start(p.ID, at); !errors.Is(err, ErrTransition) {
			t.Fatalf("restarted a pre-crash proposal: %v", err)
		}
	}
	if err := j.SetResult(b.ID, result, at); !errors.Is(err, ErrTransition) {
		t.Fatalf("reply settled an unknown: %v", err)
	}
	result.ConfirmedBy = "read_back"
	must(t, j.SetResult(b.ID, result, at))
	if err := j.SetResult(b.ID, result, at); !errors.Is(err, ErrTransition) {
		t.Fatalf("settled twice: %v", err)
	}
	before := j.DeliveryStatus()
	must(t, j.Close())
	j = open(t, dir, at.Add(time.Second))
	if j.DeliveryStatus() != before {
		t.Fatal("second recovery appended more records")
	}
	status(t, j, b.ID, at, "executed")
}

func TestFrozenBindingAndShutdown(t *testing.T) {
	j := open(t, scratch(t), t0)
	m, r, p := put(t, j, mergeRequest()), put(t, j, runRequest()), put(t, j, mergeRequest())
	f := freeze(t, m, "")
	if _, err := Freeze(m, f.Binding, "openai/gpt-6"); err != Invalid("/params/model") {
		t.Fatal("non-run model edit was allowed")
	}
	f.Params["head"] = strings.Repeat("b", 40)
	if err := j.Approve(f, t0); err != ErrStale {
		t.Fatalf("tampered snapshot: %v", err)
	}
	old := freeze(t, m, "")
	must(t, j.Decline(m.ID, "No.", t0))
	if err := j.Approve(old, t0); err != ErrTransition {
		t.Fatalf("approved a stale snapshot: %v", err)
	}
	edited := freeze(t, r, "openai/gpt-6")
	if r.ParamsHash == edited.ParamsHash || edited.OriginalModel != "anthropic/claude-opus-5" || !ValidID(r.RunnerID, "run") {
		t.Fatal("model edit lost the original model or digest")
	}
	var w sync.WaitGroup
	results := make(chan error, 8)
	for range cap(results) {
		w.Add(1)
		go func() { defer w.Done(); results <- j.Approve(edited, t0) }()
	}
	w.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if err != ErrTransition {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("snapshot was consumed %d times", success)
	}
	must(t, j.Shutdown(t0))
	status(t, j, r.ID, t0, "expired")
	if _, err := j.Start(r.ID, t0); err != ErrStopping {
		t.Fatal("shutdown did not disable start")
	}
	if err := j.Approve(freeze(t, p, ""), t0); err != ErrStopping {
		t.Fatal("shutdown did not disable approval")
	}
}

func TestExpiryAndConcurrentSweep(t *testing.T) {
	j := open(t, scratch(t), t0)
	p := put(t, j, mergeRequest())
	must(t, j.Approve(freeze(t, p, ""), t0))
	if _, err := j.Start(p.ID, t0.Add(startWindow+time.Millisecond)); err != ErrTransition {
		t.Fatal("late start was allowed")
	}
	status(t, j, p.ID, t0.Add(startWindow+time.Second), "expired")
	const n = 10
	for range n {
		put(t, j, mergeRequest())
	}
	pending, err := j.Pending(t0.Add(Lifetime - time.Millisecond))
	must(t, err)
	if len(pending) != n {
		t.Fatal("proposals expired early")
	}
	var w sync.WaitGroup
	for range n {
		w.Add(1)
		go func() {
			defer w.Done()
			p, err := j.Pending(t0.Add(Lifetime))
			if err != nil || len(p) != 0 {
				t.Errorf("late pending: %d, %v", len(p), err)
			}
		}()
	}
	w.Wait()
	if s := j.DeliveryStatus(); s.Waiting != 2*n+3 {
		t.Fatalf("duplicate or missing expiries: %+v", s)
	}
}

func TestAppendOnlyAndTornTail(t *testing.T) {
	dir := scratch(t)
	j := open(t, dir, t0)
	p := put(t, j, mergeRequest())
	path := filepath.Join(dir, journalName)
	prefix, err := os.ReadFile(path)
	must(t, err)
	_, err = j.Withdraw(p.ID, "", t0)
	must(t, err)
	all, err := os.ReadFile(path)
	must(t, err)
	must(t, j.Close())
	if !bytes.HasPrefix(all, prefix) || len(all) <= len(prefix) {
		t.Fatal("withdraw changed prior bytes instead of appending")
	}
	for _, length := range []int{3, headerSize, len(all) - len(prefix) - 1} {
		dir := scratch(t)
		must(t, PrivateDir(dir))
		path := filepath.Join(dir, journalName)
		must(t, os.WriteFile(path, all[:len(prefix)+length], 0o600))
		reopened := open(t, dir, t0)
		status(t, reopened, p.ID, t0, "pending")
		clean, err := os.ReadFile(path)
		must(t, err)
		if !bytes.Equal(clean, prefix) {
			t.Fatalf("torn frame repair at length %d changed committed prefix", length)
		}
		_, err = reopened.Withdraw(p.ID, "", t0)
		must(t, err)
	}
	all[len(all)-1] ^= 1
	must(t, os.WriteFile(path, all, 0o600))
	if _, err := OpenJournal(dir, t0, ""); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("accepted a complete corrupt frame: %v", err)
	}
	after, err := os.ReadFile(path)
	must(t, err)
	if !bytes.Equal(after, all) {
		t.Fatal("a checksum error truncated the file")
	}
	must(t, os.WriteFile(path, nil, 0o600))
	if _, err := OpenJournal(dir, t0, ""); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("empty existing journal reset its id: %v", err)
	}
}

func TestWriteFailureIsNeverAuthority(t *testing.T) {
	dir := scratch(t)
	fail := false
	j, err := openJournal(dir, t0, "", journalHooks{newID, nonce, func(f *os.File) error {
		if fail {
			return os.ErrInvalid
		}
		return syncJournal(f)
	}})
	must(t, err)
	t.Cleanup(func() { j.Close() })
	p := put(t, j, mergeRequest())
	must(t, j.Approve(freeze(t, p, ""), t0))
	fail = true
	if _, err := j.Start(p.ID, t0); err != ErrJournalIO {
		t.Fatalf("start allowed after failed sync: %v", err)
	}
	if _, err := j.Start(p.ID, t0); err != ErrJournalIO {
		t.Fatalf("retry allowed after failed sync: %v", err)
	}
	must(t, j.Close())
	reopened := open(t, dir, t0)
	// The complete start reached the durable prefix by Close, but no handler was ever given authority.
	status(t, reopened, p.ID, t0, "outcome_unknown")
}

func TestPrivateFilesAndInstanceLock(t *testing.T) {
	dir := scratch(t)
	j := open(t, dir, t0)
	for _, name := range []string{journalName, "instance.lock"} {
		_, err := CheckPrivate(filepath.Join(dir, name), 0o600)
		must(t, err)
	}
	if _, err := OpenJournal(dir, t0, ""); !errors.Is(err, ErrLocked) {
		t.Fatalf("two writers: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	must(t, os.Symlink(dir, alias))
	if _, err := OpenJournal(alias, t0, ""); !errors.Is(err, ErrPrivate) {
		t.Fatalf("accepted symlink state directory: %v", err)
	}
	must(t, j.Close())
	path := filepath.Join(dir, journalName)
	must(t, os.Chmod(path, 0o644))
	if _, err := OpenJournal(dir, t0, ""); !errors.Is(err, ErrPrivate) {
		t.Fatalf("accepted nonprivate journal: %v", err)
	}
	must(t, os.Rename(path, path+".old"))
	must(t, os.Symlink(path+".old", path))
	if _, err := OpenJournal(dir, t0, ""); err == nil {
		t.Fatal("accepted symlink journal")
	}
	must(t, os.Remove(path))
	must(t, os.Link(path+".old", path))
	if _, err := OpenJournal(dir, t0, ""); !errors.Is(err, ErrPrivate) {
		t.Fatalf("accepted hardlinked journal: %v", err)
	}
}

func TestReplayDoesNotRevalidateHistoricalParameters(t *testing.T) {
	dir := scratch(t)
	j := open(t, dir, t0)
	r := runRequest()
	r.Params["checkout"] = "Old Name"
	p := Proposal{Request: r, ID: "apr_01m7wyd4m0f858m2ga1850m2ga", Status: "pending", Version: 1,
		Salt: nonce(), ProposedAt: t0, ExpiresAt: t0.Add(Lifetime)}
	var err error
	p.ParamsHash, err = paramsDigest(r.Type, r.Params, p.Salt)
	must(t, err)
	_, err = j.add(p)
	must(t, err)
	must(t, j.Close())
	j = open(t, dir, t0)
	if got := status(t, j, p.ID, t0, "pending"); got.Params["checkout"] != "Old Name" {
		t.Fatal("lost historical parameters")
	}
	if _, err := j.Add(r, PeerLabel{}, t0); err != Invalid("/params/checkout") {
		t.Fatalf("old schema accepted at intake: %v", err)
	}
}

func TestCreateJournalCallsTheSyncHookForTheDirectory(t *testing.T) {
	id := "jrn_01m7wyd4m0f858m2ga1850m2ga"
	hook := func(reject error, saw *bool) func(*os.File) error {
		return func(f *os.File) error {
			info, err := f.Stat()
			must(t, err)
			if !info.IsDir() {
				return nil
			}
			*saw = true
			return reject
		}
	}
	var saw bool
	path := filepath.Join(t.TempDir(), journalName)
	if err := createJournal(path, id, hook(nil, &saw)); err != nil || !saw {
		t.Fatalf("directory hook called=%v: %v", saw, err)
	}
	saw = false
	if err := createJournal(filepath.Join(t.TempDir(), journalName), id, hook(unix.EINVAL, &saw)); err != nil || !saw {
		t.Fatalf("rejected full sync did not still see the directory (called=%v): %v", saw, err)
	}
	saw = false
	if err := createJournal(filepath.Join(t.TempDir(), journalName), id, hook(os.ErrInvalid, &saw)); !errors.Is(err, os.ErrInvalid) || !saw {
		t.Fatalf("directory hook called=%v, error hidden: %v", saw, err)
	}
}

func TestDirectoryFlushReportsTheCallThatReachedDisk(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Open(dir)
	must(t, err)
	defer f.Close()
	how, err := syncParent(f)
	must(t, err)
	switch runtime.GOOS {
	case "darwin":
		if how != flushFull && how != flushSync {
			t.Fatalf("directory flush reported %q", how)
		}
	default:
		if how != flushSync {
			t.Fatalf("directory flush reported %q", how)
		}
	}
	t.Logf("directory flush: %s", how)

	var full, plain int
	how, err = flushDir(f, func(int) error { full++; return nil }, func(int) error { plain++; return os.ErrInvalid })
	if err != nil || how != flushFull || full != 1 || plain != 0 {
		t.Fatalf("supported full flush: %s calls=%d/%d err=%v", how, full, plain, err)
	}
	how, err = flushDir(f, func(int) error { full++; return unix.ENOTSUP }, func(int) error { plain++; return nil })
	if err != nil || how != flushSync || plain != 1 {
		t.Fatalf("rejected full flush: %s fsyncs=%d err=%v", how, plain, err)
	}
	if _, err := flushDir(f, func(int) error { return os.ErrClosed }, func(int) error { plain++; return nil }); !errors.Is(err, os.ErrClosed) || plain != 1 {
		t.Fatalf("non-rejection fell back to fsync: %v, fsyncs=%d", err, plain)
	}
}
