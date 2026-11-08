// Package lease keeps one Beacon instance per person sending (DEC-25).
//
// Time is cut into periods of a fixed length. The lease for period N is the Signal claim with key
// "claim:beacon:lease:<person>:<N>": a key holds one event only, so one instance holds each period. The instance
// that holds period N claims N+1 during N, so it keeps the lease for as long as it runs. Another instance tries the
// current period each time it runs, gets 409 idempotency_conflict while the lease is held, and takes the lease within
// two periods of the holder stopping. Each claim closes the holder's previous claim, so at most two stay open.
//
// Claims are request bodies like any other: they go through the outbox, and a retry sends the stored bytes.
package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/signal"
)

// Writer writes one body to Signal.
type Writer interface {
	Write(ctx context.Context, body []byte) (signal.Result, error)
}

// Lease is this instance's view of the lease.
type Lease struct {
	Outbox *outbox.Outbox
	Writer Writer
	Person string // "@handle" or "act_…"
	Run    string // names this instance, the same across restarts
	Period time.Duration
	Now    func() time.Time
}

// ErrRefused means Signal refused a claim for a reason other than another instance holding it.
var ErrRefused = errors.New("lease: Signal refused the claim")

// RefusedError is Signal's refusal of a claim: its status and error code.
type RefusedError struct {
	Status int
	Code   string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("Signal refused the lease claim with %d %s", e.Status, e.Code)
}

// Is makes errors.Is(err, ErrRefused) true.
func (e *RefusedError) Is(target error) bool { return target == ErrRefused }

// Subject is the subject lease claims are written about.
func Subject(person string) string {
	return "beacon:lease:" + strings.TrimPrefix(person, "@")
}

// Key is the claim key for period p.
func (l *Lease) Key(p int64) string {
	return "claim:" + Subject(l.Person) + ":" + strconv.FormatInt(p, 10)
}

func (l *Lease) period(t time.Time) int64 { return t.Unix() / int64(l.Period/time.Second) }

// end is when period p ends.
func (l *Lease) end(p int64) time.Time { return time.Unix((p+1)*int64(l.Period/time.Second), 0) }

// expire marks pending claims for periods before p as expired: they can no longer be held, and would otherwise stay
// pending for ever.
func (l *Lease) expire(p int64) error {
	prefix := "claim:" + Subject(l.Person) + ":"
	for _, e := range l.Outbox.Pending() {
		if e.Kind != outbox.Lease || !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(e.Key, prefix), 10, 64)
		if err != nil || n < p {
			if err := l.Outbox.Expire(e.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

// Status is what Ensure found.
type Status struct {
	Held bool
	// Period is the current period, and Until when the lease as held now runs out.
	Period int64
	Until  time.Time
}

// Ensure makes sure this instance holds the current period if it can, and claims the next one ahead. It reports
// whether this instance holds the lease now. An error says why it could not find out: Signal was unreachable
// (signal.ErrUnavailable), the key was refused (signal.ErrUnauthorized), Signal refused the claim (ErrRefused), or
// the period ended first (context.DeadlineExceeded). With no error and Held false, another instance holds it.
func (l *Lease) Ensure(ctx context.Context) (Status, error) {
	if l.Period < time.Minute {
		return Status{}, errors.New("lease: period under a minute")
	}
	p := l.period(l.Now())
	st := Status{Period: p}
	if err := l.expire(p); err != nil {
		return st, err
	}
	held, err := l.claim(ctx, p)
	if err != nil || !held {
		return st, err
	}
	st.Held = true
	st.Until = l.end(p)
	next, err := l.claim(ctx, p+1)
	if err != nil {
		return st, err
	}
	if next {
		st.Until = l.end(p + 1)
	}
	return st, nil
}

// holds reports whether the outbox shows this instance holding period p.
func (l *Lease) holds(p int64) bool {
	e, ok := l.Outbox.Get(l.Key(p))
	return ok && (e.State == outbox.Sent || e.State == outbox.Replayed)
}

// claim makes sure a claim for p was sent, and reports whether it is this instance's.
func (l *Lease) claim(ctx context.Context, p int64) (bool, error) {
	key := l.Key(p)
	e, ok := l.Outbox.Get(key)
	if !ok {
		body, err := l.body(p)
		if err != nil {
			return false, err
		}
		if err := l.Outbox.Append(outbox.Lease, key, Subject(l.Person), "", body); err != nil {
			return false, err
		}
		e, _ = l.Outbox.Get(key)
	}
	switch e.State {
	case outbox.Sent, outbox.Replayed:
		return true, nil
	case outbox.Rejected:
		if e.Code != "idempotency_conflict" {
			return false, &RefusedError{Status: e.Status, Code: e.Code}
		}
		return false, nil
	case outbox.Expired:
		return false, nil
	}
	// A claim is worth nothing once its period has ended, so its write, with its retries, ends then too.
	wctx, cancel := context.WithTimeout(ctx, l.end(p).Sub(l.Now()))
	defer cancel()
	res, err := l.Writer.Write(wctx, e.Body)
	if err != nil {
		return false, err
	}
	switch res.Outcome {
	case signal.Written:
		return true, l.Outbox.Mark(key, outbox.Sent, res.EventID, res.Status, "", "")
	case signal.Replayed:
		return true, l.Outbox.Mark(key, outbox.Replayed, res.EventID, res.Status, "", "")
	case signal.Conflict:
		return false, l.Outbox.Mark(key, outbox.Rejected, "", res.Status, res.Code, res.Message)
	default:
		if err := l.Outbox.Mark(key, outbox.Rejected, "", res.Status, res.Code, res.Message); err != nil {
			return false, err
		}
		return false, &RefusedError{Status: res.Status, Code: res.Code}
	}
}

type ref struct {
	Key string `json:"key"`
}

type claimBody struct {
	Subject string `json:"subject"`
	Type    string `json:"type"`
	Key     string `json:"key"`
	Open    struct {
		Title string `json:"title"`
	} `json:"open"`
	To       []string `json:"to"`
	Closes   []ref    `json:"closes,omitempty"`
	Metadata struct {
		Run           string `json:"run"`
		PeriodStart   string `json:"period_start"`
		PeriodSeconds int64  `json:"period_seconds"`
	} `json:"metadata"`
}

// body builds the claim for p. It depends only on the person, the run, the period and the outbox, so a retry from
// this instance is the same request and replays.
func (l *Lease) body(p int64) ([]byte, error) {
	var b claimBody
	b.Subject = Subject(l.Person)
	b.Type = "beacon.lease_claimed"
	b.Key = l.Key(p)
	b.Open.Title = "Beacon lease for " + l.Person
	// A claim is an ask to the writer itself (Signal DEC-76). With no `by`, `me` is Beacon's own actor.
	b.To = []string{"me"}
	if l.holds(p - 1) {
		b.Closes = []ref{{Key: l.Key(p - 1)}}
	}
	secs := int64(l.Period / time.Second)
	b.Metadata.Run = l.Run
	b.Metadata.PeriodStart = time.Unix(p*secs, 0).UTC().Format(time.RFC3339)
	b.Metadata.PeriodSeconds = secs
	return json.Marshal(b)
}

// InstanceFile is the file in the state directory naming this instance.
const InstanceFile = "instance"

// Instance returns this instance's id, creating it on first use. It stays the same across restarts, so a restarted
// instance replays its own claim instead of losing the lease to itself.
func Instance(dir string) (string, error) {
	path := filepath.Join(dir, InstanceFile)
	data, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if id == "" {
			return "", fmt.Errorf("lease: %s is empty", path)
		}
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		f.Close()
		return "", err
	}
	return id, f.Close()
}
