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
