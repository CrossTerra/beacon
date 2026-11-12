package rules

import (
	"fmt"
	"sort"
	"strings"
)

// Problem is one finding of Check about one rule.
type Problem struct {
	Rule *Rule
	// Unreachable is true when the rule can never match.
	Unreachable bool
	// Warning is true for a rule that works but may not do what was meant.
	Warning bool
	Message string
}

// Error reports whether the problem is an error in the rule: neither unreachable nor a warning.
func (p Problem) Error() bool { return !p.Unreachable && !p.Warning }

func (p Problem) String() string {
	at := fmt.Sprintf("rule %d %q", p.Rule.Index, p.Rule.Name)
	if p.Rule.Line > 0 {
		at += fmt.Sprintf(" (line %d)", p.Rule.Line)
	}
	switch {
	case p.Unreachable:
		return at + ": can never match: " + p.Message
	case p.Warning:
		return "warning: " + at + ": " + p.Message
	}
	return at + ": " + p.Message
}

// Source is what Check knows about one enabled source.
type Source struct {
	Fields      []string
	StateFields []string
	// Identity is true when the config names the person's own identity in this source.
	Identity bool
	// Personal says whether the source's items are under a personal stem (DEC-10): every item, some, or none.
	Personal Personal
	// PersonalField and PersonalValues, for PersonalSome, say which items are personal: those whose field holds one of
	// the values (Slack: channel_type im, mpim or private).
	PersonalField  string
	PersonalValues []string
}

// Personal is how many of a source's items are under a personal stem.
type Personal int

// Personal kinds.
const (
	PersonalNone Personal = iota
	PersonalAll
	PersonalSome
)

// Check reports rules that can never match under the enabled sources, fields and state fields a rule sends that no
// source it can match has, and own-act rules for sources where the person has no identity, whose events therefore
// carry no `by` (DEC-27).
//
// A rule is reported unreachable when it matches no enabled source, matches on a field none of its sources have, or
// is covered by an earlier rule: every item it could match, the earlier rule matches first. The cover check is
// conservative; it reports only cases it can prove.
func (f *File) Check(sources map[string]Source) []Problem {
	var out []Problem
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)

	for i, r := range f.Rules {
		srcs := r.sources(names)
		if len(srcs) == 0 {
			msg := "no source is enabled"
			if r.Match.Source != nil {
				msg = fmt.Sprintf("source %s matches no enabled source (%s)", r.Match.Source, strings.Join(names, ", "))
			}
			out = append(out, Problem{Rule: r, Unreachable: true, Message: msg})
			continue
		}
		reported := false
		fields := map[string][]string{}
		state := map[string][]string{}
		for n, s := range sources {
			fields[n], state[n] = s.Fields, s.StateFields
		}
		for _, field := range sortedKeys(r.Match.Fields) {
			if !anyHas(fields, srcs, field) {
				out = append(out, Problem{Rule: r, Unreachable: true,
					Message: fmt.Sprintf("field %q is not a field of %s", field, strings.Join(srcs, ", "))})
				reported = true
			}
		}
		for _, field := range r.Fields {
			if field == FieldActor || field == FieldReferences {
				continue
			}
			if !anyHas(fields, srcs, field) {
				out = append(out, Problem{Rule: r,
					Message: fmt.Sprintf("fields: %q is not a field of %s", field, strings.Join(srcs, ", "))})
			}
		}
		for _, name := range r.State {
			if !anyHas(state, srcs, name) {
				out = append(out, Problem{Rule: r,
					Message: fmt.Sprintf("state: %q is not a state field of %s", name, strings.Join(srcs, ", "))})
			}
		}
		if r.Tier != Never {
			out = append(out, r.checkSeal(srcs, sources)...)
		}
		if r.Tier != Never && r.Match.Own != nil && *r.Match.Own {
			for _, n := range srcs {
				if !sources[n].Identity {
					out = append(out, Problem{Rule: r, Warning: true, Message: fmt.Sprintf(
						"own acts from %s are sent without by: person.identities has no %s entry, so Signal records them as Beacon's acts, not yours", n, n)})
				}
			}
		}
		if reported {
			continue
		}
		for _, earlier := range f.Rules[:i] {
			if earlier.covers(r, names) {
				out = append(out, Problem{Rule: r, Unreachable: true,
					Message: fmt.Sprintf("rule %d %q matches every item it could match", earlier.Index, earlier.Name)})
				break
			}
		}
	}
	return out
}

