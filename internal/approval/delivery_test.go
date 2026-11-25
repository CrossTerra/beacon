package approval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type refusal struct {
	status                     int
	code, field, reason, after string
}

type fakeSignal struct {
	mu             sync.Mutex
	transport      *SignalRecords
	posted, stored [][]byte
	keys           []string
	lose           string
	person         string
	script         map[string][]refusal
}

func signalFake(t *testing.T) *fakeSignal {
	t.Helper()
	s := &fakeSignal{script: map[string][]refusal{}}
	listener := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(listener.Close)
	var err error
	s.transport, err = NewSignalRecords(listener.URL, func(h http.Header) { h.Set("Authorization", "Bearer sig_fixture_key") })
	must(t, err)
	return s
}

func (s *fakeSignal) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method != "POST" || r.URL.Path != "/api/app-records" || r.Header.Get("Content-Type") != "application/json" {
		w.WriteHeader(404)
		return
	}
	if r.Header.Get("Authorization") != "Bearer sig_fixture_key" {
		w.WriteHeader(401)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxRecord+1))
	if err != nil || len(b) > maxRecord {
		w.WriteHeader(413)
		return
	}
	o, err := ParseObject(b)
	key := text(o, "key")
	parts := strings.Split(key, ":")
	if err != nil || len(parts) != 3 || parts[0] != "beacon" || !ValidID(parts[1], "jrn") {
		w.WriteHeader(400)
		return
	}
	s.posted = append(s.posted, b)
	if script := s.script[key]; len(script) > 0 {
		s.script[key] = script[1:]
		writeRefusal(w, script[0])
		return
	}
	ack := func(i int, replayed bool) {
		event, _ := newID("evt", t0.Add(time.Duration(i)*time.Second))
		w.Header().Set("Content-Type", "application/json")
		if !replayed {
			w.WriteHeader(201)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": event, "replayed": replayed})
	}
	if i := slices.Index(s.keys, key); i >= 0 {
		if bytes.Equal(s.stored[i], b) {
			ack(i, true)
		} else {
			writeRefusal(w, refusal{status: 409, code: "idempotency_conflict", field: "/key"})
		}
		return
	}
	if s.person != "" && text(obj(o["metadata"]), "person") != s.person {
		writeRefusal(w, refusal{status: 400, code: "invalid", field: "/metadata/person"})
		return
	}
	seq, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	if len(s.keys) > 0 {
		last := strings.Split(s.keys[len(s.keys)-1], ":")
		lastSeq, _ := strconv.ParseInt(last[2], 10, 64)
		if !(parts[1] == last[1] && seq > lastSeq || parts[1] > last[1] && seq == 1) {
			writeRefusal(w, refusal{status: 409, code: "conflict", reason: "journal_order"})
			return
		}
	}
	s.keys, s.stored = append(s.keys, key), append(s.stored, b)
	if s.lose == key {
		s.lose = ""
		c, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			c.Close()
		}
		return
	}
	ack(len(s.keys)-1, false)
}

func writeRefusal(w http.ResponseWriter, f refusal) {
	if f.after != "" {
		w.Header().Set("Retry-After", f.after)
	}
	w.WriteHeader(f.status)
	_ = json.NewEncoder(w).Encode(Object{"error": Object{"code": f.code, "details": Object{"reason": f.reason, "field": f.field}}})
}

func key(j *Journal, seq int) string { return fmt.Sprintf("beacon:%s:%d", j.ID(), seq) }

func drain(t *testing.T, j *Journal, s *fakeSignal, as SignalIdentity, want DrainResult) {
	t.Helper()
	out, err := j.Drain(t.Context(), as, s.transport)
	must(t, err)
	if out != want {
		t.Fatalf("drain = %+v, want %+v", out, want)
	}
}

func TestSignalFixtureDeliveryInExactOrder(t *testing.T) {
	j, records := fixtureJournal(t)
	s := signalFake(t)
	drain(t, j, s, who, DrainResult{Sent: len(records)})
	drain(t, j, s, who, DrainResult{})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.posted) != len(records) || len(s.stored) != len(records) {
		t.Fatal("incorrect delivery count")
	}
	for i, expected := range records {
		expected["key"] = key(j, i+1) // the fixture includes runner events, which the skeleton does not generate
		b, err := Canonical(expected)
		must(t, err)
		if !bytes.Equal(s.stored[i], b) {
			t.Fatalf("record %d: got %s\nwant %s", i+1, s.stored[i], b)
		}
	}
}

