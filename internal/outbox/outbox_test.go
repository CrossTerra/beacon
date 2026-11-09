package outbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clock() func() time.Time {
	t := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Second); return t }
}

func TestAppendMarkReload(t *testing.T) {
	dir := t.TempDir()
	o, err := Open(dir, clock())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"subject":"s","key":"k1","to":["@winton"],"title":"<b>&"}`)
	if err := o.Append(Event, "k1", "s", "rule-a", body); err != nil {
		t.Fatal(err)
	}
	if err := o.Append(Event, "k1", "s", "rule-a", body); !errors.Is(err, ErrExists) {
		t.Fatalf("second append: %v", err)
	}
	if err := o.Append(Event, "k2", "s", "rule-b", []byte(`{"key":"k2","closes":[{"key":"k1"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := o.Mark("k1", Sent, "evt_1", 201, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := o.Mark("k1", Sent, "evt_1", 201, "", ""); err == nil {
		t.Fatal("marked a key twice")
	}
	o.Close()

	r, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := r.Get("k1")
	if string(e.Body) != string(body) || e.State != Sent || e.EventID != "evt_1" || e.Rule != "rule-a" || !e.Asks {
		t.Fatalf("k1 after reload: %+v", e)
	}
	if p := r.Pending(); len(p) != 1 || p[0].Key != "k2" {
		t.Fatalf("pending: %+v", p)
	}
	if got := r.OpenAsks("s"); len(got) != 0 {
		t.Fatalf("k1 is closed by pending k2, open asks: %v", got)
	}
}

func TestOpenAsksIgnoreRejected(t *testing.T) {
	o, _ := Open(t.TempDir(), clock())
	defer o.Close()
	_ = o.Append(Event, "a1", "s", "", []byte(`{"to":["@w"]}`))
	_ = o.Append(Event, "a2", "s", "", []byte(`{"to":["@w"]}`))
	_ = o.Append(Event, "a3", "other", "", []byte(`{"to":["@w"]}`))
	_ = o.Append(Event, "c", "s", "", []byte(`{"closes":[{"key":"a1"}]}`))
	_ = o.Mark("c", Rejected, "", 422, "ask_not_found", "")
	_ = o.Mark("a2", Rejected, "", 409, "idempotency_conflict", "")
	got := o.OpenAsks("s")
	if len(got) != 1 || got[0] != "a1" {
		t.Fatalf("open asks %v, want [a1]", got)
	}
}

func TestTornLastLine(t *testing.T) {
	dir := t.TempDir()
	o, _ := Open(dir, clock())
	_ = o.Append(Event, "k1", "s", "", []byte(`{"a":1}`))
	o.Close()
	path := filepath.Join(dir, FileName)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"key":"k2","state":"pending","bo`)
	f.Close()

	o, err := Open(dir, clock())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := o.Get("k2"); ok {
		t.Fatal("torn line loaded")
	}
	if err := o.Append(Event, "k2", "s", "", []byte(`{"b":2}`)); err != nil {
		t.Fatal(err)
	}
	o.Close()
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), `"bo`+"{") || strings.Count(string(data), "\n") != 2 {
		t.Fatalf("file after recovery:\n%s", data)
	}
	if _, err := Read(dir); err != nil {
		t.Fatal(err)
	}
}

func TestTamperedBody(t *testing.T) {
	dir := t.TempDir()
	o, _ := Open(dir, clock())
	_ = o.Append(Event, "k1", "s", "", []byte(`{"title":"a"}`))
	_ = o.Append(Event, "k2", "s", "", []byte(`{"title":"b"}`))
	o.Close()
	path := filepath.Join(dir, FileName)
	data, _ := os.ReadFile(path)
	_ = os.WriteFile(path, []byte(strings.Replace(string(data), `\"a\"`, `\"z\"`, 1)), 0o600)
	if _, err := Read(dir); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("tampered body: %v", err)
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	o, err := Open(dir, clock())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, clock()); !errors.Is(err, ErrLocked) {
		t.Fatalf("second open: %v", err)
	}
	o.Close()
	o2, err := Open(dir, clock())
	if err != nil {
		t.Fatal(err)
	}
	o2.Close()
}

func TestFilePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	o, _ := Open(dir, clock())
	o.Close()
	for _, p := range []string{dir, filepath.Join(dir, FileName)} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v, readable by others", p, st.Mode().Perm())
		}
	}
}

func TestExpireOnlyLeaseClaims(t *testing.T) {
	dir := t.TempDir()
	o, _ := Open(dir, clock())
	_ = o.Append(Lease, "claim:x:1", "s", "", []byte(`{"a":1}`))
	_ = o.Append(Event, "e1", "s", "", []byte(`{"b":1}`))
	if err := o.Expire("e1"); err == nil {
		t.Fatal("expired an event")
	}
	if err := o.Expire("claim:x:1"); err != nil {
		t.Fatal(err)
	}
	if p := o.PendingEvents(); len(p) != 1 || p[0].Key != "e1" {
		t.Fatalf("%+v", p)
	}
	o.Close()
	r, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := r.Get("claim:x:1"); e.State != Expired {
		t.Fatalf("%+v", e)
	}
}

