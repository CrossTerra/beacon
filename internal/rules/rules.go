// Package rules decides each item's tier. Rules are YAML, first match wins, and an item no rule matches is never
// sent (DEC-3, DEC-5).
package rules

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/CrossTerra/beacon/internal/source"
)

// Tier is what happens to an item (DEC-4).
type Tier string

const (
	// Send appends the projected item to the outbox and sends it.
	Send Tier = "send"
	// Hold keeps the item in the local review queue until the person approves it.
	Hold Tier = "hold"
	// Never drops the item. It is recorded locally and nothing about it leaves the machine.
	Never Tier = "never"
)

// Item-level fields a rule may name in `fields`, beside the source's own fields.
const (
	FieldActor      = "actor"
	FieldReferences = "references"
)

var priorities = map[string]bool{"low": true, "medium": true, "high": true, "urgent": true}

// File is a parsed rules file.
type File struct {
	Rules []*Rule `yaml:"rules"`
}

// Rule is one rule.
type Rule struct {
	Name          string   `yaml:"name"`
	Match         Match    `yaml:"match"`
	Tier          Tier     `yaml:"tier"`
	Fields        []string `yaml:"fields,omitempty"`
	State         []string `yaml:"state,omitempty"`
	Ask           string   `yaml:"ask,omitempty"`
	Priority      string   `yaml:"priority,omitempty"`
	CloseByOthers bool     `yaml:"close_by_others,omitempty"`

	// Index is the rule's position in the file, from 1.
	Index int `yaml:"-"`
	// Line is the rule's line in the file.
	Line int `yaml:"-"`
}

// Match is the conditions of a rule. Every condition present must hold; an empty match matches every item.
type Match struct {
	Source    *Matcher            `yaml:"source,omitempty"`
	Type      *Matcher            `yaml:"type,omitempty"`
	Actor     *Matcher            `yaml:"actor,omitempty"`
	Own       *bool               `yaml:"own,omitempty"`
	Addressed *bool               `yaml:"addressed,omitempty"`
	Fields    map[string]*Matcher `yaml:"fields,omitempty"`
}

// MatchKind is how a Matcher compares.
type MatchKind string

// Matcher kinds. Glob and regex must match the whole value.
const (
	Exact MatchKind = "exact"
	Glob  MatchKind = "glob"
	Regex MatchKind = "regex"
)

// Matcher tests one string. In YAML it is a plain string (exact), or a mapping with exactly one of exact, glob or
// regex.
type Matcher struct {
	Kind    MatchKind
	Pattern string
	re      *regexp.Regexp
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (m *Matcher) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag != "!!str" {
			return fmt.Errorf("line %d: a match value is a string, or one of exact, glob or regex", n.Line)
		}
		*m = Matcher{Kind: Exact, Pattern: n.Value}
	case yaml.MappingNode:
		if len(n.Content) != 2 {
			return fmt.Errorf("line %d: a matcher has exactly one of exact, glob or regex", n.Line)
		}
		k, v := n.Content[0], n.Content[1]
		if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
			return fmt.Errorf("line %d: %s takes a string", v.Line, k.Value)
		}
		switch MatchKind(k.Value) {
		case Exact, Glob, Regex:
			*m = Matcher{Kind: MatchKind(k.Value), Pattern: v.Value}
		default:
			return fmt.Errorf("line %d: unknown matcher %q: use exact, glob or regex", k.Line, k.Value)
		}
	default:
		return fmt.Errorf("line %d: a match value is a string, or one of exact, glob or regex", n.Line)
	}
	if m.Pattern == "" {
		return fmt.Errorf("line %d: a match value is blank", n.Line)
	}
	return m.compile()
}

func (m *Matcher) compile() error {
	switch m.Kind {
	case Exact:
		return nil
	case Glob:
		m.re = regexp.MustCompile(globToRegexp(m.Pattern))
	case Regex:
		re, err := regexp.Compile(`^(?:` + m.Pattern + `)$`)
		if err != nil {
			return fmt.Errorf("regex %q: %w", m.Pattern, err)
		}
		m.re = re
	}
	return nil
}

// globToRegexp turns a glob into an anchored expression: * is any run of characters, ? is one character.
func globToRegexp(glob string) string {
	var b strings.Builder
	b.WriteString(`^`)
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(`$`)
	return `(?s)` + b.String()
}

// Matches reports whether v satisfies the matcher.
func (m *Matcher) Matches(v string) bool {
	if m.Kind == Exact {
		return v == m.Pattern
	}
	return m.re.MatchString(v)
}

func (m *Matcher) String() string { return fmt.Sprintf("%s %q", m.Kind, m.Pattern) }