// checkSeal reports a personal body without seal (DEC-32), a seal no personal subject can take (Signal seals only a
// person's own subjects), and, as a warning, a send rule with plaintext personal content (DEC-36).
func (r *Rule) checkSeal(srcs []string, sources map[string]Source) []Problem {
	var personal []string
	for _, n := range srcs {
		if r.canBePersonal(sources[n]) {
			personal = append(personal, n)
		}
	}
	var out []Problem
	switch {
	case r.Seal == "" && r.ListsBody() && len(personal) > 0:
		out = append(out, Problem{Rule: r, Message: fmt.Sprintf(
			"lists body and can match personal items from %s: a personal body leaves only sealed; add seal: owner or seal: owner+agents (DEC-32)", strings.Join(personal, ", "))})
	case r.Seal != "" && len(personal) == 0:
		out = append(out, Problem{Rule: r, Message: fmt.Sprintf(
			"seal: Signal seals only a person's own subjects, and %s items here are shared", strings.Join(srcs, ", "))})
	}
	plaintext := len(r.Plain) > 0
	if r.Seal == "" {
		plaintext = len(r.Fields) > 0 || len(r.State) > 0
	}
	if r.Tier == Send && plaintext && len(personal) > 0 {
		out = append(out, Problem{Rule: r, Warning: true, Message: fmt.Sprintf(
			"plaintext personal content from %s: start as hold until its output has been checked (DEC-36)", strings.Join(personal, ", "))})
	}
	return out
}

// canBePersonal reports whether the rule can match an item of s under a personal stem. For a source where only some
// items are, a condition on the deciding field that matches none of the personal values proves it cannot.
func (r *Rule) canBePersonal(s Source) bool {
	switch s.Personal {
	case PersonalAll:
		return true
	case PersonalSome:
		m := r.Match.Fields[s.PersonalField]
		if m == nil {
			return true
		}
		for _, v := range s.PersonalValues {
			if m.Matches(v) {
				return true
			}
		}
		return false
	}
	return false
}

func sortedKeys(m map[string]*Matcher) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func anyHas(sources map[string][]string, names []string, field string) bool {
	for _, n := range names {
		for _, f := range sources[n] {
			if f == field {
				return true
			}
		}
	}
	return false
}

// sources returns the enabled sources the rule's source condition can match.
func (r *Rule) sources(enabled []string) []string {
	var out []string
	for _, n := range enabled {
		if r.Match.Source == nil || r.Match.Source.Matches(n) {
			out = append(out, n)
		}
	}
	return out
}

// covers reports whether every item b could match, a matches.
func (a *Rule) covers(b *Rule, enabled []string) bool {
	if a.Match.Source != nil {
		for _, s := range b.sources(enabled) {
			if !a.Match.Source.Matches(s) {
				return false
			}
		}
	}
	if !implies(b.Match.Type, a.Match.Type) || !implies(b.Match.Actor, a.Match.Actor) {
		return false
	}
	if a.Match.Own != nil && (b.Match.Own == nil || *b.Match.Own != *a.Match.Own) {
		return false
	}
	if a.Match.Addressed != nil && (b.Match.Addressed == nil || *b.Match.Addressed != *a.Match.Addressed) {
		return false
	}
	for name, am := range a.Match.Fields {
		if !implies(b.Match.Fields[name], am) {
			return false
		}
	}
	return true
}

// implies reports whether every value b matches, a matches. A nil a matches everything; a nil b is any value.
func implies(b, a *Matcher) bool {
	if a == nil {
		return true
	}
	if b == nil {
		return false
	}
	if b.Kind == Exact {
		return a.Matches(b.Pattern)
	}
	if a.Kind == Glob && a.Pattern == "*" {
		return true
	}
	return a.Kind == b.Kind && a.Pattern == b.Pattern
}