func TestRequeueRefusedOnly(t *testing.T) {
	dir := t.TempDir()
	o, err := Open(dir, clock())
	if err != nil {
		t.Fatal(err)
	}
	decision := []byte(`{"item":{"source":"github","key":"k-refused"}}`)
	body := []byte(`{"subject":"s","key":"k-refused"}`)
	if err := o.AppendDecided(Event, "k-refused", "s", "rule-a", body, decision); err != nil {
		t.Fatal(err)
	}
	_ = o.Append(Event, "k-sent", "s", "", []byte(`{"key":"k-sent"}`))
	_ = o.Append(Event, "k-replayed", "s", "", []byte(`{"key":"k-replayed"}`))
	_ = o.Append(Event, "k-409", "s", "", []byte(`{"key":"k-409"}`))
	_ = o.Append(Event, "k-pending", "s", "", []byte(`{"key":"k-pending"}`))
	_ = o.Append(Lease, "claim:x:1", "s", "", []byte(`{"key":"claim"}`))
	_ = o.Mark("k-refused", Rejected, "", 400, "invalid", "bad shape")
	_ = o.Mark("k-sent", Sent, "evt_1", 201, "", "")
	_ = o.Mark("k-replayed", Replayed, "evt_2", 200, "", "")
	_ = o.Mark("k-409", Rejected, "", 409, "idempotency_conflict", "held")
	_ = o.Mark("claim:x:1", Rejected, "", 400, "invalid", "no")

	next := []byte(`{"subject":"s","key":"k-refused","state":{"fields":{"status":"open"}}}`)
	for _, key := range []string{"k-sent", "k-replayed", "k-409", "k-pending", "claim:x:1"} {
		if err := o.Requeue(key, next); !errors.Is(err, ErrNotRetryable) {
			t.Fatalf("requeue %s: %v", key, err)
		}
	}
	if err := o.Append(Event, "k-bare", "s", "", []byte(`{"key":"k-bare"}`)); err != nil {
		t.Fatal(err)
	}
	if err := o.Mark("k-bare", Rejected, "", 400, "invalid", "no item"); err != nil {
		t.Fatal(err)
	}
	if err := o.Requeue("k-bare", next); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("requeue without an item: %v", err)
	}
	if err := o.Requeue("k-refused", next); err != nil {
		t.Fatal(err)
	}
	e, _ := o.Get("k-refused")
	if e.State != Pending || string(e.Body) != string(next) || string(e.Decision) != string(decision) {
		t.Fatalf("after requeue: %+v", e)
	}
	if err := o.Requeue("k-refused", next); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("requeue of pending: %v", err)
	}
	o.Close()

	r, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, _ = r.Get("k-refused")
	if e.State != Pending || string(e.Body) != string(next) || string(e.Decision) != string(decision) {
		t.Fatalf("reloaded: %+v", e)
	}
	if e, _ := r.Get("k-sent"); e.State != Sent || string(e.Body) != `{"key":"k-sent"}` {
		t.Fatalf("sent was rewritten: %+v", e)
	}
	if e, _ := r.Get("k-409"); e.State != Rejected || e.Status != 409 {
		t.Fatalf("409 was touched: %+v", e)
	}
}

func TestAppendAttemptKeepsTheRefusal(t *testing.T) {
	dir := t.TempDir()
	o, err := Open(dir, clock())
	if err != nil {
		t.Fatal(err)
	}
	old := []byte(`{"key":"k400","state":{"status":"open","observed_at":"2026-11-07T12:00:00.000Z","version":"1"}}`)
	if err := o.Append(Event, "k400", "s", "old-rule", old); err != nil {
		t.Fatal(err)
	}
	if err := o.Mark("k400", Rejected, "", 400, "invalid", "flat"); err != nil {
		t.Fatal(err)
	}
	_ = o.Append(Event, "k409", "s", "", []byte(`{"key":"k409"}`))
	_ = o.Mark("k409", Rejected, "", 409, "idempotency_conflict", "held")
	kept := []byte(`{"item":{"key":"k-kept"}}`)
	_ = o.AppendDecided(Event, "k-kept", "s", "r", []byte(`{"key":"k-kept"}`), kept)
	_ = o.Mark("k-kept", Rejected, "", 400, "invalid", "had item")
	if e, _ := o.Get("k400"); !e.Undecided() {
		t.Fatal("a 400 with no item counts as decided")
	}
	if e, _ := o.Get("k409"); e.Undecided() {
		t.Fatal("a 409 does not count as decided")
	}
	if e, _ := o.Get("k-kept"); e.Undecided() {
		t.Fatal("a 400 with a stored item does not count as decided")
	}
	next := []byte(`{"key":"k400","state":{"fields":{"status":"open"},"observed_at":"2026-11-07T12:00:00.000Z","version":"1"}}`)
	decision := []byte(`{"item":{"key":"k400","source":"github"}}`)
	if err := o.AppendAttempt("k409", "s", "r", next, decision); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("409 attempt: %v", err)
	}
	if err := o.AppendAttempt("k-kept", "s", "r", next, decision); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("stored-item attempt: %v", err)
	}
	if err := o.AppendAttempt("k400", "subj", "new-rule", next, decision); err != nil {
		t.Fatal(err)
	}
	e, _ := o.Get("k400")
	if e.State != Pending || e.Rule != "new-rule" || string(e.Body) != string(next) || string(e.Decision) != string(decision) {
		t.Fatalf("attempt: %+v", e)
	}
	o.Close()
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `\"status\":\"open\",\"observed_at\"`) {
		t.Fatalf("refused body was not kept:\n%s", data)
	}
	r, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, _ = r.Get("k400")
	if e.State != Pending || string(e.Decision) != string(decision) || e.Undecided() {
		t.Fatalf("reloaded attempt: %+v", e)
	}
}
