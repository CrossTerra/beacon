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
	// CloseSiblingsBefore, on an own act, also closes earlier asks in the same conversation that DEC-10 makes their own
	// subjects: an open ask on a sibling subject (one that differs from Subject only in the last part) that opened that
	// subject (the ask's key ends in that last part, so it is not a reply inside it) and whose last part is before
	// this value. A top-level reply in a Slack direct message sets it to its own ts, for the asks recorded before
	// direct messages had one subject a day. With CloseOnly, CloseOnly narrows the asks on Subject and this adds the
	// siblings'.
	CloseSiblingsBefore string `json:"close_siblings_before,omitempty"`
	// CloseOnly, when set, narrows what this item may close to these ask keys on its subject. A Slack deletion names the
	// ask the deleted message opened, so a close_by_others rule closes that ask and not the others in its thread. The
	// person's own message in a Slack direct message names the asks it answers on its day's subject, or only its own
	// key, which is never an ask, when it answers none.
	CloseOnly []string `json:"close_only,omitempty"`
	// Native is the source's own opaque id for the message or object: a Gmail message id, a Slack channel id and ts,
	// a Drive file id, a calendar event id. Never a Message-ID header or anything that names a domain or a person. A
	// sealed event carries it as metadata.native (DEC-31, Signal DEC-264).
	Native string `json:"native,omitempty"`
	// Local is what the item gives the raise to owner (DEC-33, DEC-35). It is matched on the machine only: it is not
	// a field, it is never projected, stored or logged.
	Local *Local `json:"-"`
}

// Local is text, addresses and labels a source read for an item, matched against seal.owner on the machine. A field
// a rule does not send is still matched. Nothing in it leaves the machine or is written to the state directory.
type Local struct {
	// Text is matched against seal.owner words, by where it came from ("subject", "body", "thread_root").
	Text map[string]string `json:"text,omitempty"`
	// Addresses are lowercased addresses matched against seal.owner senders, by where they came from.
	Addresses map[string][]string `json:"addresses,omitempty"`
	// Labels are label names matched against seal.owner labels.
	Labels []string `json:"labels,omitempty"`
	// Meeting is the calendar subject of the meeting a note or transcript belongs to (DEC-35).
	Meeting string `json:"meeting,omitempty"`
}

var decimalPattern = regexp.MustCompile(`^[0-9]{1,20}(\.[0-9]{1,20})?$`)

// DecimalBefore reports whether a is less than b, both decimal numbers ("1794132000.000100"). Anything else is not
// before anything, so a malformed bound closes nothing.
func DecimalBefore(a, b string) bool {
	if !decimalPattern.MatchString(a) || !decimalPattern.MatchString(b) {
		return false
	}
	ai, af, _ := strings.Cut(a, ".")
	bi, bf, _ := strings.Cut(b, ".")
	ai, bi = strings.TrimLeft(ai, "0"), strings.TrimLeft(bi, "0")
	if len(ai) != len(bi) {
		return len(ai) < len(bi)
	}
	if ai != bi {
		return ai < bi
	}
	for len(af) < len(bf) {
		af += "0"
	}
	for len(bf) < len(af) {
		bf += "0"
	}
	return af < bf
}

// ClosesSibling reports whether an own act on subject with CloseSiblingsBefore before closes the open ask askKey on
// askSubject: a sibling subject other than its own, opened by that ask, before the bound.
func ClosesSibling(subject, before, askSubject, askKey string) bool {
	if before == "" || askSubject == subject || !IsSibling(subject, askSubject) {
		return false
	}
	last := askSubject[len(SiblingPrefix(askSubject)):]
	return strings.HasSuffix(askKey, ":"+last) && DecimalBefore(last, before)
}

// SiblingPrefix is the part of subject that its siblings share: everything up to and including the last colon. It
// is "" when the subject has no colon.
func SiblingPrefix(subject string) string {
	i := strings.LastIndex(subject, ":")
	if i < 0 {
		return ""
	}
	return subject[:i+1]
}

