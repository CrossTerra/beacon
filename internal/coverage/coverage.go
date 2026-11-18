// Package coverage records how well each source is being read (DEC-42): when it last collected, when it last failed
// and why, whether its last read was cut or rate limited, and for Slack how many of its channels a pass read and how
// long the least recently read one has waited. The agent keeps it in coverage.json in the state directory, which
// `beacon status --coverage` prints, and reports it to Signal: a heartbeat lease per source, renewed after each pass
// that collected, and a state snapshot on a subject of its own when the coverage changes (report.go).
//
// Everything in it is a source name, a time, a count, a flag or a fixed reason code. No item, key, id or text.
package coverage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

// FileName is the coverage record in the state directory.
const FileName = "coverage.json"

// FailingAfter is how many passes in a row must fail before a source is failing. One failure, a network that is not
// up yet after a wake, say, is recorded and not reported as failing.
const FailingAfter = 2

// Behind is how long a channel may wait since its last read before the source is limited: twice the quiet channels'
// interval.
const Behind = 30 * time.Minute

// Statuses.
const (
	StatusOK      = "ok"
	StatusFailing = "failing"
	StatusLimited = "limited"
)

// Record is coverage.json.
type Record struct {
	Sources map[string]*Source `json:"sources,omitempty"`
	// Slept is the last sleep the agent saw: the machine was asleep from From to To, and no source was read.
	Slept *Sleep `json:"slept,omitempty"`
}

