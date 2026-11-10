package slack

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// cursorData is a pass's position: when it ran, the threads it follows, each conversation's watermark, the mentions
// already seen, and the asks it checks for deletion. It holds ids, kinds, subjects and timestamps, no text.
type cursorData struct {
	// Since is the first server Date of the pass that stored it. Search reads from 15 minutes before it.
	Since string `json:"since"`
	// Threads are keyed by "<channel>/<thread ts>".
	Threads map[string]*thread `json:"threads,omitempty"`
	// Channels holds the configured channels and the group direct messages, by id.
	Channels map[string]*watermark `json:"channels,omitempty"`
	// Mentions maps "<channel>/<ts>" of each mention search found in the last 7 days to its ts.
	Mentions map[string]string `json:"mentions,omitempty"`
	// Asked holds messages that addressed the person, by "<channel>/<ts>", to notice their deletion.
	Asked map[string]*asked `json:"asked,omitempty"`
}

func (c *cursorData) init() {
	if c.Threads == nil {
		c.Threads = map[string]*thread{}
	}
	if c.Channels == nil {
		c.Channels = map[string]*watermark{}
	}
	if c.Mentions == nil {
		c.Mentions = map[string]string{}
	}
	if c.Asked == nil {
		c.Asked = map[string]*asked{}
	}
}

// thread is a thread the person posted in or was mentioned in.
type thread struct {
	// Kind is the conversation's kind: a thread outside a public channel is personal.
	Kind string `json:"kind,omitempty"`
	// Joined and Last are the ts of the person's first and latest post or mention seen there.
	Joined string `json:"joined"`
	Last   string `json:"last"`
	// Seen is the newest message ts read in the thread; a thread starts at the window of the pass that followed it.
	Seen string `json:"seen,omitempty"`
	// Polled is when the thread was last read.
	Polled string `json:"polled,omitempty"`
}

// watermark is a conversation's top level read so far. Seen is the newest ts read with nothing unread below it. When a
// read stopped at its page limit, Gap is the oldest ts it read and Head the newest: everything between Seen and Gap is
// read next.
type watermark struct {
	Kind   string `json:"kind,omitempty"`
	Seen   string `json:"seen"`
	Gap    string `json:"gap,omitempty"`
	Head   string `json:"head,omitempty"`
	Polled string `json:"polled,omitempty"`
}

// asked is a message that addressed the person: its subject, its thread root when it is a reply, and when it was last
// seen to exist.
type asked struct {
	Subject string `json:"subject"`
	Root    string `json:"root,omitempty"`
	Kind    string `json:"kind"`
	Checked string `json:"checked,omitempty"`
}

func (t *thread) polledAt() string    { return t.Polled }
func (t *thread) latest() string      { return maxTS(t.Last, t.Seen) }
func (w *watermark) polledAt() string { return w.Polled }
func (w *watermark) latest() string   { return maxTS(maxTS(w.Seen, w.Head), w.Gap) }

func maxTS(a, b string) string {
	if tsLess(a, b) {
		return b
	}
	return a
}

var kinds = map[string]bool{"": true, kindIM: true, kindMPIM: true, kindPrivate: true, kindChannel: true}

func parseCursor(b []byte) (cursorData, error) {
	var c cursorData
	if err := json.Unmarshal(b, &c); err != nil {
		return cursorData{}, errors.New("slack: the cursor could not be read")
	}
	if _, err := time.Parse(time.RFC3339, c.Since); err != nil {
		return cursorData{}, errors.New("slack: the cursor time could not be read")
	}
	ts := func(v string, optional bool) bool { return (optional && v == "") || tsPattern.MatchString(v) }
	for k, t := range c.Threads {
		ch, root, ok := strings.Cut(k, "/")
		if !ok || t == nil || !channelPattern.MatchString(ch) || !ts(root, false) || !ts(t.Joined, false) || !ts(t.Last, false) || !ts(t.Seen, true) || !kinds[t.Kind] {
			return cursorData{}, errors.New("slack: the cursor holds a thread it cannot read")
		}
	}
	for ch, w := range c.Channels {
		if w == nil || !channelPattern.MatchString(ch) || !ts(w.Seen, false) || !ts(w.Gap, true) || !ts(w.Head, true) || !kinds[w.Kind] {
			return cursorData{}, errors.New("slack: the cursor holds a conversation it cannot read")
		}
	}
	for k, v := range c.Mentions {
		ch, at, ok := strings.Cut(k, "/")
		if !ok || !channelPattern.MatchString(ch) || at != v || !ts(v, false) {
			return cursorData{}, errors.New("slack: the cursor holds a mention it cannot read")
		}
	}
	for k, a := range c.Asked {
		ch, at, ok := strings.Cut(k, "/")
		if !ok || a == nil || !channelPattern.MatchString(ch) || !ts(at, false) || !ts(a.Root, true) || !kinds[a.Kind] ||
			!(strings.HasPrefix(a.Subject, "slack:") && printableSubject.MatchString(a.Subject)) {
			return cursorData{}, errors.New("slack: the cursor holds an ask it cannot read")
		}
	}
	return c, nil
}

type pollable interface {
	polledAt() string
	latest() string
}

// dueOrder is what this pass reads, at most max: never read first, then longest unread. One with a message in the
// last activeFor is due every pass; a quieter one every quietEvery. Each is read from its own watermark, so one that
// waits loses nothing.
func dueOrder[T pollable](items map[string]T, now time.Time, max int) []string {
	keys := make([]string, 0, len(items))
	for k, it := range items {
		if it.polledAt() != "" {
			polled, err := time.Parse(time.RFC3339, it.polledAt())
			quiet := tsTime(it.latest()).Before(now.Add(-activeFor))
			if err == nil && quiet && now.Sub(polled) < quietEvery {
				continue
			}
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := items[keys[i]].polledAt(), items[keys[j]].polledAt()
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	if len(keys) > max {
		keys = keys[:max]
	}
	return keys
}
