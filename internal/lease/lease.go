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

// Subject is the subject lease claims are written about.
func Subject(person string) string {
	return "beacon:lease:" + strings.TrimPrefix(person, "@")
}

// Key is the claim key for period p.
func (l *Lease) Key(p int64) string {
	return "claim:" + Subject(l.Person) + ":" + strconv.FormatInt(p, 10)
}

func (l *Lease) period(t time.Time) int64 { return t.Unix() / int64(l.Period/time.Second) }

// Status is what Ensure found.
type Status struct {
	Held bool
	// Period is the current period, and Until when the lease as held now runs out.
	Period int64
	Until  time.Time
}

// Ensure makes sure this instance holds the current period if it can, and claims the next one ahead. It reports
// whether this instance holds the lease now.
func (l *Lease) Ensure(ctx context.Context) (Status, error) {
	if l.Period < time.Minute {
		return Status{}, errors.New("lease: period under a minute")
	}
	p := l.period(l.Now())
	st := Status{Period: p}
	held, err := l.claim(ctx, p)
	if err != nil || !held {
		return st, err
	}
	st.Held = true
	st.Until = time.Unix((p+1)*int64(l.Period/time.Second), 0)
	next, err := l.claim(ctx, p+1)
	if err != nil {
		return st, err
	}
	if next {
		st.Until = time.Unix((p+2)*int64(l.Period/time.Second), 0)
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
		return false, nil
	}
	res, err := l.Writer.Write(ctx, e.Body)
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
		return false, fmt.Errorf("%w: %d %s", ErrRefused, res.Status, res.Code)
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