func TestLostReplyAndRestart(t *testing.T) {
	dir := scratch(t)
	j := open(t, dir, t0)
	for range 3 {
		put(t, j, mergeRequest())
	}
	s := signalFake(t)
	s.lose = key(j, 2)
	drain(t, j, s, who, DrainResult{Sent: 1, Retry: true})
	must(t, j.Close())
	j = open(t, dir, t0.Add(time.Second))
	drain(t, j, s, who, DrainResult{Sent: 2})
	drain(t, j, s, who, DrainResult{})
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Equal(s.keys, []string{key(j, 1), key(j, 2), key(j, 3)}) || len(s.posted) != 4 || !bytes.Equal(s.posted[1], s.posted[2]) {
		t.Fatal("lost reply did not replay exactly once in place")
	}
}

func TestConflictsHoldTheHeadAcrossRestart(t *testing.T) {
	for _, reason := range []string{"transition", "idempotency_conflict"} {
		t.Run(reason, func(t *testing.T) {
			dir := scratch(t)
			j := open(t, dir, t0)
			for range 3 {
				put(t, j, mergeRequest())
			}
			s := signalFake(t)
			if reason == "transition" {
				s.script[key(j, 2)] = []refusal{{status: 409, code: "conflict", reason: reason}}
			} else {
				// Seed genuinely different bytes under key 2, after the first was acknowledged.
				first, err := j.beginDelivery(who)
				must(t, err)
				a := s.transport.PostRecord(t.Context(), first)
				must(t, j.endDelivery("ack", a.EventID, false))
				body, err := j.beginDelivery(who)
				must(t, err)
				foreign, err := ParseObject(body)
				must(t, err)
				foreign["occurred_at"] = "2030-01-01T00:00:00.000Z"
				b, err := Canonical(foreign)
				must(t, err)
				if s.transport.PostRecord(t.Context(), b).EventID == "" {
					t.Fatal("failed to seed conflict")
				}
			}
			want := DrainResult{Stopped: "conflict:" + reason}
			if reason == "transition" {
				want.Sent = 1
			}
			drain(t, j, s, who, want)
			must(t, j.Close())
			j = open(t, dir, t0)
			want.Sent = 0
			drain(t, j, s, who, want)
			if got := j.DeliveryStatus(); got != (DeliveryStatus{1, 2, want.Stopped}) {
				t.Fatalf("status = %+v", got)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, b := range s.posted {
				o, _ := ParseObject(b)
				if o["key"] == key(j, 3) {
					t.Fatal("overtook a stopped head")
				}
			}
		})
	}
}

func TestRetriesKeepExactBytes(t *testing.T) {
	j := open(t, scratch(t), t0)
	for range 2 {
		put(t, j, mergeRequest())
	}
	s := signalFake(t)
	s.script[key(j, 1)] = []refusal{{status: 429, after: "120"}, {status: 401}, {status: 400, field: "/occurred_at"}, {status: 403}, {status: 503, after: "99999"}}
	for _, wait := range []time.Duration{2 * time.Minute, 0, 0, 0, time.Hour} {
		drain(t, j, s, who, DrainResult{Retry: true, RetryAfter: wait})
	}
	drain(t, j, s, who, DrainResult{Sent: 2})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.posted) != 7 {
		t.Fatal("wrong number of attempts")
	}
	for _, retry := range s.posted[:6] {
		if !bytes.Equal(s.posted[0], retry) {
			t.Fatal("retry changed the bytes")
		}
	}
}