// Load reads and parses a rules file.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Parse parses and validates a rules file. Unknown keys are errors.
func Parse(data []byte) (*File, error) {
	var f File
	if len(bytes.TrimSpace(data)) > 0 {
		var root yaml.Node
		if err := yaml.Unmarshal(data, &root); err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil {
			return nil, err
		}
		var extra yaml.Node
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, errors.New("the rules file holds more than one YAML document; keep every rule in one")
		}
		nodes := ruleNodes(&root)
		for i, r := range f.Rules {
			if r == nil {
				return nil, fmt.Errorf("rule %d is empty", i+1)
			}
			r.Index = i + 1
			if i < len(nodes) {
				r.Line = nodes[i].Line
				if !hasMatch(nodes[i]) {
					return nil, fmt.Errorf("rule %d (line %d): match is required; write match: {} to match every item", r.Index, r.Line)
				}
			}
		}
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

func ruleNodes(root *yaml.Node) []*yaml.Node {
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	m := root.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "rules" && m.Content[i+1].Kind == yaml.SequenceNode {
			return m.Content[i+1].Content
		}
	}
	return nil
}

func hasMatch(rule *yaml.Node) bool {
	if rule.Kind != yaml.MappingNode {
		return true
	}
	for i := 0; i+1 < len(rule.Content); i += 2 {
		if rule.Content[i].Value == "match" {
			return rule.Content[i+1].Tag != "!!null"
		}
	}
	return false
}

func (f *File) validate() error {
	var errs []error
	names := map[string]int{}
	for _, r := range f.Rules {
		at := fmt.Sprintf("rule %d", r.Index)
		if r.Line > 0 {
			at = fmt.Sprintf("rule %d (line %d)", r.Index, r.Line)
		}
		if r.Name == "" {
			errs = append(errs, fmt.Errorf("%s: name is required", at))
		} else if prev, ok := names[r.Name]; ok {
			errs = append(errs, fmt.Errorf("%s: name %q is already used by rule %d", at, r.Name, prev))
		} else {
			names[r.Name] = r.Index
		}
		switch r.Tier {
		case Send, Hold:
		case Never:
			if len(r.Fields) > 0 || len(r.State) > 0 || r.Ask != "" || r.Priority != "" || r.CloseByOthers {
				errs = append(errs, fmt.Errorf("%s: a never rule takes no fields, state, ask, priority or close_by_others", at))
			}
		case "":
			errs = append(errs, fmt.Errorf("%s: tier is required: send, hold or never", at))
		default:
			errs = append(errs, fmt.Errorf("%s: tier %q is not send, hold or never", at, r.Tier))
		}
		if r.Ask != "" && r.Ask != "me" {
			errs = append(errs, fmt.Errorf("%s: ask is me or absent: Beacon asks only for its own person (DEC-12)", at))
		}
		if r.Priority != "" {
			if !priorities[r.Priority] {
				errs = append(errs, fmt.Errorf("%s: priority %q is not low, medium, high or urgent", at, r.Priority))
			}
			if r.Ask == "" {
				errs = append(errs, fmt.Errorf("%s: priority applies to an ask: set ask: me", at))
			}
		}
		for _, list := range []struct {
			key   string
			names []string
		}{{"fields", r.Fields}, {"state", r.State}} {
			seen := map[string]bool{}
			for _, name := range list.names {
				if name == "" {
					errs = append(errs, fmt.Errorf("%s: a name in %s is empty", at, list.key))
				}
				if seen[name] {
					errs = append(errs, fmt.Errorf("%s: %s lists %q twice", at, list.key, name))
				}
				seen[name] = true
			}
		}
	}
	return errors.Join(errs...)
}

