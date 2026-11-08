// Package project builds the Signal request body for an item a rule allowed. Only the fields the rule lists leave
// the machine (DEC-6).
package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/source"
)

// Fields with a place of their own in the request body. Every other listed field goes to metadata.fields.
const (
	FieldTitle = "title"
	FieldBody  = "body"
)

// Limits from Signal (src/domain/limits.ts and REQUEST_BODY_MAX_BYTES).
const (
	titleMaxUnits  = 300
	bodyMaxBytes   = 64 * 1024
	requestMaxSize = 64 * 1024
	closesMax      = 20
)

// Person is who Beacon serves (DEC-27).
type Person struct {
	// Handle is the person's Signal actor, "@handle" or "act_…". Asks name it in `to`.
	Handle string
	// Identities maps a source name to the person's identity there, used for `by` on own acts.
	Identities map[string]string
}

// Body is a Signal write, POST /api/events. Field order is fixed, so the same input always yields the same bytes.
type Body struct {
	Subject    string         `json:"subject"`
	Type       string         `json:"type"`
	Key        string         `json:"key"`
	OccurredAt string         `json:"occurred_at"`
	Title      string         `json:"title,omitempty"`
	Body       string         `json:"body,omitempty"`
	Open       *Open          `json:"open"`
	By         *By            `json:"by,omitempty"`
	To         []string       `json:"to,omitempty"`
	Priority   string         `json:"priority,omitempty"`
	DueAt      string         `json:"due_at,omitempty"`
	Closes     []Ref          `json:"closes,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	// State is the object's state snapshot (DEC-28): the state fields the rule lists, observed_at and version.
	State map[string]string `json:"state,omitempty"`
}

// Open makes sure the subject has a thread (DEC-17).
type Open struct {
	Title string `json:"title,omitempty"`
}

// By names whose act the event records.
type By struct {
	Identity string `json:"identity"`
}

// Ref names an earlier event by its key.
type Ref struct {
	Key string `json:"key"`
}

// Input is everything the projection reads.
type Input struct {
	Item   source.Item
	Rule   *rules.Rule
	Person Person
	// OpenAsks are the keys of events Beacon wrote that ask the person and are not yet closed, on the item's subject,
	// oldest first.
	OpenAsks []string
}

// Project builds the request body and its bytes. It fails if the rule does not send or hold, or if the body would
// exceed Signal's limits.
func Project(in Input) (*Body, []byte, error) {
	r, it := in.Rule, in.Item
	if r == nil || (r.Tier != rules.Send && r.Tier != rules.Hold) {
		return nil, nil, errors.New("project: only items a send or hold rule matched are projected")
	}
	b := &Body{
		Subject:    it.Subject,
		Type:       it.Source + "." + it.Type,
		Key:        it.Key,
		OccurredAt: Instant(it.OccurredAt),
		Open:       &Open{},
	}
	fields := map[string]string{}
	meta := map[string]any{}
	for _, name := range r.Fields {
		switch name {
		case rules.FieldActor:
			if it.Actor != "" {
				meta["actor"] = it.Actor
			}
		case rules.FieldReferences:
			if len(it.References) > 0 {
				meta["references"] = append([]string(nil), it.References...)
			}
		default:
			v, ok := it.Fields[name]
			if !ok {
				continue
			}
			switch name {
			case FieldTitle:
				b.Title = title(v)
				b.Open.Title = b.Title
			case FieldBody:
				b.Body = v
			default:
				fields[name] = v
			}
		}
	}
	if len(fields) > 0 {
		meta["fields"] = fields
	}
	if len(meta) > 0 {
		b.Metadata = meta
	}
	if len(it.State) > 0 && it.ObservedAt != nil {
		state := map[string]string{}
		for _, name := range r.State {
			if v, ok := it.State[name]; ok {
				state[name] = v
			}
		}
		if len(state) > 0 {
			state[source.StateObservedAt] = Instant(*it.ObservedAt)
			state[source.StateVersion] = it.Version
			b.State = state
		}
	}
	if it.Own {
		if id := in.Person.Identities[it.Source]; id != "" {
			b.By = &By{Identity: id}
		}
	} else if r.Ask == "me" {
		b.To = []string{in.Person.Handle}
		b.Priority = r.Priority
		if it.DueAt != nil {
			b.DueAt = Instant(*it.DueAt)
		}
	}
	if it.Own || r.CloseByOthers {
		for i, k := range in.OpenAsks {
			if i == closesMax {
				break
			}
			b.Closes = append(b.Closes, Ref{Key: k})
		}
	}
	if len(b.Body) > bodyMaxBytes {
		return nil, nil, fmt.Errorf("project: body is %d bytes, over Signal's %d", len(b.Body), bodyMaxBytes)
	}
	data, err := Marshal(b)
	if err != nil {
		return nil, nil, err
	}
	if len(data) > requestMaxSize {
		return nil, nil, fmt.Errorf("project: request is %d bytes, over Signal's %d", len(data), requestMaxSize)
	}
	return b, data, nil
}

// Marshal encodes a body. Map keys are sorted by encoding/json, so the bytes are stable.
func Marshal(b *Body) ([]byte, error) { return json.Marshal(b) }

// Instant formats a time as Signal stores it: UTC, millisecond precision.
func Instant(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// title trims a title and shortens it to Signal's limit of 300 UTF-16 code units.
func title(v string) string {
	v = strings.TrimSpace(v)
	if len(utf16.Encode([]rune(v))) <= titleMaxUnits {
		return v
	}
	units := 0
	for i, r := range v {
		n := len(utf16.Encode([]rune{r}))
		if units+n > titleMaxUnits {
			return strings.TrimSpace(v[:i])
		}
		units += n
	}
	return v
}

// AsksOf reports whether a body asks someone, and ClosesOf the keys it closes. The outbox uses them to track open
// asks per subject.
func AsksOf(b *Body) bool { return len(b.To) > 0 }

// ClosesOf returns the keys a body closes, sorted.
func ClosesOf(b *Body) []string {
	out := make([]string, 0, len(b.Closes))
	for _, c := range b.Closes {
		out = append(out, c.Key)
	}
	sort.Strings(out)
	return out
}
