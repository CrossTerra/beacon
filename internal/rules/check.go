package rules

import (
	"fmt"
	"sort"
	"strings"
)

// Problem is one finding of Check about one rule.
type Problem struct {
	Rule *Rule
	// Unreachable is true when the rule can never match; otherwise the problem is an error in the rule.
	Unreachable bool
	Message     string
}

func (p Problem) String() string {
	at := fmt.Sprintf("rule %d %q", p.Rule.Index, p.Rule.Name)
	if p.Rule.Line > 0 {
		at += fmt.Sprintf(" (line %d)", p.Rule.Line)
	}
	if p.Unreachable {
		return at + ": can never match: " + p.Message
	}
	return at + ": " + p.Message
}

// Check reports rules that can never match under the enabled sources, and fields a rule sends that no source it can
// match has. sources maps each enabled source name to its fixed field list.
//
// A rule is reported unreachable when it matches no enabled source, matches on a field none of its sources have, or
// is covered by an earlier rule: every item it could match, the earlier rule matches first. The cover check is
// conservative; it reports only cases it can prove.
func (f *File) Check(sources map[string][]string) []Problem {
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
		for _, field := range sortedKeys(r.Match.Fields) {
			if !anyHas(sources, srcs, field) {
				out = append(out, Problem{Rule: r, Unreachable: true,
					Message: fmt.Sprintf("field %q is not a field of %s", field, strings.Join(srcs, ", "))})
				reported = true
			}
		}
		for _, field := range r.Fields {
			if field == FieldActor || field == FieldReferences {
				continue
			}
			if !anyHas(sources, srcs, field) {
				out = append(out, Problem{Rule: r,
					Message: fmt.Sprintf("fields: %q is not a field of %s", field, strings.Join(srcs, ", "))})
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
