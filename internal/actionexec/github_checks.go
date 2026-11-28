package actionexec

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

const (
	mergePageSize  = 100
	mergePageLimit = 10
)

// These labels are local display text, never URLs or strings taken from a GitHub response.
type githubReadStep string

const (
	ghAccountRead githubReadStep = "account"
	ghPullRead    githubReadStep = "pull request"
	ghReviewsRead githubReadStep = "reviews"
	ghRulesRead   githubReadStep = "branch rules"
	ghBranchRead  githubReadStep = "base branch"
	ghChecksRead  githubReadStep = "check runs"
	ghStatusRead  githubReadStep = "commit statuses"
)

func (s githubReadStep) unavailable(why string) approval.Refusal {
	return approval.Refusal{Code: "unavailable", Detail: "GitHub " + string(s) + ": " + why}
}

// Add evidence to an otherwise opaque read failure. Keep the journal code, permission exceptions,
// quotas and refusal decisions unchanged. Never render an error, response body, path or header.
func (s githubReadStep) explain(ctx context.Context, status int, err error) error {
	var r approval.Refusal
	if !errors.As(err, &r) || r.Code != "unavailable" || r.Detail != "" {
		return err
	}
	switch {
	case status >= 300:
		// A definite HTTP refusal is better evidence than a timer racing local decoding.
		return s.unavailable(fmt.Sprintf("HTTP %d", status))
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return s.unavailable("the read deadline expired")
	case errors.Is(ctx.Err(), context.Canceled):
		return s.unavailable("the read was canceled")
	case status == 0:
		return s.unavailable("no complete response")
	default:
		return s.unavailable("incomplete or invalid response")
	}
}

