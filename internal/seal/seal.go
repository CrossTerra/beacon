// Package seal holds what Beacon knows about sealed events (DEC-30 to DEC-35): the levels, the local raise to owner
// from seal.owner in the config, and the subject floor. Beacon handles no keys: Signal encrypts the sealed parts at
// rest. Nothing here sends, logs or stores item content.
package seal

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/CrossTerra/beacon/internal/source"
)

// Level is how a sealed event opens in Signal (Signal DEC-253, DEC-254). None is a plain event.
type Level string

// Levels, lowest first.
const (
	None   Level = ""
	Agents Level = "owner+agents"
	Owner  Level = "owner"
)

// Valid reports whether s names a level a rule may seal at.
func Valid(s string) bool { return s == string(Owner) || s == string(Agents) }

// Rank orders levels: None 0, Agents 1, Owner 2. Anything else ranks as Owner, so an unknown level never lowers one.
func (l Level) Rank() int {
	switch l {
	case None:
		return 0
	case Agents:
		return 1
	}
	return 2
}

// Max is the higher of two levels.
func Max(a, b Level) Level {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

// LogName is the level as a log attribute name: owner, owner_agents.
func (l Level) LogName() string {
	if l == Agents {
		return "owner_agents"
	}
	return string(l)
}

// OwnerList is seal.owner in the config: what raises an item to owner on the machine (DEC-33).
type OwnerList struct {
	// Words match whole words, case-insensitively, unless written {exact: PIP}. A word may be a phrase.
	Words []Word `yaml:"words,omitempty"`
	// Senders are addresses (dana@example.com) or domains (example.com, which also covers its subdomains).
	Senders []string `yaml:"senders,omitempty"`
	// Labels are Gmail label names, compared without case.
	Labels []string `yaml:"labels,omitempty"`
}

// Word is one entry in seal.owner.words.
type Word struct {
	Text  string
	Exact bool
	re    *regexp.Regexp
}

// UnmarshalYAML implements yaml.Unmarshaler: a string, or {exact: TEXT}.
func (w *Word) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag != "!!str" {
			return fmt.Errorf("line %d: a seal word is a string, or {exact: TEXT}", n.Line)
		}
		*w = Word{Text: n.Value}
	case yaml.MappingNode:
		if len(n.Content) != 2 || n.Content[0].Value != "exact" || n.Content[1].Kind != yaml.ScalarNode || n.Content[1].Tag != "!!str" {
			return fmt.Errorf("line %d: a seal word is a string, or {exact: TEXT}", n.Line)
		}
		*w = Word{Text: n.Content[1].Value, Exact: true}
	default:
		return fmt.Errorf("line %d: a seal word is a string, or {exact: TEXT}", n.Line)
	}
	return w.compile()
}

// MarshalYAML implements yaml.Marshaler.
func (w Word) MarshalYAML() (any, error) {
	if w.Exact {
		return map[string]string{"exact": w.Text}, nil
	}
	return w.Text, nil
}

func (w *Word) compile() error {
	parts := strings.Fields(w.Text)
	if len(parts) == 0 {
		return fmt.Errorf("a seal word is blank")
	}
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	expr := `(?:^|[^\pL\pN_])` + strings.Join(parts, `\s+`) + `(?:$|[^\pL\pN_])`
	if !w.Exact {
		expr = `(?i)` + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return err
	}
	w.re = re
	return nil
}

// Matches reports whether the word appears in text as a whole word.
func (w *Word) Matches(text string) bool {
	if w.re == nil && w.compile() != nil {
		return false
	}
	return w.re.MatchString(text)
}

var (
	addressPattern = regexp.MustCompile(`^[a-z0-9._%+'-]+@[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	domainPattern  = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+$`)
)

// Validate checks the lists: no blank word or label, and each sender a lowercase address or domain.
func (o *OwnerList) Validate() error {
	for i := range o.Words {
		if err := o.Words[i].compile(); err != nil {
			return fmt.Errorf("seal.owner.words[%d]: %w", i, err)
		}
	}
	for i, s := range o.Senders {
		if !addressPattern.MatchString(s) && !domainPattern.MatchString(s) {
			return fmt.Errorf("seal.owner.senders[%d]: %q is not a lowercase address or domain", i, s)
		}
	}
	for i, l := range o.Labels {
		if strings.TrimSpace(l) == "" {
			return fmt.Errorf("seal.owner.labels[%d] is blank", i)
		}
	}
	return nil
}

// Empty reports whether the lists hold nothing.
func (o *OwnerList) Empty() bool {
	return o == nil || len(o.Words) == 0 && len(o.Senders) == 0 && len(o.Labels) == 0
}

// Match says what in an item's local text matched, for `beacon explain` only: "word \"salary\" in subject". It is
// never logged. Empty when nothing matched.
func (o *OwnerList) Match(l *source.Local) string {
	if o.Empty() || l == nil {
		return ""
	}
	for _, where := range sortedKeys(l.Text) {
		text := l.Text[where]
		for i := range o.Words {
			if o.Words[i].Matches(text) {
				return fmt.Sprintf("word %q in %s", o.Words[i].Text, where)
			}
		}
	}
	for _, where := range sortedKeys(l.Addresses) {
		for _, a := range l.Addresses[where] {
			for _, s := range o.Senders {
				if SenderMatches(s, a) {
					return fmt.Sprintf("sender %q in %s", s, where)
				}
			}
		}
	}
	for _, have := range l.Labels {
		for _, want := range o.Labels {
			if strings.EqualFold(strings.TrimSpace(have), strings.TrimSpace(want)) {
				return fmt.Sprintf("label %q", want)
			}
		}
	}
	return ""
}

// SenderMatches reports whether address is the sender, or is at the sender's domain or a subdomain of it.
func SenderMatches(sender, address string) bool {
	address = strings.ToLower(strings.TrimSpace(address))
	if strings.Contains(sender, "@") {
		return address == sender
	}
	_, domain, ok := strings.Cut(address, "@")
	if !ok {
		return false
	}
	return domain == sender || strings.HasSuffix(domain, "."+sender)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
