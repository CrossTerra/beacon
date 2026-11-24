package approval

import (
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type Action string

const (
	Merge Action = "github.merge"
	Post  Action = "slack.post"
	Run   Action = "agent.run"

	maxText  = 32_000
	maxNote  = 4_000
	Lifetime = 24 * time.Hour
)

// Invalid identifies a bad field without returning its value.
type Invalid string

func (Invalid) Error() string { return "invalid approval field" }

const (
	repo    = `[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}`
	branch  = `[\x21-\x7e]{1,255}`
	slackID = `[A-Z0-9]{1,32}`
	slackTS = `[0-9]{1,16}\.[0-9]{6}`
	model   = `[A-Za-z0-9_.:@-]{1,80}/[A-Za-z0-9_.:@/-]{1,120}`
	opaque  = `[A-Za-z0-9_.:@/-]{1,200}`
)

type field struct {
	pattern  *regexp.Regexp
	choices  []any
	max      int // >0: length in Unicode scalars
	positive bool
	nullable bool
}

func pattern(s string) field { return field{pattern: regexp.MustCompile("^(?:" + s + ")$")} }

var (
	label       = pattern(opaque)
	contextName = pattern(`[\x21-\x7e]{1,200}`)
	httpsOrigin = pattern(`https://[a-z0-9.-]{1,253}(:[0-9]{1,5})?`)
	statusCode  = pattern(`[a-z][a-z0-9_]{0,63}`)
	mergeNative = pattern(`github:merge:[0-9a-f]{40}`)
	slackNative = pattern(`slack:message:` + slackID + `:` + slackTS)
	threadTS    = field{pattern: pattern(slackTS).pattern, nullable: true}
	positive    = field{positive: true}
)

var paramsSchemas = map[Action]map[string]field{
	Merge: {"repo": pattern(repo), "number": positive, "base": pattern(branch),
		"head": pattern(`[0-9a-f]{40}`), "method": {choices: []any{"merge", "squash", "rebase"}}},
	Post: {"workspace": pattern(slackID), "channel": pattern(slackID), "thread_ts": threadTS, "text": {max: maxText}},
	Run:  {"checkout": pattern(`[a-z0-9][a-z0-9_.-]{0,63}`), "prompt": {max: maxText}, "model": pattern(model)},
}

var destinationSchemas = map[Action]map[string]field{
	Merge: {"repository_id": positive, "pr": positive, "base": pattern(branch)},
	Post:  {"workspace": pattern(slackID), "channel": pattern(slackID), "thread_ts": threadTS},
	Run:   {"checkout_id": label, "branch": pattern(branch)},
}

func (f field) matches(v any) bool {
	if f.positive {
		n, ok := v.(int64)
		return ok && n > 0 && n <= maxSafeInteger
	}
	if v == nil {
		return f.nullable
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	switch {
	case f.max > 0:
		return s != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= f.max
	case f.pattern != nil:
		return f.pattern.MatchString(s)
	default:
		return slices.Contains(f.choices, any(s))
	}
}

func (a Action) ParamsSchema() Object {
	fields := paramsSchemas[a]
	if fields == nil {
		return nil
	}
	props := Object{}
	required := make([]any, 0, len(fields))
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		f := fields[k]
		required = append(required, k)
		switch {
		case f.positive:
			props[k] = Object{"type": "integer", "minimum": int64(1), "maximum": int64(maxSafeInteger)}
		case f.max > 0:
			props[k] = Object{"type": "string", "minLength": int64(1), "maxLength": int64(f.max)}
		case f.pattern != nil:
			var typ any = "string"
			if f.nullable {
				typ = []any{"string", "null"}
			}
			props[k] = Object{"type": typ, "pattern": f.pattern.String()}
		default:
			props[k] = Object{"enum": slices.Clone(f.choices)}
		}
	}
	return Object{"type": "object", "additionalProperties": false, "required": required, "properties": props}
}

// CheckParams also validates a snapshot read back from disk before a handler builds API paths from it.
func (a Action) CheckParams(o Object) error { return validate(o, "/params", paramsSchemas[a]) }

func validate(v any, path string, fields map[string]field) error {
	o, ok := v.(Object)
	if !ok || fields == nil || o == nil {
		return Invalid(path)
	}
	for _, k := range slices.Sorted(maps.Keys(o)) {
		if _, ok := fields[k]; !ok {
			return Invalid(path + "/" + k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		if v, ok := o[k]; !ok || !fields[k].matches(v) {
			return Invalid(path + "/" + k)
		}
	}
	return nil
}

type Request struct {
	Type    Action `json:"type"`
	Params  Object `json:"params"`
	Reason  string `json:"reason"`
	Agent   string `json:"agent"`
	Session string `json:"session,omitempty"`
	Context string `json:"context,omitempty"` // the proposer's words, never a routing input
}

// CheckKeys reports the first unexpected key, using a JSON pointer. Use only in protocol replies, not logs.
func CheckKeys(o Object, path string, allowed ...string) error {
	for _, k := range slices.Sorted(maps.Keys(o)) {
		if !slices.Contains(allowed, k) {
			return Invalid(path + "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(k))
		}
	}
	return nil
}

func ParseRequest(o Object) (Request, error) {
	if err := CheckKeys(o, "", "op", "type", "params", "reason", "agent", "context"); err != nil {
		return Request{}, err
	}
	r := Request{Type: Action(text(o, "type")), Reason: text(o, "reason")}
	if paramsSchemas[r.Type] == nil {
		return Request{}, Invalid("/type")
	}
	if err := validate(o["params"], "/params", paramsSchemas[r.Type]); err != nil {
		return Request{}, err
	}
	r.Params = o["params"].(Object)
	a, ok := o["agent"].(Object)
	if !ok {
		return Request{}, Invalid("/agent")
	}
	if err := CheckKeys(a, "/agent", "name", "session"); err != nil {
		return Request{}, err
	}
	r.Agent = text(a, "name")
	if v, ok := a["session"]; ok {
		if !label.matches(v) {
			return Request{}, Invalid("/agent/session")
		}
		r.Session = v.(string)
	}
	if v, ok := o["context"]; ok {
		c, ok := v.(Object)
		if !ok {
			return Request{}, Invalid("/context")
		}
		if err := CheckKeys(c, "/context", "subject"); err != nil {
			return Request{}, err
		}
		if !contextName.matches(c["subject"]) {
			return Request{}, Invalid("/context/subject")
		}
		r.Context = text(c, "subject")
	}
	return r, r.check()
}

func (r Request) check() error {
	if paramsSchemas[r.Type] == nil {
		return Invalid("/type")
	}
	if err := validate(r.Params, "/params", paramsSchemas[r.Type]); err != nil {
		return err
	}
	for _, f := range []struct {
		path string
		ok   bool
	}{
		{"/reason", (field{max: maxNote}).matches(r.Reason)}, {"/agent/name", label.matches(r.Agent)},
		{"/agent/session", r.Session == "" || label.matches(r.Session)},
		{"/context/subject", r.Context == "" || contextName.matches(r.Context)},
	} {
		if !f.ok {
			return Invalid(f.path)
		}
	}
	return nil
}

func text(o Object, key string) string { s, _ := o[key].(string); return s }

// PeerLabel records what the kernel said about the connection. PID and path are local-only labels;
// only a successful lookup by the supervisor can give the peer a RunID.
type PeerLabel struct {
	PID        int    `json:"pid,omitempty"`
	Executable string `json:"executable,omitempty"`
	RunID      string `json:"run_id,omitempty"`
}

type Proposal struct {
	Request
	ID         string          `json:"id"`
	Salt       string          `json:"salt"`
	ParamsHash string          `json:"params_hash"`
	Peer       PeerLabel       `json:"peer"`
	RunnerID   string          `json:"runner_id,omitempty"` // the run being proposed, not the proposer
	Slack      *SlackPlacement `json:"slack_placement,omitempty"`
	ProposedAt time.Time       `json:"proposed_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
	Status     string          `json:"status"`
	Version    int64           `json:"version"`
}

type Binding struct {
	Origin      string `json:"origin"` // empty for a local run (null in the hash and record)
	Account     string `json:"account"`
	Destination Object `json:"destination"`
}

// Resolved is a binding verified by a handler and the local-only facts shown beside it.
// Only the binding enters the approval hash; display labels never enter a Signal record.
type Resolved struct {
	Binding
	AccountLabel, DestinationLabel string
	Facts                          Object
	Pin                            io.Closer // live checkout capability, owned by the reviewer; never serialized
}

// Refusal carries a fixed reason code and optional display-only detail chosen by a handler.
type Refusal struct{ Code, Detail string }

func (r Refusal) Error() string { return r.Code }

type Frozen struct {
	Binding
	ID            string `json:"id"`
	Version       int64  `json:"version"`
	Params        Object `json:"params"`
	OriginalModel string `json:"original_model,omitempty"`
	ParamsHash    string `json:"approved_params_hash"`
	Hash          string `json:"approved_hash"`
}

// Freeze captures an exact snapshot from the durable proposal and a trusted resolver's binding.
// It is not authorization. Only a run's model may differ from its proposed value.
func Freeze(p Proposal, b Binding, selectedModel string) (Frozen, error) {
	if p.Status != "pending" {
		return Frozen{}, ErrTransition
	}
	if !label.matches(b.Account) {
		return Frozen{}, Invalid("/account")
	}
	if p.Type == Run && b.Origin != "" || p.Type != Run && !httpsOrigin.matches(b.Origin) {
		return Frozen{}, Invalid("/origin")
	}
	if err := validate(b.Destination, "/destination", destinationSchemas[p.Type]); err != nil {
		return Frozen{}, err
	}
	b.Destination = maps.Clone(b.Destination)
	f := Frozen{Binding: b, ID: p.ID, Version: p.Version, Params: maps.Clone(p.Params)}
	if err := validate(f.Params, "/params", paramsSchemas[p.Type]); err != nil {
		return Frozen{}, err
	}
	if selectedModel != "" {
		if p.Type != Run || !paramsSchemas[Run]["model"].matches(selectedModel) {
			return Frozen{}, Invalid("/params/model")
		}
		if current := text(p.Params, "model"); current != selectedModel {
			f.OriginalModel = current
			f.Params["model"] = selectedModel
		}
	}
	switch p.Type {
	case Merge:
		if b.Destination["pr"] != f.Params["number"] || b.Destination["base"] != f.Params["base"] {
			return Frozen{}, Invalid("/destination")
		}
	case Post:
		if p.Slack != nil && p.Slack.Account != b.Account {
			return Frozen{}, Invalid("/account")
		}
		for _, k := range []string{"workspace", "channel", "thread_ts"} {
			if b.Destination[k] != f.Params[k] {
				return Frozen{}, Invalid("/destination/" + k)
			}
		}
	}
	var err error
	f.ParamsHash, err = paramsDigest(p.Type, f.Params, p.Salt)
	if err != nil {
		return Frozen{}, err
	}
	f.Hash, err = digest(Object{"id": p.ID, "type": string(p.Type), "params": f.Params, "origin": originValue(b.Origin),
		"account": b.Account, "destination": b.Destination, "salt": p.Salt})
	return f, err
}

func originValue(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func paramsDigest(action Action, params Object, salt string) (string, error) {
	return digest(Object{"type": string(action), "params": params, "salt": salt})
}
