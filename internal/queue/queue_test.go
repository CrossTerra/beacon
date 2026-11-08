package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

func TestHoldAndDrop(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	h := Held{Key: "k1", Rule: "r", HeldAt: now, Item: source.Item{Key: "k1", Fields: map[string]string{"title": "t"}}}
	for i := 0; i < 2; i++ {
		if err := q.Hold(h); err != nil {
			t.Fatal(err)
		}
		if err := q.Drop(Dropped{Key: "k2", DroppedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	q, _ = Open(dir)
	if !q.IsHeld("k1") || len(q.HeldItems()) != 1 || q.HeldItems()[0].Item.Fields["title"] != "t" {
		t.Fatalf("%+v", q.HeldItems())
	}
	data, _ := os.ReadFile(filepath.Join(dir, DroppedFile))
	if strings.Count(string(data), "\n") != 1 || strings.Contains(string(data), "title") {
		t.Fatalf("dropped record:\n%s", data)
	}
}

// A line torn by a crash is cut off, so the next append does not join it and corrupt the file.
func TestTornLastLineIsCut(t *testing.T) {
	for _, torn := range []string{`{"key":"k9","rule":"r","dro`, "{\"key\":\"k9\",\"ru\n"} {
		dir := t.TempDir()
		q, _ := Open(dir)
		_ = q.Drop(Dropped{Key: "k1", DroppedAt: time.Now()})
		path := filepath.Join(dir, DroppedFile)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		_, _ = f.WriteString(torn)
		f.Close()

		q, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Drop(Dropped{Key: "k2", DroppedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		q, err = Open(dir)
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

func TestCorruptMiddleLineIsAnError(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, HeldFile), []byte("not json\n{\"key\":\"k\"}\n"), 0o600)
	if _, err := Open(dir); err == nil {
		t.Fatal("loaded a corrupt line that is not the last")
	}
}
