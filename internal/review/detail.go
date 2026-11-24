package review

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/CrossTerra/beacon/internal/approval"
)

type field struct{ Key, Value string }

type detail struct {
	approval.View
	Fields, Destination          []field
	Exact, CSRF                  string
	SlackPreview                 *slackPreview
	ShowFrozen, Review, ReadBack bool
}

func (s *Site) detail(w http.ResponseWriter, r *http.Request, sess session, at time.Time) {
	ref, ok := strings.CutPrefix(r.URL.Path, "/p/")
	id, extra, more := strings.Cut(ref, "/")
	if !ok || !approval.ValidID(id, "apr") || more && extra != "frozen" {
		http.NotFound(w, r)
		return
	}
	v, _, err := s.journal.Get(id, at)
	if errors.Is(err, approval.ErrNotFound) || err == nil && more && v.Frozen == nil {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "journal unavailable", http.StatusServiceUnavailable)
		return
	}
	model := detail{View: v, ShowFrozen: more, CSRF: sess.csrf, Review: v.Status == "pending" && s.supports(v.Type),
		ReadBack: v.Status == "outcome_unknown" && s.actions.ReadBack != nil && s.supports(v.Type)}
	params := v.Params
	if more {
		params = v.Frozen.Params
		model.Destination = fields(v.Frozen.Destination)
	}
	b, err := approval.Canonical(params)
	if err != nil {
		http.Error(w, "parameters unavailable", http.StatusServiceUnavailable)
		return
	}
	model.Exact, model.Fields = string(b), fields(params)
	model.SlackPreview = slackDraft(v.Type, params)
	render(w, "proposal", http.StatusOK, model)
}

func fields(o approval.Object) []field {
	out := make([]field, 0, len(o))
	for _, key := range slices.Sorted(maps.Keys(o)) {
		value, ok := o[key].(string)
		if !ok {
			b, _ := approval.Canonical(o[key])
			value = string(b)
		}
		out = append(out, field{key, value})
	}
	return out
}

func actionName(a approval.Action) string {
	switch a {
	case approval.Merge:
		return "Merge a pull request"
	case approval.Post:
		return "Post a Slack message"
	case approval.Run:
		return "Run an agent"
	default:
		return "Proposal"
	}
}

// Like the Swift desk, surface invisible formatting and bidi controls instead of letting them change
// the way the text is read. Keep newlines and tabs. This is for display only, not for hashes or storage.
func visibleText(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for _, r := range s {
		if hidden(r) {
			fmt.Fprintf(&out, "⟨U+%04X⟩", r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func hidden(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	switch r {
	case 0x034F, 0x115F, 0x1160, 0x17B4, 0x17B5, 0x2028, 0x2029, 0x3164, 0xFFA0:
		return true
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Variation_Selector, r) ||
		r >= 0x2065 && r <= 0x206F || r >= 0xE0000 && r <= 0xE007F
}
