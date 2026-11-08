package queue

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/source"
)

// open opens the queue for writing as a send pass does: under the outbox lock.
func open(t *testing.T, dir string) *Queue {
	t.Helper()
	ob, err := outbox.Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ob.Close() })
	q, err := Open(dir, ob)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestHoldAndDrop(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	h := Held{Key: "k1", Rule: "r", HeldAt: now, Item: source.Item{Key: "k1", Fields: map[string]string{"title": "t"}}}
	func() {
		ob, _ := outbox.Open(dir, time.Now)
		defer ob.Close()
		q, err := Open(dir, ob)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := q.Hold(h); err != nil {
				t.Fatal(err)
			}
			if err := q.Drop(Dropped{Key: "k2", DroppedAt: now}); err != nil {
				t.Fatal(err)
			}
		}
	}()
	q, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !q.IsHeld("k1") || len(q.HeldItems()) != 1 || q.HeldItems()[0].Item.Fields["title"] != "t" {
		t.Fatalf("%+v", q.HeldItems())
	}
	data, _ := os.ReadFile(filepath.Join(dir, DroppedFile))
	if strings.Count(string(data), "\n") != 1 || strings.Contains(string(data), "title") {
		t.Fatalf("dropped record:\n%s", data)
	}
}

// A line torn by a crash is cut off by a writer, so the next append does not join it and corrupt the file.
func TestTornLastLineIsCut(t *testing.T) {
	for _, torn := range []string{`{"key":"k9","rule":"r","dro`, "{\"key\":\"k9\",\"ru\n"} {
		dir := t.TempDir()
		func() {
			ob, _ := outbox.Open(dir, time.Now)
			defer ob.Close()
			q, _ := Open(dir, ob)
			_ = q.Drop(Dropped{Key: "k1", DroppedAt: time.Now()})
		}()
		path := filepath.Join(dir, DroppedFile)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		_, _ = f.WriteString(torn)
		f.Close()

		func() {
			ob, _ := outbox.Open(dir, time.Now)
			defer ob.Close()
			q, err := Open(dir, ob)
			if err != nil {
				t.Fatal(err)
			}
			if err := q.Drop(Dropped{Key: "k2", DroppedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}()
		q, err := Read(dir)
		if err != nil {
			t.Fatalf("after a torn line and an append, the file does not load: %v", err)
		}
		for _, k := range []string{"k1", "k2"} {
			if _, ok := q.DroppedBy(k); !ok {
				t.Fatalf("%s lost", k)
			}
		}
		if _, ok := q.DroppedBy("k9"); ok {
			t.Fatal("the torn record loaded")
		}
	}
}

// Read changes nothing: a torn last line, which may be a record a sending process is appending right now, is
// skipped and left in place, nothing is created, and Hold and Drop are refused.
func TestReadNeverWrites(t *testing.T) {
	dir := t.TempDir()
	func() {
		ob, _ := outbox.Open(dir, time.Now)
		defer ob.Close()
		q, _ := Open(dir, ob)
		_ = q.Drop(Dropped{Key: "k1", DroppedAt: time.Now()})
		_ = q.Hold(Held{Key: "h1", Rule: "r", HeldAt: time.Now()})
	}()
	appending := map[string]string{DroppedFile: `{"key":"k2","dropped_at":"2026-11-0`, HeldFile: `{"key":"h2","ru`}
	before := map[string][]byte{}
	for name, partial := range appending {
		path := filepath.Join(dir, name)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		_, _ = f.WriteString(partial)
		f.Close()
		before[name], _ = os.ReadFile(path)
	}

	q, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q.DroppedBy("k1"); !ok || !q.IsHeld("h1") || q.IsHeld("h2") {
		t.Fatal("read the wrong records")
	}
	if err := q.Drop(Dropped{Key: "k3"}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Drop on a read-only queue: %v", err)
	}
	if err := q.Hold(Held{Key: "h3"}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Hold on a read-only queue: %v", err)
	}
	for name, want := range before {
		got, _ := os.ReadFile(filepath.Join(dir, name))
		if string(got) != string(want) {
			t.Fatalf("Read changed %s:\nbefore %q\nafter  %q", name, want, got)
		}
	}

	empty := filepath.Join(t.TempDir(), "state")
	if _, err := Read(empty); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Read created the state directory")
	}
}

func TestOpenNeedsTheOutboxLock(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	if _, err := Open(dir, nil); !errors.Is(err, ErrNotLocked) {
		t.Fatalf("no outbox: %v", err)
	}
	unlocked, _ := outbox.Read(dir)
	if _, err := Open(dir, unlocked); !errors.Is(err, ErrNotLocked) {
		t.Fatalf("read-only outbox: %v", err)
	}
	elsewhere, _ := outbox.Open(other, time.Now)
	if _, err := Open(dir, elsewhere); !errors.Is(err, ErrNotLocked) {
		t.Fatalf("another directory's lock: %v", err)
	}
	elsewhere.Close()
	closed, _ := outbox.Open(dir, time.Now)
	closed.Close()
	if _, err := Open(dir, closed); !errors.Is(err, ErrNotLocked) {
		t.Fatalf("closed outbox: %v", err)
	}
	if q := open(t, dir); q == nil {
		t.Fatal("no queue under the lock")
	}
}

func TestCorruptMiddleLineIsAnError(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, HeldFile), []byte("not json\n{\"key\":\"k\"}\n"), 0o600)
	if _, err := Read(dir); err == nil {
		t.Fatal("loaded a corrupt line that is not the last")
	}
}
