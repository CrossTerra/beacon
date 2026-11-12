package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// Fixture is a source that reads recorded items from a JSON file. It contacts no host.
//
// The file is {"source": "<name>", "fields": [...], "state_fields": [...], "items": [...]}, each item in the shape
// of Item without "source", which the file names once.
type Fixture struct {
	name        string
	fields      []string
	stateFields []string
	items       []Item
}

type fixtureFile struct {
	Source      string        `json:"source"`
	Fields      []string      `json:"fields"`
	StateFields []string      `json:"state_fields"`
	Items       []fixtureItem `json:"items"`
}

// fixtureItem is an item with what it gives the raise (Local), which an Item never serialises.
type fixtureItem struct {
	Item
	Local *Local `json:"local,omitempty"`
}

// LoadFixture reads a fixture file. Unknown keys are errors, and every item must validate.
func LoadFixture(path string) (*Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f fixtureFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !ValidName(f.Source) {
		return nil, fmt.Errorf("%s: source name %q is invalid", path, f.Source)
	}
	if err := ValidStateFields(f.StateFields); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: more than one JSON value", path)
	}
	items := make([]Item, len(f.Items))
	for i := range f.Items {
		items[i] = f.Items[i].Item
		items[i].Local = f.Items[i].Local
	}
	seen := map[string]bool{}
	for i := range items {
		if items[i].Source == "" {
			items[i].Source = f.Source
		}
		if items[i].Source != f.Source {
			return nil, fmt.Errorf("%s: item %d names source %q, not %q", path, i, items[i].Source, f.Source)
		}
		if err := Validate(items[i], f.Fields, f.StateFields); err != nil {
			return nil, fmt.Errorf("%s: item %d: %w", path, i, err)
		}
		if seen[items[i].Key] {
			return nil, fmt.Errorf("%s: item %d repeats key %q", path, i, items[i].Key)
		}
		seen[items[i].Key] = true
	}
	return &Fixture{name: f.Source, fields: f.Fields, stateFields: f.StateFields, items: items}, nil
}

// Name implements Source.
func (f *Fixture) Name() string { return f.name }

// Fields implements Source.
func (f *Fixture) Fields() []string { return append([]string(nil), f.fields...) }

// StateFields implements Source.
func (f *Fixture) StateFields() []string { return append([]string(nil), f.stateFields...) }

// Hosts implements Source. A fixture contacts nothing.
func (f *Fixture) Hosts() []string { return nil }

// Collect implements Source. A fixture has no cursor.
func (f *Fixture) Collect(context.Context, []byte) (Collected, error) {
	out := make([]Item, len(f.items))
	copy(out, f.items)
	return Collected{Items: out}, nil
}
