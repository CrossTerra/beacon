package signaltest

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// The limits Signal holds a sealed write to (src/domain/seal.ts, src/domain/limits.ts, src/config.ts).
const (
	sealedPartsMax   = 65_536
	sealedRequestMax = 131_072
	plainRequestMax  = 64 * 1024
	stateFlagsMax    = 16
	metadataMax      = 32 * 1024
	bodyMax          = 64 * 1024
)

var personalStems = []string{"gmail:", "calendar:", "drive:", "slack:dm:"}

// SealedProblem checks a request the way Signal's parseSealedWrite does (Signal DEC-250 to DEC-264), and returns the
// JSON pointer Signal would name in details.field, or "" when Signal would accept it. A body without sealed is
// checked only against the plain request size.
func SealedProblem(body []byte) string {
	var b map[string]json.RawMessage
	if err := json.Unmarshal(body, &b); err != nil {
		return "/"
	}
	raw, ok := b["sealed"]
	if !ok {
		if len(body) > plainRequestMax {
			return "/"
		}
		return ""
	}
	if len(body) > sealedRequestMax {
		return "/"
	}
	var sealed map[string]json.RawMessage
	if json.Unmarshal(raw, &sealed) != nil || sealed == nil {
		return "/sealed"
	}
	for k := range sealed {
		switch k {
		case "level", "title", "body", "metadata", "state", "open":
		default:
			return "/sealed"
		}
	}
	var level string
	if json.Unmarshal(sealed["level"], &level) != nil || (level != "owner" && level != "owner+agents") {
		return "/sealed/level"
	}
	var typ, subject string
	_ = json.Unmarshal(b["type"], &typ)
	_ = json.Unmarshal(b["subject"], &subject)
	for _, p := range []string{"thread.", "subject.", "link.", "access.", "state.", "instruction.", "autonomy.", "action.", "sealed.", "triage."} {
		if strings.HasPrefix(typ, p) {
			return "/type"
		}
	}
	personal := false
	for _, stem := range personalStems {
		personal = personal || strings.HasPrefix(subject, stem)
	}
	if !personal {
		return "/sealed"
	}
	var key *string
	if json.Unmarshal(b["key"], &key) != nil || key == nil {
		return "/key"
	}
	if strings.Contains(*key, "@") {
		return "/key"
	}
	for _, name := range []string{"title", "body"} {
		if _, ok := b[name]; ok {
			return "/" + name
		}
	}
	var open map[string]json.RawMessage
	if raw, ok := b["open"]; ok {
		if json.Unmarshal(raw, &open) == nil {
			if _, ok := open["description"]; ok {
				return "/open/description"
			}
		}
	}
	// sealed.open is { title }, the title of the thread the write opens, with a plain open and no plain open.title
	// (Signal DEC-281).
	if raw, ok := sealed["open"]; ok {
		var so map[string]json.RawMessage
		var t string
		if json.Unmarshal(raw, &so) != nil || len(so) != 1 || json.Unmarshal(so["title"], &t) != nil || strings.TrimSpace(t) == "" || utf8.RuneCountInString(t) > 300 {
			return "/sealed/open"
		}
		if open == nil {
			return "/open"
		}
		if _, ok := open["title"]; ok {
			return "/open/title"
		}
	}
	if raw, ok := b["metadata"]; ok {
		var meta map[string]json.RawMessage
		if json.Unmarshal(raw, &meta) != nil {
			return "/metadata"
		}
		for name := range meta {
			// references may stay in the envelope: subjects, as plain as the event's own (Signal DEC-284).
			if name != "fields" && name != "native" && name != "references" {
				return "/metadata/" + name
			}
		}
		if r, ok := meta["references"]; ok {
			var refs []string
			if json.Unmarshal(r, &refs) != nil || len(refs) == 0 || len(refs) > 50 {
				return "/metadata/references"
			}
		}
		if n, ok := meta["native"]; ok {
			var native string
			if json.Unmarshal(n, &native) != nil || strings.Contains(native, "@") {
				return "/metadata/native"
			}
		}
		if len(raw) > metadataMax {
			return "/metadata"
		}
	}
	hasState := false
	if raw, ok := b["state"]; ok && string(raw) != "null" {
		var state struct {
			Fields map[string]json.RawMessage `json:"fields"`
		}
		if json.Unmarshal(raw, &state) != nil {
			return "/state"
		}
		hasState = true
		if len(state.Fields) > stateFlagsMax {
			return "/state/fields"
		}
		for name, v := range state.Fields {
			if s := string(bytes.TrimSpace(v)); s != "true" && s != "false" {
				return "/state/fields/" + name
			}
		}
	}
	if raw, ok := sealed["title"]; ok {
		var t string
		if json.Unmarshal(raw, &t) != nil || strings.TrimSpace(t) == "" || utf8.RuneCountInString(t) > 300 {
			return "/sealed/title"
		}
	}
	if raw, ok := sealed["body"]; ok {
		var t string
		if json.Unmarshal(raw, &t) != nil || len(t) > bodyMax {
			return "/sealed/body"
		}
	}
	for _, name := range []string{"metadata", "state"} {
		raw, ok := sealed[name]
		if !ok {
			continue
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil || m == nil || len(raw) > metadataMax {
			return "/sealed/" + name
		}
	}
	if _, ok := sealed["state"]; ok && !hasState {
		return "/sealed/state"
	}
	parts := map[string]json.RawMessage{}
	for k, v := range sealed {
		if k != "level" {
			parts[k] = v
		}
	}
	data, _ := json.Marshal(parts)
	if len(data) > sealedPartsMax {
		return "/sealed"
	}
	return ""
}