func TestResumeAndPersonCorrection(t *testing.T) {
	for _, mode := range []string{"unbuildable", "refused", "temporary_5xx", "crash_before_ack"} {
		t.Run(mode, func(t *testing.T) {
			dir := scratch(t)
			j := open(t, dir, t0)
			put(t, j, mergeRequest())
			s := signalFake(t)
			corrected := SignalIdentity{Person: "act_01m6a6ft00e40r2081040g2082"}
			wantIdentical := false
			switch mode {
			case "unbuildable":
				drain(t, j, s, SignalIdentity{Person: "@winton"}, DrainResult{Stopped: "record:/metadata/person"})
			case "refused":
				s.person = corrected.Person
				drain(t, j, s, who, DrainResult{Stopped: "refused:400:/metadata/person"})
			case "temporary_5xx":
				wantIdentical = true
				s.script[key(j, 1)] = []refusal{{status: 503}, {status: 400, field: "/metadata/person"}}
				drain(t, j, s, who, DrainResult{Retry: true})
				drain(t, j, s, who, DrainResult{Stopped: "refused:400:/metadata/person"})
			case "crash_before_ack":
				wantIdentical = true
				body, err := j.beginDelivery(who)
				must(t, err)
				if s.transport.PostRecord(t.Context(), body).EventID == "" {
					t.Fatal("fake never stored first request")
				}
				must(t, j.Close()) // process never recorded the success (or ambiguous reply)
				j = open(t, dir, t0)
				s.mu.Lock()
				s.script[key(j, 1)] = []refusal{{status: 400, field: "/metadata/person"}}
				s.mu.Unlock()
				drain(t, j, s, who, DrainResult{Stopped: "refused:400:/metadata/person"})
			}
			must(t, j.Close())
			j = open(t, dir, t0)
			if j.DeliveryStatus().Stopped == "" {
				t.Fatal("stop was not durable")
			}
			must(t, j.ResumeDelivery())
			drain(t, j, s, corrected, DrainResult{Sent: 1})
			s.mu.Lock()
			defer s.mu.Unlock()
			if (len(s.posted) > 1 && bytes.Equal(s.posted[0], s.posted[len(s.posted)-1])) != wantIdentical {
				t.Fatal("resume rebuilt an ambiguous body or failed to rebuild a definite refusal")
			}
		})
	}
}

func TestNewJournalAndLocalOnlyEntries(t *testing.T) {
	s := signalFake(t)
	old := open(t, scratch(t), t0)
	p := put(t, old, mergeRequest())
	drain(t, old, s, who, DrainResult{Sent: 1})
	dir := scratch(t)
	if _, err := OpenJournal(dir, t0.Add(-time.Millisecond), old.ID()); err != ErrJournalAge {
		t.Fatalf("older journal was created: %v", err)
	}
	fresh, err := OpenJournal(dir, t0.Add(time.Millisecond), old.ID())
	must(t, err)
	t.Cleanup(func() { fresh.Close() })
	must(t, EnrollPasskey(dir, 0, t0, []byte("sealed fixture"), func([]byte) error { return nil }, func() error { return nil }))
	for _, kind := range []string{"settings_changed", "passkeys_reset"} {
		must(t, fresh.RecordLocal(kind, t0))
	}
	put(t, fresh, mergeRequest())
	must(t, fresh.Close())
	fresh = open(t, dir, t0)
	drain(t, fresh, s, who, DrainResult{Sent: 1})
	must(t, old.Decline(p.ID, "", t0))
	drain(t, old, s, who, DrainResult{Stopped: "conflict:journal_order"})
	s.mu.Lock()
	if !slices.Equal(s.keys, []string{key(old, 1), key(fresh, 1)}) {
		t.Error("local entries moved the Signal sequence")
	}
	replay := bytes.Clone(s.stored[0])
	s.mu.Unlock()
	if s.transport.PostRecord(t.Context(), replay).EventID == "" {
		t.Error("exact old-journal replay was refused")
	}
}

func TestRecordTransportRejectsRedirectsAndBadOrigins(t *testing.T) {
	for _, s := range []string{"http://example.test", "https://someone@signal.example", "https://signal.test/api", "https://signal.test#x", "file:///tmp/a"} {
		if _, err := NewSignalRecords(s, func(http.Header) {}); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	reached := make(chan struct{}, 1)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached <- struct{}{} }))
	t.Cleanup(other.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	t.Cleanup(redirect.Close)
	s, err := NewSignalRecords(redirect.URL, func(h http.Header) {})
	must(t, err)
	if got := s.PostRecord(t.Context(), []byte(`{}`)); !got.Ambiguous {
		t.Fatal("redirect was treated as a definite response")
	}
	select {
	case <-reached:
		t.Fatal("transport followed a redirect")
	default:
	}
}
