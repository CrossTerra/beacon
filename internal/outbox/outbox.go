// Package outbox is the local, append-only record of every request body Beacon sends (DEC-8).
//
// The file is JSON lines. A body is appended, and synced to disk, before it is sent. Each later change of state is a
// new line for the same key; the latest line is the key's state. A body is never rewritten: a retry sends the stored
// bytes.
package outbox

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// FileName is the outbox file inside the state directory.
const FileName = "outbox.jsonl"

// State is where a key stands.
type State string

const (
	// Pending was appended and not yet answered by Signal. It is sent, with its stored bytes, on the next run.
	Pending State = "pending"
	// Sent was written by Signal (201).
	Sent State = "sent"
	// Replayed was already in Signal from an earlier send of the same bytes (200, replayed).
	Replayed State = "replayed"
	// Rejected was refused by Signal. It is reported and never sent again.
	Rejected State = "rejected"
)

// Kind says what a body is for.
type Kind string

const (
	// Event is an item a rule sent.
	Event Kind = "event"
	// Lease is a lease claim (DEC-25).
	Lease Kind = "lease"
)

// record is one line of the file.
type record struct {
	Key     string    `json:"key"`
	State   State     `json:"state"`
	At      time.Time `json:"at"`
	Kind    Kind      `json:"kind,omitempty"`
	Subject string    `json:"subject,omitempty"`
	Rule    string    `json:"rule,omitempty"`
	Body    string    `json:"body,omitempty"`
	SHA256  string    `json:"sha256,omitempty"`
	EventID string    `json:"event_id,omitempty"`
	Status  int       `json:"status,omitempty"`
	Code    string    `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
}

// Entry is a key's current state.
type Entry struct {
	Key     string
	Kind    Kind
	Subject string
	Rule    string
	// Body is the exact bytes sent, or to be sent.
	Body       []byte
	State      State
	EventID    string
	Status     int
	Code       string
	Message    string
	AppendedAt time.Time
	UpdatedAt  time.Time

	// Asks is true when the body asks someone; Closes lists the keys it closes.
	Asks   bool
	Closes []string
}

// Outbox is the outbox of one state directory. A writer holds an exclusive lock on the file.
type Outbox struct {
	path    string
	file    *os.File
	entries map[string]*Entry
	order   []string
	now     func() time.Time
}

// ErrExists is returned when a key is appended twice.
var ErrExists = errors.New("outbox: key already appended")

// ErrLocked is returned when another process holds the outbox.
var ErrLocked = errors.New("outbox: another beacon process holds the outbox")

// Open opens the outbox in dir for writing, creating it if needed, and takes an exclusive lock on it. A line torn
// by a crash while it was written is cut off: it was never synced, so its body was never sent.
func Open(dir string, now func() time.Time) (*Outbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, FileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	o := &Outbox{path: path, file: f, now: now}
	good, err := o.load(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Truncate(good); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return o, nil
}

// Read loads the outbox in dir without locking it, for display.
func Read(dir string) (*Outbox, error) {
	f, err := os.Open(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return &Outbox{entries: map[string]*Entry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	o := &Outbox{path: f.Name()}
	if _, err := o.load(f); err != nil {
		return nil, err
	}
	return o, nil
}

// load reads every complete line and returns the offset after the last one.
func (o *Outbox) load(r io.Reader) (int64, error) {
	o.entries = map[string]*Entry{}
	o.order = nil
	br := bufio.NewReader(r)
	var offset int64
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			// A last line without its newline was torn while it was written.
			return offset, nil
		}
		if err != nil {
			return 0, err
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			if _, peekErr := br.Peek(1); errors.Is(peekErr, io.EOF) {
				return offset, nil
			}
			return 0, fmt.Errorf("outbox: %s line %d: %w", o.path, n, err)
		}
		if err := o.apply(rec); err != nil {
			return 0, fmt.Errorf("outbox: %s line %d: %w", o.path, n, err)
		}
		offset += int64(len(line))
	}
}

func (o *Outbox) apply(rec record) error {
	e, ok := o.entries[rec.Key]
	if rec.State == Pending && rec.Body != "" {
		if ok {
			return fmt.Errorf("key %q appended twice", rec.Key)
		}
		sum := sha256.Sum256([]byte(rec.Body))
		if hex.EncodeToString(sum[:]) != rec.SHA256 {
			return fmt.Errorf("key %q: body does not match its sha256", rec.Key)
		}
		e = &Entry{Key: rec.Key, Kind: rec.Kind, Subject: rec.Subject, Rule: rec.Rule, Body: []byte(rec.Body),
			State: Pending, AppendedAt: rec.At, UpdatedAt: rec.At}
		e.Asks, e.Closes = inspect(e.Body)
		o.entries[rec.Key] = e
		o.order = append(o.order, rec.Key)
		return nil
	}
	if !ok {
		return fmt.Errorf("key %q changes state before it was appended", rec.Key)
	}
	switch rec.State {
	case Sent, Replayed, Rejected:
	default:
		return fmt.Errorf("key %q: unknown state %q", rec.Key, rec.State)
	}
	if e.State != Pending {
		return fmt.Errorf("key %q: state %s after %s", rec.Key, rec.State, e.State)
	}
	e.State, e.EventID, e.Status, e.Code, e.Message, e.UpdatedAt = rec.State, rec.EventID, rec.Status, rec.Code, rec.Message, rec.At
	return nil
}

func inspect(body []byte) (asks bool, closes []string) {
	var b struct {
		To     []string `json:"to"`
		Closes []struct {
			Key string `json:"key"`
		} `json:"closes"`
	}
	if json.Unmarshal(body, &b) != nil {
		return false, nil
	}
	for _, c := range b.Closes {
		closes = append(closes, c.Key)
	}
	return len(b.To) > 0, closes
}

func (o *Outbox) write(rec record) error {
	if o.file == nil {
		return errors.New("outbox: opened read-only")
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := o.file.Write(line); err != nil {
		return err
	}
	return o.file.Sync()
}

// Append records body as pending under key and syncs it to disk. Only then may it be sent.
func (o *Outbox) Append(kind Kind, key, subject, rule string, body []byte) error {
	if _, ok := o.entries[key]; ok {
		return ErrExists
	}
	if !json.Valid(body) {
		return errors.New("outbox: body is not JSON")
	}
	sum := sha256.Sum256(body)
	rec := record{Key: key, State: Pending, At: o.now().UTC(), Kind: kind, Subject: subject, Rule: rule,
		Body: string(body), SHA256: hex.EncodeToString(sum[:])}
	if err := o.write(rec); err != nil {
		return err
	}
	return o.apply(rec)
}

// Mark records Signal's answer for a pending key.
func (o *Outbox) Mark(key string, state State, eventID string, status int, code, message string) error {
	e, ok := o.entries[key]
	if !ok {
		return fmt.Errorf("outbox: no key %q", key)
	}
	if e.State != Pending {
		return fmt.Errorf("outbox: key %q is %s, not pending", key, e.State)
	}
	rec := record{Key: key, State: state, At: o.now().UTC(), EventID: eventID, Status: status, Code: code, Message: message}
	if err := o.write(rec); err != nil {
		return err
	}
	return o.apply(rec)
}

// Close releases the lock.
func (o *Outbox) Close() error {
	if o.file == nil {
		return nil
	}
	err := o.file.Close()
	o.file = nil
	return err
}

// Get returns a key's entry.
func (o *Outbox) Get(key string) (Entry, bool) {
	e, ok := o.entries[key]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// Entries returns every entry in append order.
func (o *Outbox) Entries() []Entry {
	out := make([]Entry, 0, len(o.order))
	for _, k := range o.order {
		out = append(out, *o.entries[k])
	}
	return out
}

// Pending returns the pending entries in append order.
func (o *Outbox) Pending() []Entry {
	var out []Entry
	for _, k := range o.order {
		if e := o.entries[k]; e.State == Pending {
			out = append(out, *e)
		}
	}
	return out
}

// OpenAsks returns the keys of event bodies on subject that ask someone and that no other body closes, oldest
// first. Rejected bodies count for neither.
func (o *Outbox) OpenAsks(subject string) []string {
	closed := map[string]bool{}
	for _, k := range o.order {
		e := o.entries[k]
		if e.State == Rejected {
			continue
		}
		for _, c := range e.Closes {
			closed[c] = true
		}
	}
	var out []string
	for _, k := range o.order {
		e := o.entries[k]
		if e.Kind == Event && e.Subject == subject && e.Asks && e.State != Rejected && !closed[k] {
			out = append(out, k)
		}
	}
	return out
}

// Equal reports whether the stored body for key is exactly body.
func (o *Outbox) Equal(key string, body []byte) bool {
	e, ok := o.entries[key]
	return ok && bytes.Equal(e.Body, body)
}
