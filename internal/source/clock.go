package source

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
)

// ErrClock means the server Date cannot be used as a cursor. The text names no item.
var ErrClock = errors.New("source: the server Date cannot be used as a cursor")

// Clock is the first server Date of one collect. Reset it at the start of each pass. A later Date in the same
// pass is ignored: the cursor is the time the pass began, so a pass longer than the lookback does not skip events
// that arrived while it was running.
type Clock struct {
	first time.Time
}

// Reset forgets the previous pass.
func (c *Clock) Reset() { c.first = time.Time{} }

// Note records the Date header if this pass has not seen one yet.
func (c *Clock) Note(h http.Header) {
	if !c.first.IsZero() {
		return
	}
	t, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return
	}
	c.first = t.UTC()
}

// First is the first server Date of this pass, or zero.
func (c *Clock) First() time.Time { return c.first }

// Next is the cursor to commit. stored is the committed cursor, or zero when there is none. now is the local
// clock. A Date more than an hour ahead of now fails the pass. The result is never before stored. With no Date,
// fallback is the newest source event time in events; those times must not be local-clock stamps. A zero result
// is an error: the cursor is not invented from the local clock.
func (c *Clock) Next(stored, now time.Time, events []time.Time) ([]byte, error) {
	if !c.first.IsZero() {
		if !now.IsZero() && c.first.After(now.Add(DateSkew)) {
			return nil, ErrClock
		}
		t := c.first
		if stored.After(t) {
			t = stored
		}
		return []byte(t.UTC().Format(time.RFC3339)), nil
	}
	t := stored
	for _, e := range events {
		if e.After(t) {
			t = e
		}
	}
	if t.IsZero() {
		return nil, ErrClock
	}
	return []byte(t.UTC().Format(time.RFC3339)), nil
}

// FitKey returns key unchanged when it is at most 200 printable characters. A longer key is shortened with a
// hash of the original, so the same change always yields the same key and is not dropped. The returned version
// is the last part of the key.
func FitKey(key, version string) (string, string) {
	if len(key) <= 200 && printable.MatchString(key) {
		return key, version
	}
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:8])
	head, rest, ok := strings.Cut(key, ":")
	if !ok || head == "" {
		head = "k"
		rest = key
	}
	// head + ":" + rest + ":" + h must fit in 200, and must stay printable.
	budget := 200 - len(head) - 2 - len(h)
	if budget < 0 {
		budget = 0
	}
	rest = strings.Map(func(r rune) rune {
		if r >= 0x21 && r <= 0x7e && r != ':' {
			return r
		}
		return -1
	}, rest)
	if len(rest) > budget {
		rest = rest[:budget]
	}
	out := head + ":" + h
	if rest != "" {
		out = head + ":" + rest + ":" + h
	}
	if len(out) > 200 {
		out = out[:200-len(h)-1] + ":" + h
	}
	return out, h
}
