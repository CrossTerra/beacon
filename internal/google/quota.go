package google

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

// Gmail's per-user quota. Since May 2026 a Cloud project that had not used the Gmail API before then gets 6,000 units
// per user per minute, per project, and threads.get costs 40 units (it was 10). Beacon cannot tell which quota a
// project has, so it paces by the newer, smaller one, which is also under the older one (250 units per second and
// 15,000 per minute, at a quarter of the cost). Every tool that uses the same OAuth client for the same person
// spends the same per-user minute.
const (
	// Published per-user limit on the newer quota. The budget below stays under it.
	gmailPerMinute = 6000
	// Headroom, so a window that does not line up with Google's, and another tool on the same OAuth client, still
	// stay under the limit.
	gmailMinuteBudget = gmailPerMinute * 4 / 5
	// A burst cap, under the older quota's 250 units per second.
	gmailSecondBudget = 200
	// gmailLimitWait is how long a call waits after Google answers with a rate limit that names no Retry-After: a
	// whole per-minute window.
	gmailLimitWait = time.Minute
	// gmailMaxLimitWait is the longest Retry-After a pass waits out before it pauses instead.
	gmailMaxLimitWait = 2 * time.Minute
)

// gmailUnits is the quota cost of one Gmail call, on the newer quota. A path is the part after /gmail/v1/users/me/,
// without the query. Beacon reads threads only with format=metadata and messages only with format=full.
func gmailUnits(path string) int {
	switch {
	case path == "profile" || path == "labels":
		return 1
	case path == "history":
		return 2
	case path == "messages":
		return 5
	case strings.HasPrefix(path, "messages/"):
		// messages.get with format=full: 60 (20 for metadata, which Beacon does not ask for).
		return 60
	case path == "threads":
		return 10
	case strings.HasPrefix(path, "threads/"):
		// threads.get with format=metadata: 40 (120 for full, which Beacon does not ask for).
		return 40
	default:
		return 60
	}
}

// gmailPathUnits is the cost of a request to the Gmail API, from its URL path.
func gmailPathUnits(p string) int {
	const prefix = "/gmail/v1/users/me/"
	if i := strings.Index(p, prefix); i >= 0 {
		return gmailUnits(p[i+len(prefix):])
	}
	return gmailUnits("")
}

type quotaSpend struct {
	at    time.Time
	units int
}

// gmailQuota is a token bucket for one Gmail source: at most gmailSecondBudget units in any second and
// gmailMinuteBudget in any minute. Every call is counted when it is sent, retries included. It uses the source's
// clock and sleep, so a test clock that does not move still advances by the wait the sleep was asked for.
type gmailQuota struct {
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
	last   time.Time
	offset time.Duration
	spent  []quotaSpend
	// until is when Google said, by a rate limit, that the next call may go. Zero means no hold.
	until time.Time
}

func (q *gmailQuota) clock() time.Time {
	n := time.Now()
	if q.now != nil {
		n = q.now()
	}
	if q.last.IsZero() || n.After(q.last) {
		q.last = n
	}
	return q.last.Add(q.offset)
}

func (q *gmailQuota) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	var err error
	if q.sleep != nil {
		err = q.sleep(ctx, d)
	} else {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-t.C:
		}
	}
	if err != nil {
		return err
	}
	var n time.Time
	if q.now != nil {
		n = q.now()
	} else {
		n = time.Now()
	}
	if !n.After(q.last) {
		q.offset += d
	} else {
		q.last = n
	}
	return nil
}

// take waits until spending units would stay under both budgets, and until any hold a rate limit set has passed,
// then records the spend. A cancelled wait returns the context's error and spends nothing.
func (q *gmailQuota) take(ctx context.Context, units int) error {
	if q == nil || units <= 0 {
		return nil
	}
	if units > gmailSecondBudget || units > gmailMinuteBudget {
		return source.ErrRateLimited
	}
	for {
		wait := q.delay(units)
		if wait <= 0 {
			q.spent = append(q.spent, quotaSpend{at: q.clock(), units: units})
			q.prune(q.clock())
			return nil
		}
		if err := q.wait(ctx, wait); err != nil {
			return err
		}
	}
}

// hold makes the next call wait d from now. Google answered with a rate limit, so the units Beacon counted are not
// the whole story (another tool on the same OAuth client, or a window that does not line up with this one).
func (q *gmailQuota) hold(d time.Duration) {
	if q == nil || d <= 0 {
		return
	}
	if t := q.clock().Add(d); t.After(q.until) {
		q.until = t
	}
}

// recent is the units spent in the last minute.
func (q *gmailQuota) recent() int {
	if q == nil {
		return 0
	}
	now := q.clock()
	n := 0
	for _, s := range q.spent {
		if age := now.Sub(s.at); age >= 0 && age < time.Minute {
			n += s.units
		}
	}
	return n
}

// delay is how long to wait before spending units fits both windows. Zero means it fits now.
func (q *gmailQuota) delay(units int) time.Duration {
	now := q.clock()
	if q.until.After(now) {
		return q.until.Sub(now)
	}
	if q.fits(now, units) {
		return 0
	}
	candidates := []time.Time{now}
	for _, s := range q.spent {
		candidates = append(candidates, s.at.Add(time.Second), s.at.Add(time.Minute))
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Before(candidates[j]) })
	for _, t := range candidates {
		if t.Before(now) {
			continue
		}
		if q.fits(t, units) {
			return t.Sub(now)
		}
	}
	if len(q.spent) == 0 {
		return 0
	}
	return q.spent[0].at.Add(time.Minute).Sub(now)
}

func (q *gmailQuota) fits(at time.Time, units int) bool {
	sec, min := 0, 0
	for _, s := range q.spent {
		age := at.Sub(s.at)
		if age < 0 {
			continue
		}
		if age < time.Second {
			sec += s.units
		}
		if age < time.Minute {
			min += s.units
		}
	}
	return sec+units <= gmailSecondBudget && min+units <= gmailMinuteBudget
}

func (q *gmailQuota) prune(now time.Time) {
	out := q.spent[:0]
	for _, s := range q.spent {
		if now.Sub(s.at) < time.Minute {
			out = append(out, s)
		}
	}
	q.spent = out
}
