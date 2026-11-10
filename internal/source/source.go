// Package source defines the item every source yields and the interface a source implements.
package source

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Item is one change of state in a source (DEC-9, DEC-18).
type Item struct {
	// Source is the source's name, as rules and keys use it ("github", "fixture").
	Source string `json:"source"`
	// Subject is the source's stable id for the object (DEC-10).
	Subject string `json:"subject"`
	// Key is "<source>:<native id>:<version>" (DEC-9). The same change always yields the same key.
	Key string `json:"key"`
	// Type is the kind of change, without the source prefix ("review_requested").
	Type string `json:"type"`
	// EventTitle is a short description of this change, used as the event title. It is not a source field and not
	// the object's title. Every service source sets it. Empty means the projection uses the title field for the event
	// title as well, which is how a fixture that omits it behaves.
	EventTitle string    `json:"event_title,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	// Actor is the identity of whoever caused the change ("github:user:dana").
	Actor string `json:"actor"`
	// Own is true when the person Beacon serves caused the change (DEC-19).
	Own bool `json:"own"`
	// Addressed is true when the person was asked or mentioned.
	Addressed bool `json:"addressed"`
	// Fields holds the source's fixed fields. A field not in the source's list is never set.
	Fields map[string]string `json:"fields"`
	// References are explicit references to other subjects (DEC-17).
	References []string `json:"references,omitempty"`
	// DueAt is the source's own due time, when it has one (DEC-21).
	DueAt *time.Time `json:"due_at,omitempty"`
	// State is the object's current state, when the source returned it (DEC-28). Only names in the source's state
	// field list.
	State map[string]string `json:"state,omitempty"`
	// ObservedAt is when the source returned State.
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	// Version is the source's version marker for this change: the last part of Key (DEC-9).
	Version string `json:"version,omitempty"`
}

// Lookback is how far before a committed cursor a pass reads again. Indexing lag and clock skew land in this
// window; keys make the overlap harmless.
const Lookback = 15 * time.Minute

// DateSkew is how far a server Date may be ahead of the local clock before the pass fails. A Date further ahead
// would jump the cursor.
const DateSkew = time.Hour

// Names a source may not use in its state field list: the state snapshot carries them itself (DEC-28).
const (
	StateObservedAt = "observed_at"
	StateVersion    = "version"
)

// Collected is one collect. Next is the cursor to store after every item of this pass is decided. A nil Next
// leaves the committed cursor where it is. Dropped is items the source refused and did not return; the runner
// logs the count and does not fail the pass.
type Collected struct {
	Items   []Item
	Next    []byte
	Dropped int
	// Gap is true when the source could not read every change since the cursor (Gmail's history expired) and
	// re-read what it could. The runner reports it.
	Gap bool
	// GapCut is true when that re-read stopped at its limit, so older changes in the gap were not read.
	GapCut bool
}

// A source returns these when a pass must not move the cursor. Their text is fixed: it names no item, token or URL.
var (
	// ErrRateLimited means the API's rate limit ended the pass. The caller resumes from the committed cursor.
	ErrRateLimited = errors.New("source: the API rate limit ends this pass; the cursor is not moved")
	// ErrUnauthorized means the source refused the token.
	ErrUnauthorized = errors.New("source: the token was refused")
	// ErrScope means a request would have left the configured repositories, projects or host. It is not sent.
	ErrScope = errors.New("source: the request is outside the configured repositories, projects or host")
	// ErrIdentity means person.identities for this source does not match the token's user.
	ErrIdentity = errors.New("source: person.identities does not match the token's user")
)

// Source yields items. It holds its own credentials. The runner holds its cursor and commits it only after deciding
// every item of a pass.
type Source interface {
	// Name is the source name items carry.
	Name() string
	// Fields is the source's fixed field list (DEC-6).
	Fields() []string
	// StateFields is the source's fixed list of state fields (DEC-28).
	StateFields() []string
	// Hosts are the hosts the source contacts, as host:port. Every one is listed by `beacon egress`.
	Hosts() []string
	// Collect returns the items available since cursor. A nil cursor means the source has no committed cursor, and
	// it reads open state, not history (DEC-23). The same cursor yields the same keys.
	Collect(ctx context.Context, cursor []byte) (Collected, error)
}

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	typePattern = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*$`)
	printable   = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)
)

// ValidName reports whether s can be a source name.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// ValidStateFields checks a source's state field list.
func ValidStateFields(names []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" || n == StateObservedAt || n == StateVersion {
			return fmt.Errorf("state field %q is reserved or empty", n)
		}
		if seen[n] {
			return fmt.Errorf("state field %q is listed twice", n)
		}
		seen[n] = true
	}
	return nil
}

// Validate checks an item against its source's field lists and Signal's limits on subjects, keys and types.
func Validate(it Item, fields, stateFields []string) error {
	if !ValidName(it.Source) {
		return fmt.Errorf("source name %q is invalid", it.Source)
	}
	if !typePattern.MatchString(it.Type) {
		return fmt.Errorf("type %q is invalid", it.Type)
	}
	if !printable.MatchString(it.Subject) {
		return fmt.Errorf("subject is not 1 to 200 printable characters")
	}
	if !printable.MatchString(it.Key) {
		return fmt.Errorf("key is not 1 to 200 printable characters")
	}
	if len(it.Key) < len(it.Source)+1 || it.Key[:len(it.Source)+1] != it.Source+":" {
		return fmt.Errorf("key does not start with %q", it.Source+":")
	}
	if it.OccurredAt.IsZero() {
		return fmt.Errorf("occurred_at is missing")
	}
	allowed := make(map[string]bool, len(fields))
	for _, f := range fields {
		allowed[f] = true
	}
	for name := range it.Fields {
		if !allowed[name] {
			return fmt.Errorf("field %q is not in the source's field list", name)
		}
	}
	allowedState := make(map[string]bool, len(stateFields))
	for _, f := range stateFields {
		allowedState[f] = true
	}
	for name := range it.State {
		if !allowedState[name] {
			return fmt.Errorf("state field %q is not in the source's state field list", name)
		}
	}
	if it.Version != "" && !strings.HasSuffix(it.Key, ":"+it.Version) {
		return fmt.Errorf("version is not the last part of the key")
	}
	if len(it.State) > 0 && (it.ObservedAt == nil || it.ObservedAt.IsZero() || it.Version == "") {
		return fmt.Errorf("a state snapshot needs observed_at and version")
	}
	return nil
}
