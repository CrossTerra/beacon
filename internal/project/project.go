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
	// State is the object's state snapshot (DEC-28), in the shape Signal stores.
	State *Snapshot `json:"state,omitempty"`
}

// Snapshot is a state snapshot (DEC-28, Signal plan 12.01). The listed state fields are nested under Fields.
// ObservedAt and Version stay beside Fields; any other key is refused by Signal.
type Snapshot struct {
	Fields     map[string]string `json:"fields"`
	ObservedAt string            `json:"observed_at"`
	Version    string            `json:"version"`
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
	// Truncated omits the body and marks the event truncated. The marker is a fixed flag, not item content.
	Truncated bool
}

// Decision is what a later pass needs to decide the item again and project it. It keeps the ids, times, and the
// fields the rule listed. It does not keep an unlisted field, including a body. It stays on the machine.
type Decision struct {
	Item          source.Item `json:"item"`
	Fields        []string    `json:"fields,omitempty"`
	State         []string    `json:"state,omitempty"`
	Ask           string      `json:"ask,omitempty"`
	Priority      string      `json:"priority,omitempty"`
	CloseByOthers bool        `json:"close_by_others,omitempty"`
	Truncated     bool        `json:"truncated,omitempty"`
}

// Decide records the ids, times, and listed fields a send used. An unlisted field is not copied.
func Decide(it source.Item, rule *rules.Rule, truncated bool) Decision {
	d := Decision{
		Truncated: truncated,
		Item: source.Item{
			Source: it.Source, Subject: it.Subject, Key: it.Key, Type: it.Type,
			// The change description is not a source field. A retry must project the same event title, and it
			// never carries comment or body text.
			EventTitle: it.EventTitle,
			OccurredAt: it.OccurredAt, Own: it.Own, Addressed: it.Addressed, DueAt: it.DueAt,
		},
	}
	if rule == nil {
		return d
	}
	d.Fields = append([]string(nil), rule.Fields...)
	d.State = append([]string(nil), rule.State...)
	d.Ask = rule.Ask
	d.Priority = rule.Priority
	d.CloseByOthers = rule.CloseByOthers
	fields := map[string]string{}
	for _, name := range rule.Fields {
		switch name {
		case rules.FieldActor:
			d.Item.Actor = it.Actor
		case rules.FieldReferences:
			if len(it.References) > 0 {
				d.Item.References = append([]string(nil), it.References...)
			}
		default:
			if v, ok := it.Fields[name]; ok {
				fields[name] = v
			}
		}
	}
	if len(fields) > 0 {
		d.Item.Fields = fields
	}
	state := map[string]string{}
	for _, name := range rule.State {
		if name == source.StateObservedAt || name == source.StateVersion {
			continue
		}
		if v, ok := it.State[name]; ok {
			state[name] = v
		}
	}
	if len(state) > 0 {
		d.Item.State = state
		d.Item.ObservedAt = it.ObservedAt
		d.Item.Version = it.Version
	}
	return d
}

// Narrow is a send rule whose fields, state, ask, and close are listed by both this decision and the current rule.
// Nil when the current rule does not send: a retry must not widen what leaves.
func (d Decision) Narrow(current *rules.Rule) *rules.Rule {
	if current == nil || current.Tier != rules.Send {
		return nil
	}
	r := &rules.Rule{
		Name:   current.Name,
		Tier:   rules.Send,
		Fields: intersect(d.Fields, current.Fields),
		State:  intersect(d.State, current.State),
	}
	if d.Ask == "me" && current.Ask == "me" {
		r.Ask = "me"
		r.Priority = current.Priority
	}
	r.CloseByOthers = d.CloseByOthers && current.CloseByOthers
	return r
}

func intersect(stored, current []string) []string {
	have := map[string]bool{}
	for _, name := range current {
		have[name] = true
	}
	var out []string
	for _, name := range stored {
		if have[name] {
			out = append(out, name)
		}
	}
	return out
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
	// Every service source sets this to describe the change. It is not the object's title, and it is not gated by
	// listing title: a thread's events would otherwise all read as that title. It never includes a body.
	if it.EventTitle != "" {
		b.Title = title(it.EventTitle)
	}
	fields := map[string]string{}
	meta := map[string]any{}
	for _, name := range r.Fields {
		switch name {
		case rules.FieldActor:
			// Other people only (DEC-27): the person's own identity leaves only as by, on own acts.
			if it.Actor != "" && !it.Own {
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
				// The object's title stays on the thread. It is the event title only when the source did not
				// describe the change.
				b.Open.Title = title(v)
				if b.Title == "" {
					b.Title = b.Open.Title
				}
			case FieldBody:
				if in.Truncated {
					continue
				}
				b.Body = v
			default:
				fields[name] = v
			}
		}
	}
	// An issue's or an event's summary, and a mail's subject, is its title. It stays on the thread when the rule
	// lists it and did not list title (summary before subject). It also remains in metadata.fields, which is where a
	// listed field other than title goes.
	for _, name := range []string{"summary", "subject"} {
		if b.Open.Title != "" {
			break
		}
		if v, ok := fields[name]; ok {
			b.Open.Title = title(v)
		}
	}
	if len(fields) > 0 {
		meta["fields"] = fields
	}
	if in.Truncated {
		meta["truncated"] = true
	}
	if len(meta) > 0 {
		b.Metadata = meta
	}
	if len(it.State) > 0 && it.ObservedAt != nil && it.Version != "" {
		fields := map[string]string{}
		for _, name := range r.State {
			if name == source.StateObservedAt || name == source.StateVersion {
				continue
			}
			if v, ok := it.State[name]; ok {
				fields[name] = v
			}
		}
		if len(fields) > 0 {
			b.State = &Snapshot{Fields: fields, ObservedAt: Instant(*it.ObservedAt), Version: it.Version}
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
