// Package outbox is the local, append-only record of every request body Beacon sends (DEC-8).
//
// The file is JSON lines. A body is appended, and synced to disk, before it is sent. Each later change of state is a
// new line for the same key; the latest line is the key's state. A pending retry sends the stored bytes. A body Signal
// accepted is never rewritten. A body Signal refused with 409 is never retried. Any other refusal may be re-projected
// and appended again, only by an explicit retry, and only when the decided item was stored with the append.
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

	"github.com/CrossTerra/beacon/internal/source"
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
	// Rejected was refused by Signal. beacon run does not send it again. A 409 stays final. Any other refusal may be
	// re-projected by an explicit retry when the decided item was stored, and only if the current rules still send it.
	Rejected State = "rejected"
	// Superseded is a refusal the current rules no longer send. It is not sent. Code is hold or never.
	Superseded State = "superseded"
	// Expired is a lease claim whose period ended before Signal answered it. It is never sent again.
	Expired State = "expired"
)

// Tiers recorded on a superseded refusal. They are fixed codes, not item content.
const (
	TierHold  = "hold"
	TierNever = "never"
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
	// Decision is the decided item and rule snapshot, as JSON. It stays on the machine and is never sent.
	Decision    string `json:"decision,omitempty"`
	DecisionSHA string `json:"decision_sha256,omitempty"`
	// Retry is a new body for a rejected event Signal did not store. It is not a second append.
	Retry   bool   `json:"retry,omitempty"`
	EventID string `json:"event_id,omitempty"`
	Status  int    `json:"status,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Entry is a key's current state.
type Entry struct {
	Key     string
	Kind    Kind
	Subject string
	Rule    string
	// Body is the exact bytes sent, or to be sent.
	Body []byte
	// Decision is the decided item and rule snapshot, when one was stored with the append. It is not part of the
	// request and is not shown by the outbox listing.
	Decision   []byte
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

// ErrNotRetryable is returned when a key cannot be requeued: it is not a rejected event, or Signal answered 409.
var ErrNotRetryable = errors.New("outbox: that entry is not retried")

// Open opens the outbox in dir for writing, creating it if needed, and takes an exclusive lock on it. A line torn
// by a crash while it was written is cut off: it was never synced, so its body was never sent.
func Open(dir string, now func() time.Time) (*Outbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, FileName)
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if created {
		// The new file's directory entry must be durable before any body in it counts as appended.
		if err := SyncDir(dir); err != nil {
			f.Close()
			return nil, err
		}
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
		sum := sha256.Sum256([]byte(rec.Body))
		if hex.EncodeToString(sum[:]) != rec.SHA256 {
			return fmt.Errorf("key %q: body does not match its sha256", rec.Key)
		}
		if rec.Decision != "" {
			dsum := sha256.Sum256([]byte(rec.Decision))
			if hex.EncodeToString(dsum[:]) != rec.DecisionSHA {
				return fmt.Errorf("key %q: decision does not match its sha256", rec.Key)
			}
		}
		if ok {
			if !rec.Retry {
				return fmt.Errorf("key %q appended twice", rec.Key)
			}
			return applyRetry(e, rec)
		}
		if rec.Retry {
			return fmt.Errorf("key %q retries before it was appended", rec.Key)
		}
		e = &Entry{Key: rec.Key, Kind: rec.Kind, Subject: rec.Subject, Rule: rec.Rule, Body: []byte(rec.Body),
			Decision: []byte(rec.Decision), State: Pending, AppendedAt: rec.At, UpdatedAt: rec.At}
		e.Asks, e.Closes = inspect(e.Body)
		o.entries[rec.Key] = e
		o.order = append(o.order, rec.Key)
		return nil
	}
	if rec.State == Superseded {
		if !ok {
			return fmt.Errorf("key %q superseded before it was appended", rec.Key)
		}
		if e.Kind != Event || e.State != Rejected || conflict(e) {
			return fmt.Errorf("key %q: cannot supersede %s", rec.Key, e.State)
		}
		if rec.Code != TierHold && rec.Code != TierNever {
			return fmt.Errorf("key %q: superseded tier %q", rec.Key, rec.Code)
		}
		e.State, e.Code, e.UpdatedAt = Superseded, rec.Code, rec.At
		return nil
	}
	if !ok {
		return fmt.Errorf("key %q changes state before it was appended", rec.Key)
	}
	switch rec.State {
	case Sent, Replayed, Rejected:
	case Expired:
		if e.Kind != Lease {
			return fmt.Errorf("key %q: only a lease claim expires", rec.Key)
		}
	default:
		return fmt.Errorf("key %q: unknown state %q", rec.Key, rec.State)
	}
	if e.State != Pending {
		return fmt.Errorf("key %q: state %s after %s", rec.Key, rec.State, e.State)
	}
	e.State, e.EventID, e.Status, e.Code, e.Message, e.UpdatedAt = rec.State, rec.EventID, rec.Status, rec.Code, rec.Message, rec.At
	return nil
}

// applyRetry replaces the body of a rejected event Signal did not store and sets it pending. The original append
// line, including its decision, stays. A 409 is not retried: Signal already holds another request under that key.
func applyRetry(e *Entry, rec record) error {
	if e.Kind != Event {
		return fmt.Errorf("key %q: only an event is retried", rec.Key)
	}
	if e.State != Rejected {
		return fmt.Errorf("key %q: retry after %s", rec.Key, e.State)
	}
	if conflict(e) {
		return fmt.Errorf("key %q: a conflict is not retried", rec.Key)
	}
	if rec.Decision != "" {
		if len(e.Decision) != 0 || e.Status != 400 || conflict(e) {
			return fmt.Errorf("key %q: a new attempt needs a 400 with no stored item", rec.Key)
		}
		e.Decision = []byte(rec.Decision)
	}
	if rec.Rule != "" {
		e.Rule = rec.Rule
	}
	if rec.Subject != "" {
		e.Subject = rec.Subject
	}
	e.Body = []byte(rec.Body)
	e.Asks, e.Closes = inspect(e.Body)
	e.State = Pending
	e.EventID, e.Status, e.Code, e.Message = "", 0, "", ""
	e.UpdatedAt = rec.At
	return nil
}

// conflict reports a 409, including idempotency_conflict. Those entries are final.
func conflict(e *Entry) bool {
	return e.Status == 409 || e.Code == "idempotency_conflict"
}

// Undecided reports whether this entry must not block a later decision. A 400 with no stored item is one Signal
// never stored, so the same key may be decided again. Anything Signal accepted, a 409, and a refusal that still
// has its item, count as decided.
func (e Entry) Undecided() bool {
	return e.Kind == Event && e.State == Rejected && e.Status == 400 && !conflict(&e) && len(e.Decision) == 0
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
	return o.append(kind, key, subject, rule, body, nil)
}

// AppendDecided records body and the decided item it was projected from. The decision stays on the machine so a
// later refusal that Signal did not store can be projected again. It is not sent.
func (o *Outbox) AppendDecided(kind Kind, key, subject, rule string, body, decision []byte) error {
	if len(decision) == 0 || !json.Valid(decision) {
		return errors.New("outbox: decision is not JSON")
	}
	return o.append(kind, key, subject, rule, body, decision)
}

func (o *Outbox) append(kind Kind, key, subject, rule string, body, decision []byte) error {
	if _, ok := o.entries[key]; ok {
		return ErrExists
	}
	if !json.Valid(body) {
		return errors.New("outbox: body is not JSON")
	}
	sum := sha256.Sum256(body)
	rec := record{Key: key, State: Pending, At: o.now().UTC(), Kind: kind, Subject: subject, Rule: rule,
		Body: string(body), SHA256: hex.EncodeToString(sum[:])}
	if len(decision) > 0 {
		rec.Decision = string(decision)
		dsum := sha256.Sum256(decision)
		rec.DecisionSHA = hex.EncodeToString(dsum[:])
	}
	if err := o.write(rec); err != nil {
		return err
	}
	return o.apply(rec)
}

// AppendAttempt records a new body for a 400 that has no stored item, and stores the decided item with it. The
// refused lines stay in the file. It refuses anything Signal accepted, a 409, and a rejection that already has an item.
func (o *Outbox) AppendAttempt(key, subject, rule string, body, decision []byte) error {
	e, ok := o.entries[key]
	if !ok || !e.Undecided() {
		return ErrNotRetryable
	}
	if len(decision) == 0 || !json.Valid(decision) {
		return errors.New("outbox: decision is not JSON")
	}
	if !json.Valid(body) {
		return errors.New("outbox: body is not JSON")
	}
	sum := sha256.Sum256(body)
	dsum := sha256.Sum256(decision)
	rec := record{Key: key, State: Pending, At: o.now().UTC(), Kind: Event, Subject: subject, Rule: rule,
		Body: string(body), SHA256: hex.EncodeToString(sum[:]),
		Decision: string(decision), DecisionSHA: hex.EncodeToString(dsum[:]), Retry: true}
	if err := o.write(rec); err != nil {
		return err
	}
	return o.apply(rec)
}

// Supersede records that the current rules no longer send a rejected event. It does not send. The refused lines stay.
// tier is hold or never.
func (o *Outbox) Supersede(key, tier string) error {
	e, ok := o.entries[key]
	if !ok || e.Kind != Event || e.State != Rejected || conflict(e) || len(e.Decision) == 0 {
		return ErrNotRetryable
	}
	if tier != TierHold && tier != TierNever {
		return errors.New("outbox: a superseded entry is hold or never")
	}
	rec := record{Key: key, State: Superseded, At: o.now().UTC(), Code: tier}
	if err := o.write(rec); err != nil {
		return err
	}
	return o.apply(rec)
}

// Requeue appends a new body for a rejected event Signal did not store and sets the key pending. It refuses an
// entry Signal accepted, a pending entry, a lease claim, a 409, and a refusal with no stored item. The original
// refused body stays in the file.
func (o *Outbox) Requeue(key string, body []byte) error {
	e, ok := o.entries[key]
	if !ok || e.Kind != Event || e.State != Rejected || conflict(e) || len(e.Decision) == 0 {
		return ErrNotRetryable
	}
	if !json.Valid(body) {
		return errors.New("outbox: body is not JSON")
	}
	sum := sha256.Sum256(body)
	rec := record{Key: key, State: Pending, At: o.now().UTC(), Body: string(body), SHA256: hex.EncodeToString(sum[:]), Retry: true}
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

// Expire records that a pending lease claim's period ended unanswered.
func (o *Outbox) Expire(key string) error {
	e, ok := o.entries[key]
	if !ok || e.Kind != Lease {
		return errors.New("outbox: only a lease claim expires")
	}
	return o.Mark(key, Expired, "", 0, "", "")
}

// SyncDir syncs a directory, so a file created in it survives a crash.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Locked reports whether this outbox holds the lock on dir: it was opened with Open on that directory and is not
// closed.
func (o *Outbox) Locked(dir string) bool {
	if o.file == nil {
		return false
	}
	want, err1 := filepath.Abs(filepath.Join(dir, FileName))
	have, err2 := filepath.Abs(o.path)
	return err1 == nil && err2 == nil && want == have
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

// PendingEvents returns the pending event bodies, leaving out lease claims.
func (o *Outbox) PendingEvents() []Entry {
	var out []Entry
	for _, e := range o.Pending() {
		if e.Kind == Event {
			out = append(out, e)
		}
	}
	return out
}

// OpenAsks returns the keys of event bodies on subject that ask someone and that no other body closes, oldest
// first. Rejected and superseded bodies count for neither: they were not sent.
func (o *Outbox) OpenAsks(subject string) []string {
	return o.openAsks(func(s, _ string) bool { return s == subject })
}

// OpenAsksSiblings returns the open asks, oldest first, on subject, and the asks on its sibling subjects that an own
// act bounded by before closes (source.ClosesSibling).
func (o *Outbox) OpenAsksSiblings(subject, before string) []string {
	return o.openAsks(func(s, key string) bool { return s == subject || source.ClosesSibling(subject, before, s, key) })
}

func (o *Outbox) openAsks(on func(subject, key string) bool) []string {
	closed := map[string]bool{}
	for _, k := range o.order {
		e := o.entries[k]
		if e.State == Rejected || e.State == Superseded {
			continue
		}
		for _, c := range e.Closes {
			closed[c] = true
		}
	}
	var out []string
	for _, k := range o.order {
		e := o.entries[k]
		if e.Kind == Event && on(e.Subject, k) && e.Asks && e.State != Rejected && e.State != Superseded && !closed[k] {
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
