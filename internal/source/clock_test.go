package source

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClockUsesFirstDate(t *testing.T) {
	var c Clock
	h := http.Header{}
	h.Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
	c.Note(h)
	h.Set("Date", "Sun, 08 Nov 2026 12:30:00 GMT")
	c.Note(h)
	if !c.First().Equal(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("latest date won: %s", c.First())
	}
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	next, err := c.Next(time.Time{}, now, nil)
	if err != nil || string(next) != "2026-11-08T12:00:00Z" {
		t.Fatalf("%s %v", next, err)
	}
	c.Reset()
	if !c.First().IsZero() {
		t.Fatal("reset kept the previous pass")
	}
}

func TestCursorDoesNotMoveBackwardsOrJump(t *testing.T) {
	var c Clock
	h := http.Header{}
	h.Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
	c.Note(h)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	stored := time.Date(2026, 11, 8, 13, 0, 0, 0, time.UTC)
	next, err := c.Next(stored, now, nil)
	if err != nil || string(next) != "2026-11-08T13:00:00Z" {
		t.Fatalf("moved backwards: %s %v", next, err)
	}
	c.Reset()
	h.Set("Date", now.Add(2*time.Hour).Format(http.TimeFormat))
	c.Note(h)
	if _, err := c.Next(time.Time{}, now, nil); err == nil {
		t.Fatal("a Date an hour ahead was accepted")
	}
}

func TestNoDateIgnoresLocalStamps(t *testing.T) {
	var c Clock
	now := time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC)
	if _, err := c.Next(time.Time{}, now, nil); err == nil {
		t.Fatal("local clock became the cursor")
	}
	stored := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	next, err := c.Next(stored, now, nil)
	if err != nil || string(next) != "2026-11-08T12:00:00Z" {
		t.Fatalf("%s %v", next, err)
	}
	event := time.Date(2026, 11, 8, 12, 10, 0, 0, time.UTC)
	next, err = c.Next(stored, now, []time.Time{event})
	if err != nil || string(next) != "2026-11-08T12:10:00Z" {
		t.Fatalf("%s %v", next, err)
	}
}

func TestFitKey(t *testing.T) {
	short := "github:acme/app#1:review:12"
	got, ver := FitKey(short, "review:12")
	if got != short || ver != "review:12" {
		t.Fatalf("short key changed: %s %s", got, ver)
	}
	long := "github:" + strings.Repeat("a", 250)
	key, ver := FitKey(long, "a")
	if len(key) > 200 || !strings.HasSuffix(key, ":"+ver) || !strings.HasPrefix(key, "github:") {
		t.Fatalf("%d %s", len(key), key)
	}
	again, ver2 := FitKey(long, "a")
	if key != again || ver != ver2 {
		t.Fatal("not stable")
	}
}