// UnmarshalYAML implements yaml.Unmarshaler. It decodes by hand so that an unknown key or a key with no value is an
// error: a blank condition must never widen a rule.
func (m *Match) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: match is a mapping", n.Line)
	}
	*m = Match{}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if seen[k.Value] {
			return fmt.Errorf("line %d: match.%s is repeated", k.Line, k.Value)
		}
		seen[k.Value] = true
		if v.Tag == "!!null" {
			return fmt.Errorf("line %d: match.%s has no value", k.Line, k.Value)
		}
		switch k.Value {
		case "source", "type", "actor":
			var mt Matcher
			if err := mt.UnmarshalYAML(v); err != nil {
				return err
			}
			switch k.Value {
			case "source":
				m.Source = &mt
			case "type":
				m.Type = &mt
			default:
				m.Actor = &mt
			}
		case "own", "addressed":
			// Only YAML booleans (true, True, TRUE, false, False, FALSE). yes, no, on, off, 1, 0 and quoted
			// strings are refused rather than guessed.
			var b bool
			if v.Kind != yaml.ScalarNode || v.Tag != "!!bool" || v.Decode(&b) != nil {
				return fmt.Errorf("line %d: match.%s is true or false", k.Line, k.Value)
			}
			if k.Value == "own" {
				m.Own = &b
			} else {
				m.Addressed = &b
			}
		case "fields":
			if v.Kind != yaml.MappingNode {
				return fmt.Errorf("line %d: match.fields is a mapping of field name to matcher", v.Line)
			}
			m.Fields = map[string]*Matcher{}
			for j := 0; j+1 < len(v.Content); j += 2 {
				fk, fv := v.Content[j], v.Content[j+1]
				if _, dup := m.Fields[fk.Value]; dup {
					return fmt.Errorf("line %d: match.fields.%s is repeated", fk.Line, fk.Value)
				}
				if fv.Tag == "!!null" {
					return fmt.Errorf("line %d: match.fields.%s has no value", fk.Line, fk.Value)
				}
				var mt Matcher
				if err := mt.UnmarshalYAML(fv); err != nil {
					return err
				}
				m.Fields[fk.Value] = &mt
			}
		default:
			return fmt.Errorf("line %d: unknown key match.%s", k.Line, k.Value)
		}
	}
	return nil
}

// Decision is the result of deciding one item.
type Decision struct {
	Tier Tier
	// Rule is the matching rule, or nil when none matched and the default applied.
	Rule *Rule
}

// Decide returns the first rule that matches the item, or the default: never.
func (f *File) Decide(it source.Item) Decision {
	for _, r := range f.Rules {
		if r.Match.Matches(it) {
			return Decision{Tier: r.Tier, Rule: r}
		}
	}
	return Decision{Tier: Never}
}

// Lists reports whether a send or hold rule that can match the source names field in its fields. A source uses it
// to skip reading what no rule could send.
func (f *File) Lists(sourceName, field string) bool {
	for _, r := range f.Rules {
		if r.Tier != Send && r.Tier != Hold {
			continue
		}
		if r.Match.Source != nil && !r.Match.Source.Matches(sourceName) {
			continue
		}
		for _, name := range r.Fields {
			if name == field {
				return true
			}
		}
	}
	return false
}

// GapsAbove reports whether a rule before matched could still match, and names a field or actor the item does not
// have. A condition the item does have that already fails proves the rule cannot apply, so it does not block a
// later send. A nil matched means every rule is above the default.
func (f *File) GapsAbove(it source.Item, matched *Rule) bool {
	for _, r := range f.Rules {
		if r == matched {
			return false
		}
		if r.Match.needsMissing(it) {
			return true
		}
	}
	return false
}

// needsMissing reports whether this match could still apply, and names an actor or a field the item does not have.
// An empty actor is missing. A known miss (source, type, own, addressed, a stored actor, or a stored field) proves
// the rule cannot apply.
func (m *Match) needsMissing(it source.Item) bool {
	if m.ruledOut(it) {
		return false
	}
	if m.Actor != nil && it.Actor == "" {
		return true
	}
	for name := range m.Fields {
		if _, ok := it.Fields[name]; !ok {
			return true
		}
	}
	return false
}

// ruledOut reports whether a condition the item does have already fails. A missing field or actor is not a miss:
// it has not been proved.
func (m *Match) ruledOut(it source.Item) bool {
	if m.Source != nil && !m.Source.Matches(it.Source) {
		return true
	}
	if m.Type != nil && !m.Type.Matches(it.Type) {
		return true
	}
	if m.Own != nil && *m.Own != it.Own {
		return true
	}
	if m.Addressed != nil && *m.Addressed != it.Addressed {
		return true
	}
	if m.Actor != nil && it.Actor != "" && !m.Actor.Matches(it.Actor) {
		return true
	}
	for name, mt := range m.Fields {
		if v, ok := it.Fields[name]; ok && !mt.Matches(v) {
			return true
		}
	}
	return false
}

// Matches reports whether every condition holds for the item. A field condition on a field the item lacks fails.
func (m *Match) Matches(it source.Item) bool {
	if m.Source != nil && !m.Source.Matches(it.Source) {
		return false
	}
	if m.Type != nil && !m.Type.Matches(it.Type) {
		return false
	}
	if m.Actor != nil && !m.Actor.Matches(it.Actor) {
		return false
	}
	if m.Own != nil && *m.Own != it.Own {
		return false
	}
	if m.Addressed != nil && *m.Addressed != it.Addressed {
		return false
	}
	for name, mt := range m.Fields {
		v, ok := it.Fields[name]
		if !ok || !mt.Matches(v) {
			return false
		}
	}
	return true
}
