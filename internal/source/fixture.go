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
// The file is {"source": "<name>", "fields": [...], "items": [...]}, each item in the shape of Item without
// "source", which the file names once.
type Fixture struct {
	name   string
	fields []string
	items  []Item
}

type fixtureFile struct {
	Source string   `json:"source"`
	Fields []string `json:"fields"`
	Items  []Item   `json:"items"`
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
	seen := map[string]bool{}
	for i := range f.Items {
		if f.Items[i].Source == "" {
			f.Items[i].Source = f.Source
		}
		if f.Items[i].Source != f.Source {
			return nil, fmt.Errorf("%s: item %d names source %q, not %q", path, i, f.Items[i].Source, f.Source)
		}
		if err := Validate(f.Items[i], f.Fields); err != nil {
			return nil, fmt.Errorf("%s: item %d: %w", path, i, err)
		}
		if seen[f.Items[i].Key] {
			return nil, fmt.Errorf("%s: item %d repeats key %q", path, i, f.Items[i].Key)
		}
		seen[f.Items[i].Key] = true
	}
	return &Fixture{name: f.Source, fields: f.Fields, items: f.Items}, nil
}

// Name implements Source.
func (f *Fixture) Name() string { return f.name }

// Fields implements Source.
func (f *Fixture) Fields() []string { return append([]string(nil), f.fields...) }

// Hosts implements Source. A fixture contacts nothing.
func (f *Fixture) Hosts() []string { return nil }

// Collect implements Source.
func (f *Fixture) Collect(context.Context) ([]Item, error) {
	out := make([]Item, len(f.items))
	copy(out, f.items)
	return out, nil
}