// Build page numbers locally. A Link can signal that the list continues, but never tells us where to send a token.
func githubPages(ctx context.Context, step githubReadStep, read func(url.Values) (answer, error), use func(answer) (int, bool, error)) error {
	for page := 1; page <= mergePageLimit; page++ {
		a, err := read(url.Values{"page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(mergePageSize)}})
		if err != nil {
			return step.explain(ctx, a.status, err)
		}
		n, more, err := use(a)
		if err != nil {
			return step.explain(ctx, a.status, err)
		}
		if n < mergePageSize && !more && !strings.Contains(strings.Join(a.header.Values("Link"), ","), `rel="next"`) {
			return nil
		}
	}
	return step.unavailable("the page limit was reached before the list was complete")
}

func (a githubAPI) reviews(ctx context.Context, m pinnedMerge) error {
	latest := make(map[int64]string)
	err := githubPages(ctx, ghReviewsRead, func(q url.Values) (answer, error) {
		return a.get(ctx, m.pullPath()+"/reviews", q)
	}, func(ans answer) (int, bool, error) {
		var reviews []struct {
			User  githubUser `json:"user"`
			State string     `json:"state"`
		}
		if ans.decode(&reviews) != nil || reviews == nil {
			return 0, false, refusal("unavailable")
		}
		// GitHub returns reviews in chronological order. Comment-only reviews don't dismiss an objection.
		for _, r := range reviews {
			switch r.State {
			case "COMMENTED", "PENDING":
				continue
			case "CHANGES_REQUESTED", "APPROVED", "DISMISSED":
				if r.User.ID > 0 {
					latest[r.User.ID] = r.State
					continue
				}
			}
			return 0, false, refusal("unavailable")
		}
		return len(reviews), false, nil
	})
	if err != nil {
		return err
	}
	for _, state := range latest {
		if state == "CHANGES_REQUESTED" {
			return refusal("changes_requested")
		}
	}
	return nil
}

type githubContext struct {
	Context string `json:"context"`
}

func (a githubAPI) requiredChecks(ctx context.Context, m pinnedMerge) (map[string]bool, error) {
	base := url.PathEscape(m.base)
	required := make(map[string]bool)
	add := func(checks []githubContext) error {
		for _, c := range checks {
			if c.Context == "" {
				return refusal("unavailable")
			}
			required[c.Context] = true
		}
		return nil
	}
	// 403/404 may mean the account cannot see the rules; GitHub still enforces them at the PUT.
	err := githubPages(ctx, ghRulesRead, func(q url.Values) (answer, error) {
		return a.get(ctx, m.repoPath()+"/rules/branches/"+base, q)
	}, func(ans answer) (int, bool, error) {
		if ans.status == 403 || ans.status == 404 {
			return 0, false, nil
		}
		var rules []struct {
			Type       string `json:"type"`
			Parameters struct {
				Checks []githubContext `json:"required_status_checks"`
			} `json:"parameters"`
		}
		if ans.decode(&rules) != nil || rules == nil {
			return 0, false, refusal("unavailable")
		}
		for _, r := range rules {
			if r.Type == "required_status_checks" {
				if len(r.Parameters.Checks) == 0 {
					return 0, false, refusal("unavailable")
				}
				if err := add(r.Parameters.Checks); err != nil {
					return 0, false, err
				}
			}
		}
		return len(rules), false, nil
	})
	if err != nil {
		return nil, err
	}
	ans, err := a.get(ctx, m.repoPath()+"/branches/"+base, nil)
	if err != nil {
		return nil, ghBranchRead.explain(ctx, ans.status, err)
	}
	if ans.status == 403 || ans.status == 404 {
		return required, nil
	}
	var branch *struct {
		Protection struct {
			Required struct {
				Contexts []string        `json:"contexts"`
				Checks   []githubContext `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if ans.decode(&branch) != nil || branch == nil {
		return nil, ghBranchRead.explain(ctx, ans.status, refusal("unavailable"))
	}
	for _, name := range branch.Protection.Required.Contexts {
		if name == "" {
			return nil, ghBranchRead.unavailable("incomplete or invalid response")
		}
		required[name] = true
	}
	return required, ghBranchRead.explain(ctx, ans.status, add(branch.Protection.Required.Checks))
}

type githubRun struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Started    time.Time `json:"started_at"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

type checkRank uint8

const (
	passed checkRank = iota
	pending
	failed
)

func (r githubRun) rank() checkRank {
	if r.Status != "completed" {
		return pending
	}
	switch r.Conclusion {
	case "success", "neutral", "skipped":
		return passed
	default:
		return failed
	}
}

func (r githubRun) newer(other githubRun) bool {
	if r.ID > 0 && other.ID > 0 && r.ID != other.ID {
		return r.ID > other.ID
	}
	// An unstarted, unfinished run sorts last; an unstarted, completed one sorts first.
	queued, otherQueued := r.Started.IsZero() && r.Status != "completed", other.Started.IsZero() && other.Status != "completed"
	if queued != otherQueued {
		return queued
	}
	if !r.Started.Equal(other.Started) {
		return r.Started.After(other.Started)
	}
	return r.rank() > other.rank() // An ambiguous tie cannot hide a failure.
}

type checkReport struct{ passed, pending, failed []string }

func (r checkReport) summary() string {
	var parts []string
	for _, group := range []struct {
		label string
		names []string
	}{{"failed", r.failed}, {"pending", r.pending}, {"passed", r.passed}} {
		if len(group.names) == 0 {
			continue
		}
		part := fmt.Sprintf("%d %s", len(group.names), group.label)
		if group.label != "passed" {
			var names []string
			for _, name := range group.names[:min(8, len(group.names))] {
				names = append(names, oneLine(name, 80))
			}
			part += ": " + strings.Join(names, ", ")
			if len(group.names) > len(names) {
				part += ", …"
			}
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "none reported"
	}
	return strings.Join(parts, " · ")
}

// Count the latest run from every app, and the current commit status. The worst of them wins per name.
func (a githubAPI) checks(ctx context.Context, m pinnedMerge, required map[string]bool) (checkReport, error) {
	path := m.repoPath() + "/commits/" + m.head
	type checkKey struct {
		name string
		app  int64
	}
	latest := make(map[checkKey]githubRun)
	runsRead := 0
	err := githubPages(ctx, ghChecksRead, func(q url.Values) (answer, error) {
		q.Set("filter", "all")
		return a.get(ctx, path+"/check-runs", q)
	}, func(ans answer) (int, bool, error) {
		var list struct {
			Total int         `json:"total_count"`
			Runs  []githubRun `json:"check_runs"`
		}
		if ans.decode(&list) != nil || list.Runs == nil {
			return 0, false, refusal("unavailable")
		}
		for _, r := range list.Runs {
			if r.Name == "" {
				return 0, false, refusal("unavailable")
			}
			k := checkKey{r.Name, r.App.ID}
			if prev, ok := latest[k]; !ok || r.newer(prev) {
				latest[k] = r
			}
		}
		runsRead += len(list.Runs)
		return len(list.Runs), runsRead < list.Total, nil
	})
	if err != nil {
		return checkReport{}, err
	}
	worst := make(map[string]checkRank)
	for k, r := range latest {
		worst[k.name] = max(worst[k.name], r.rank())
	}
	statusesRead := 0
	err = githubPages(ctx, ghStatusRead, func(q url.Values) (answer, error) {
		return a.get(ctx, path+"/status", q)
	}, func(ans answer) (int, bool, error) {
		var combined struct {
			Total    int `json:"total_count"`
			Statuses []struct {
				Context string `json:"context"`
				State   string `json:"state"`
			} `json:"statuses"`
		}
		if ans.decode(&combined) != nil || combined.Statuses == nil {
			return 0, false, refusal("unavailable")
		}
		for _, s := range combined.Statuses {
			if s.Context == "" {
				return 0, false, refusal("unavailable")
			}
			rank := failed
			switch s.State {
			case "success":
				rank = passed
			case "pending":
				rank = pending
			}
			worst[s.Context] = max(worst[s.Context], rank)
		}
		statusesRead += len(combined.Statuses)
		return len(combined.Statuses), statusesRead < combined.Total, nil
	})
	if err != nil {
		return checkReport{}, err
	}
	for name := range required {
		if _, ok := worst[name]; !ok {
			worst[name] = pending
		}
	}
	var report checkReport
	for _, name := range slices.Sorted(maps.Keys(worst)) {
		switch worst[name] {
		case failed:
			report.failed = append(report.failed, name)
		case pending:
			report.pending = append(report.pending, name)
		case passed:
			report.passed = append(report.passed, name)
		}
	}
	return report, nil
}