// Sleep is a span the machine was asleep.
type Sleep struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Source is one source's coverage.
type Source struct {
	// EverySeconds is the source's interval.
	EverySeconds int64 `json:"every_seconds"`
	// LastPass is when its last pass ended, and PassMillis how long its collect ran, without time asleep.
	LastPass   time.Time `json:"last_pass"`
	PassMillis int64     `json:"pass_ms"`
	// LastSuccess is the end of its last pass that collected.
	LastSuccess *time.Time `json:"last_success,omitempty"`
	// LastFailure and Reason are its last pass that did not, and the fixed reason code (source_unauthorized).
	LastFailure *time.Time `json:"last_failure,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	// Failures is how many passes in a row have failed, and FailingSince when the first of them ended.
	Failures     int        `json:"failures,omitempty"`
	FailingSince *time.Time `json:"failing_since,omitempty"`
	// Cut, Expired, Paused and Limited are the last pass's: a read stopped at its limit, a change history that had
	// expired, a read paused at a rate limit to continue next pass, and a rate limit the API answered with.
	Cut     bool `json:"cut,omitempty"`
	Expired bool `json:"expired,omitempty"`
	Paused  bool `json:"paused,omitempty"`
	Limited bool `json:"rate_limited,omitempty"`
	// Names is a display-name warning code from the last pass, or "".
	Names string `json:"names,omitempty"`
	// Channels is how much of its channels the last pass that collected read (Slack).
	Channels *Channels `json:"channels,omitempty"`
}

// Channels is a source's reach over its channels.
type Channels struct {
	Total   int `json:"total"`
	Read    int `json:"read"`
	Never   int `json:"never,omitempty"`
	Waiting int `json:"waiting,omitempty"`
	Gaps    int `json:"gaps,omitempty"`
	// Oldest is the oldest last read among the channels read at least once.
	Oldest *time.Time `json:"oldest,omitempty"`
}

// Pass is what one pass of a source came to.
type Pass struct {
	Took time.Duration
	// Failed is the fixed reason the collect failed, or "".
	Failed                        string
	Cut, Expired, Paused, Limited bool
	Names                         string
	Reach                         *source.Reach
}

// Observe records a pass of the named source that ended at now.
func (r *Record) Observe(name string, every time.Duration, p Pass, now time.Time) {
	if r.Sources == nil {
		r.Sources = map[string]*Source{}
	}
	s := r.Sources[name]
	if s == nil {
		s = &Source{}
		r.Sources[name] = s
	}
	now = now.UTC()
	s.EverySeconds = int64(every / time.Second)
	s.LastPass, s.PassMillis = now, p.Took.Milliseconds()
	s.Cut, s.Expired, s.Paused, s.Limited = p.Cut, p.Expired, p.Paused, p.Limited
	if p.Failed != "" {
		at := now
		s.LastFailure, s.Reason = &at, p.Failed
		if s.Failures == 0 {
			s.FailingSince = &at
		}
		s.Failures++
		return
	}
	at := now
	s.LastSuccess = &at
	s.Failures, s.FailingSince = 0, nil
	s.Names = p.Names
	s.Channels = nil
	if p.Reach != nil {
		c := &Channels{Total: p.Reach.Total, Read: p.Reach.Read, Never: p.Reach.Never, Waiting: p.Reach.Waiting, Gaps: p.Reach.Gaps}
		if !p.Reach.Oldest.IsZero() {
			o := p.Reach.Oldest.UTC()
			c.Oldest = &o
		}
		s.Channels = c
	}
}

// Asleep records that the machine slept from from to to.
func (r *Record) Asleep(from, to time.Time) {
	r.Slept = &Sleep{From: from.UTC(), To: to.UTC()}
}

// Keep drops the sources not named, so a source removed from the config leaves the record.
func (r *Record) Keep(names []string) {
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	for n := range r.Sources {
		if !keep[n] {
			delete(r.Sources, n)
		}
	}
}

// Status is the source's status now: failing after FailingAfter failed passes in a row; limited when its last read
// was cut, paused or rate limited, its history had expired, or channels wait (past the per-pass limit, below a page
// limit, or longer than Behind since their last read); else ok.
func (s *Source) Status(now time.Time) string {
	switch {
	case s.Failures >= FailingAfter:
		return StatusFailing
	case s.Cut || s.Expired || s.Paused || s.Limited || s.behind(now):
		return StatusLimited
	}
	return StatusOK
}

func (s *Source) behind(now time.Time) bool {
	c := s.Channels
	if c == nil {
		return false
	}
	return c.Waiting > 0 || c.Gaps > 0 || (c.Oldest != nil && now.Sub(*c.Oldest) > Behind)
}

// ErrRecord means coverage.json is not a record this version wrote. Its text is fixed: it does not include the file.
var ErrRecord = errors.New("coverage: the record is unreadable")

var (
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	reasonPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// maxSources bounds the record, as the config bounds nothing else.
const maxSources = 64

func validate(r Record) error {
	if len(r.Sources) > maxSources {
		return ErrRecord
	}
	for name, s := range r.Sources {
		if s == nil || !namePattern.MatchString(name) || s.LastPass.IsZero() || s.EverySeconds < 0 || s.PassMillis < 0 || s.Failures < 0 {
			return ErrRecord
		}
		if s.Reason != "" && !reasonPattern.MatchString(s.Reason) {
			return ErrRecord
		}
		switch s.Names {
		case "", source.WarnNamesScope, source.WarnNamesFailed:
		default:
			return ErrRecord
		}
		if c := s.Channels; c != nil && (c.Total < 0 || c.Read < 0 || c.Never < 0 || c.Waiting < 0 || c.Gaps < 0) {
			return ErrRecord
		}
	}
	if r.Slept != nil && (r.Slept.From.IsZero() || r.Slept.To.Before(r.Slept.From)) {
		return ErrRecord
	}
	return nil
}

// Read reads the record in dir. A missing file is an empty record. Anything else that is not a valid record is
// ErrRecord.
func Read(dir string) (Record, error) {
	f, err := os.Open(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, err
	}
	defer f.Close()
	var r Record
	if err := json.NewDecoder(io.LimitReader(f, 1<<16)).Decode(&r); err != nil {
		return Record{}, ErrRecord
	}
	if err := validate(r); err != nil {
		return Record{}, err
	}
	return r, nil
}

// Write replaces the record in dir, atomically and synced, mode 0600.
func Write(dir string, r Record) error {
	if err := validate(r); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeAtomic(dir, FileName, append(data, '\n'))
}

func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, name+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return err
	}
	cleanup = false
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Format is what `beacon status --coverage` prints: a block per source, in name order, and the last sleep. Times are
// UTC; ages are from now.
func Format(r Record, now time.Time) string {
	var b strings.Builder
	if len(r.Sources) == 0 {
		b.WriteString("coverage: none recorded yet (the agent's send loop records it)\n")
	}
	names := make([]string, 0, len(r.Sources))
	for n := range r.Sources {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := r.Sources[n]
		fmt.Fprintf(&b, "source: %s\n", n)
		fmt.Fprintf(&b, "  status: %s\n", s.Status(now))
		fmt.Fprintf(&b, "  every: %s\n", time.Duration(s.EverySeconds)*time.Second)
		fmt.Fprintf(&b, "  last_pass: %s, collect took %s\n", stamp(&s.LastPass, now), (time.Duration(s.PassMillis) * time.Millisecond).Round(time.Millisecond))
		fmt.Fprintf(&b, "  last_success: %s\n", stamp(s.LastSuccess, now))
		failure := stamp(s.LastFailure, now)
		if s.LastFailure != nil {
			failure += " " + s.Reason
		}
		fmt.Fprintf(&b, "  last_failure: %s\n", failure)
		if s.Failures > 0 {
			fmt.Fprintf(&b, "  failing: %d passes in a row, since %s\n", s.Failures, stamp(s.FailingSince, now))
		}
		var flags []string
		for _, f := range []struct {
			on   bool
			name string
		}{{s.Cut, "cut"}, {s.Expired, "history_expired"}, {s.Paused, "paused"}, {s.Limited, "rate_limited"}} {
			if f.on {
				flags = append(flags, f.name)
			}
		}
		if s.Names != "" {
			flags = append(flags, s.Names)
		}
		if len(flags) == 0 {
			flags = []string{"none"}
		}
		fmt.Fprintf(&b, "  flags: %s\n", strings.Join(flags, " "))
		if c := s.Channels; c != nil {
			fmt.Fprintf(&b, "  channels: %d read of %d, %d never read, %d waiting, %d with a gap, oldest read %s\n",
				c.Read, c.Total, c.Never, c.Waiting, c.Gaps, stamp(c.Oldest, now))
		}
	}
	if r.Slept != nil {
		fmt.Fprintf(&b, "asleep: %s to %s (%s)\n", r.Slept.From.UTC().Format(time.RFC3339), r.Slept.To.UTC().Format(time.RFC3339),
			r.Slept.To.Sub(r.Slept.From).Round(time.Second))
	}
	return b.String()
}

func stamp(t *time.Time, now time.Time) string {
	if t == nil || t.IsZero() {
		return "none"
	}
	age := now.Sub(*t).Round(time.Second)
	if age < 0 {
		age = 0
	}
	return fmt.Sprintf("%s (%s ago)", t.UTC().Format(time.RFC3339), age)
}
