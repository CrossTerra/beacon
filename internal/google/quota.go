package google

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

// Gmail's per-user quota, for a Cloud project that used the API before May 2026 and kept that quota.
// messages.get with metadata costs 5 units. The per-user limit is 250 units per second and 15,000 per
// minute. Beacon stays under both: a first-run scan that spends the budget as fast as the network allows
// is what returns 429, and the next pass then starts the same scan again.
const (
	// Published per-user limits. The budgets below stay under them.
	gmailPerSecond = 250
	gmailPerMinute = 15000
	// Headroom, so a window that does not line up with Google's, and the moving average that allows a short
	// burst, still stay under the limit.
	gmailSecondBudget = gmailPerSecond * 4 / 5
	gmailMinuteBudget = gmailPerMinute * 4 / 5
)

// gmailUnits is the quota cost of one Gmail call, on that older per-user quota. A path is the part after
// /gmail/v1/users/me/, without the query.
func gmailUnits(path string) int {
	switch {
	case path == "profile" || path == "labels":
		return 1
	case path == "history" || strings.HasPrefix(path, "history"):
		return 2
	case path == "messages" || strings.HasPrefix(path, "messages"):
		return 5
	case path == "threads" || strings.HasPrefix(path, "threads?"):
		return 10
	case strings.HasPrefix(path, "threads/"):
		return 10
	default:
		return 5
	}
}

type quotaSpend struct {
	at    time.Time
	units int
}

// gmailQuota is a token bucket for one Gmail source: at most gmailSecondBudget units in any second and
// gmailMinuteBudget in any minute. It uses the source's clock and sleep, so a test clock that does not move
// still advances by the wait the sleep was asked for.
type gmailQuota struct {
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
	last   time.Time
	offset time.Duration
	spent  []quotaSpend
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

// take waits until spending units would stay under both budgets, then records the spend. A cancelled wait
// returns the context's error and spends nothing.
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

// delay is how long to wait before spending units fits both windows. Zero means it fits now.
func (q *gmailQuota) delay(units int) time.Duration {
	now := q.clock()
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
