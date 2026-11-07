// Package source defines the item every source yields and the interface a source implements.
package source

import (
	"context"
	"fmt"
	"regexp"
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
	Type       string    `json:"type"`
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
}

// Source yields items. It holds its own credentials and cursor.
type Source interface {
	// Name is the source name items carry.
	Name() string
	// Fields is the source's fixed field list (DEC-6).
	Fields() []string
	// Hosts are the hosts the source contacts, as host or host:port. Every one is listed by `beacon egress`.
	Hosts() []string
	// Collect returns the items available now.
	Collect(ctx context.Context) ([]Item, error)
}

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	typePattern = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*$`)
	printable   = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)
)

// ValidName reports whether s can be a source name.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// Validate checks an item against its source's field list and Signal's limits on subjects, keys and types.
func Validate(it Item, fields []string) error {
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
	return nil
}