// IsSibling reports whether other differs from subject only in the last part. A subject is its own sibling.
func IsSibling(subject, other string) bool {
	p := SiblingPrefix(subject)
	if p == "" || !strings.HasPrefix(other, p) {
		return false
	}
	return !strings.Contains(other[len(p):], ":")
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

// PersonalStems are subject prefixes Signal keeps private only when that prefix is restricted (DEC-10). They are
// fixed, not taken from the enabled sources: a body already in the outbox is still personal after its source is
// removed from the config. slack:dm: covers Slack's direct and group direct messages; its channel subjects are not
// personal.
var PersonalStems = []string{"gmail:", "calendar:", "drive:", "slack:dm:"}

// PersonalStem is the longest personal stem subject starts with, or "" when it is not personal.
func PersonalStem(subject string) string {
	found := ""
	for _, stem := range PersonalStems {
		if strings.HasPrefix(subject, stem) && len(stem) > len(found) {
			found = stem
		}
	}
	return found
}

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
	// Cut is true when a read stopped at its limit (an open-state read, or a gap re-read), so older items in its
	// window were not read. The cursor still moves; the runner reports it.
	Cut bool
	// Partial is true when Next is a resume point, not a completed cursor. The read stopped at a rate limit and
	// the next pass continues from Next. The runner stores it even on a dry run. It is not a completed collect.
	Partial bool
	// ClearResume asks the runner to drop a resume cursor stored earlier, because this collect finished that
	// read and is not committing a completed cursor (a dry run, or a first run not yet accepted).
	ClearResume bool
	// Limit is the rate-limit answer that paused a Partial read, when the source knows it. The runner logs it.
	Limit *RateLimit
	// Warning is a fixed code for something the pass did without (WarnNamesScope, WarnNamesFailed). The items are
	// still complete and the cursor moves; the runner logs and prints it once.
	Warning string
}

// ErrNoTitle is a thread a source cannot name: its conversation is gone or unreadable. Asking again will not help.
var ErrNoTitle = errors.New("source: that conversation cannot be named")

// Warnings a collect may carry. Fixed codes, never text.
const (
	// WarnNamesScope is a token without the scopes display names need, so items carry no names or titles.
	WarnNamesScope = "names_scope"
	// WarnNamesFailed is a name lookup that failed this pass (a rate limit, an error), so some items carry no names
	// or titles.
	WarnNamesFailed = "names_failed"
)

// RateLimit is what an API said when it refused a call for its rate limit. It holds no body, no URL and no item
// data: a status, the API's own reason code from a fixed list (or ""), and the wait it asked for.
type RateLimit struct {
	// Status is the HTTP status: 429, or 403 with a rate-limit reason.
	Status int
	// Reason is a fixed reason code the source recognised (Google's rateLimitExceeded, userRateLimitExceeded, ...),
	// or "" when the answer had none it knows. Never free text.
	Reason string
	// RetryAfter is the wait the answer asked for. Negative means it named none.
	RetryAfter time.Duration
	// Units is what the source had spent of its own quota budget in the last minute, when it keeps one. Zero
	// otherwise.
	Units int
}

// RateLimitError is ErrRateLimited with what the API said. errors.Is(err, ErrRateLimited) holds for it.
type RateLimitError struct{ RateLimit }

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }

// Is makes a RateLimitError match ErrRateLimited.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// LimitOf returns the RateLimit an error carries, or nil.
func LimitOf(err error) *RateLimit {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		l := rl.RateLimit
		return &l
	}
	return nil
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
	for _, k := range it.CloseOnly {
		if !printable.MatchString(k) || !strings.HasPrefix(k, it.Source+":") {
			return fmt.Errorf("close_only names a key that is not this source's")
		}
	}
	if it.Native != "" && (!printable.MatchString(it.Native) || strings.Contains(it.Native, "@")) {
		return fmt.Errorf("native is not an opaque id of 1 to 200 printable characters without @")
	}
	if it.CloseSiblingsBefore != "" {
		// Siblings share everything but the last part, so the prefix must name one conversation, not a stem or a
		// person: closing every ask under "slack:dm:" or "slack:dm:<person>:" would answer conversations the act is
		// not in. slack:dm:<person>:<channel>: has four parts.
		if !it.Own {
			return fmt.Errorf("close_siblings_before is for own acts")
		}
		if strings.Count(SiblingPrefix(it.Subject), ":") < 4 {
			return fmt.Errorf("close_siblings_before needs a subject with at least four parts before the last")
		}
		if !decimalPattern.MatchString(it.CloseSiblingsBefore) {
			return fmt.Errorf("close_siblings_before is not a decimal bound")
		}
	}
	return nil
}
