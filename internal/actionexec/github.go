package actionexec

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
)

const mergeSettle = time.Minute

var sha40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

type mergeHandler struct {
	*provider
	user      int64
	openMu    sync.Mutex
	firstOpen map[string]time.Time
	readBacks mergeReadBacks
}

const mergeReadBackMax = 15 * time.Minute

type mergeAttempt struct {
	last  time.Time
	count int
}

// Automatic recovery keeps the one-minute cadence until settle, then doubles up to 15 minutes.
// Read again calls Reconcile directly and is not delayed by this.
type mergeReadBacks struct {
	mu       sync.Mutex
	attempts map[string]mergeAttempt
}

func (r *mergeReadBacks) mark(id string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attempts == nil {
		r.attempts = make(map[string]mergeAttempt)
	}
	n := min(r.attempts[id].count+1, 8)
	r.attempts[id] = mergeAttempt{at, n}
}

func (r *mergeReadBacks) get(id string) (mergeAttempt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.attempts[id]
	return a, ok
}

func mergeReadBackDelay(count int) time.Duration {
	shift := min(max(count-1, 0), 4)
	d := time.Minute << shift
	if d > mergeReadBackMax {
		return mergeReadBackMax
	}
	return d
}

type pinnedMerge struct {
	repo, base, head, method string
	number                   int64
}

func pinMerge(o approval.Object) (pinnedMerge, error) {
	if err := approval.Merge.CheckParams(o); err != nil {
		return pinnedMerge{}, err
	}
	repo, base := o["repo"].(string), o["base"].(string)
	for _, segment := range strings.Split(repo+"/"+base, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return pinnedMerge{}, refusal("destination_changed")
		}
	}
	return pinnedMerge{repo, base, o["head"].(string), o["method"].(string), o["number"].(int64)}, nil
}

func (m pinnedMerge) repoPath() string { return "/repos/" + m.repo }
func (m pinnedMerge) pullPath() string {
	return m.repoPath() + "/pulls/" + strconv.FormatInt(m.number, 10)
}

type githubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

type githubPull struct {
	State          string     `json:"state"`
	Merged         *bool      `json:"merged"`
	Mergeable      *bool      `json:"mergeable"`
	MergeableState string     `json:"mergeable_state"`
	Title          string     `json:"title"`
	MergeCommit    string     `json:"merge_commit_sha"`
	MergedBy       githubUser `json:"merged_by"`
	Base           struct {
		Ref  string `json:"ref"`
		Repo struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type githubAPI struct {
	*provider
	key   keychain.Secret
	reads *budget
}

func (a githubAPI) get(ctx context.Context, path string, q url.Values) (answer, error) {
	return a.call(ctx, a.key, a.reads, http.MethodGet, path, q, nil)
}

func (h *mergeHandler) identify(ctx context.Context, a githubAPI) (githubUser, error) {
	var user githubUser
	ans, err := a.get(ctx, "/user", nil)
	if err != nil {
		return user, err
	}
	if err := ans.decode(&user); err != nil {
		return user, err
	}
	if user.Type != "User" {
		return user, refusal("not_a_user")
	}
	if user.ID != h.user {
		return user, refusal("account_mismatch")
	}
	return user, nil
}

func (a githubAPI) pull(ctx context.Context, m pinnedMerge) (githubPull, error) {
	var p githubPull
	ans, err := a.get(ctx, m.pullPath(), nil)
	if err != nil {
		return p, err
	}
	if err := ans.decode(&p); err != nil {
		return p, err
	}
	if p.Base.Repo.ID <= 0 || p.Base.Repo.ID > 1<<53-1 || p.Merged == nil ||
		p.Base.Ref == "" || !sha40.MatchString(p.Head.SHA) || (p.State != "open" && p.State != "closed") {
		return p, refusal("unavailable")
	}
	return p, nil
}

func (p githubPull) matches(m pinnedMerge, repoID int64) error {
	switch {
	case !p.sameRepo(m, repoID):
		return refusal("repository_changed")
	case *p.Merged:
		return refusal("already_merged")
	case p.State != "open":
		return refusal("not_open")
	case p.Base.Ref != m.base:
		return refusal("base_changed")
	case p.Head.SHA != m.head:
		return refusal("head_moved")
	}
	return nil
}

func (p githubPull) sameRepo(m pinnedMerge, repoID int64) bool {
	return (repoID == 0 || p.Base.Repo.ID == repoID) &&
		(p.Base.Repo.FullName == "" || strings.EqualFold(p.Base.Repo.FullName, m.repo))
}

func (h *mergeHandler) precheck(ctx context.Context, api githubAPI, m pinnedMerge, repoID int64) (approval.Resolved, error) {
	user, err := h.identify(ctx, api)
	if err != nil {
		return approval.Resolved{}, err
	}
	p, err := api.pull(ctx, m)
	if err != nil {
		return approval.Resolved{}, err
	}
	if err := p.matches(m, repoID); err != nil {
		return approval.Resolved{}, err
	}
	if p.Mergeable == nil {
		return approval.Resolved{}, refusal("mergeability_unknown")
	}
	if !*p.Mergeable || (p.MergeableState != "clean" && p.MergeableState != "unstable" && p.MergeableState != "has_hooks") {
		return approval.Resolved{}, refusal("not_mergeable")
	}
	if err := api.reviews(ctx, m); err != nil {
		return approval.Resolved{}, err
	}
	required, err := api.requiredChecks(ctx, m)
	if err != nil {
		return approval.Resolved{}, err
	}
	report, err := api.checks(ctx, m, required)
	if err != nil {
		return approval.Resolved{}, err
	}
	if len(report.failed)+len(report.pending) > 0 {
		return approval.Resolved{}, approval.Refusal{Code: "checks_not_green", Detail: report.summary()}
	}
	name := m.repo
	if strings.EqualFold(p.Base.Repo.FullName, name) {
		name = p.Base.Repo.FullName
	}
	return approval.Resolved{Binding: approval.Binding{Origin: h.binding.Origin, Account: h.binding.Account,
		Destination: approval.Object{"repository_id": p.Base.Repo.ID, "pr": m.number, "base": m.base}},
		AccountLabel: oneLine(user.Login, 80), DestinationLabel: fmt.Sprintf("%s · #%d · %s", m.base, m.number, name),
		Facts: approval.Object{"Title": oneLine(p.Title, 256), "Mergeable": p.MergeableState,
			"Reviews": "no changes requested", "Checks": report.summary(), "Head": m.head}}, nil
}

func (h *mergeHandler) Resolve(ctx context.Context, p approval.Proposal) (approval.Resolved, error) {
	m, err := pinMerge(p.Params)
	if err != nil {
		return approval.Resolved{}, err
	}
	leave, err := h.preflight.enter()
	if err != nil {
		return approval.Resolved{}, err
	}
	defer leave()
	ctx, cancel := context.WithTimeout(ctx, readDeadline)
	defer cancel()
	read, err := h.reader()
	if err != nil {
		return approval.Resolved{}, err
	}
	defer read.Clear()
	return h.precheck(ctx, githubAPI{h.provider, read, h.preflight}, m, 0)
}

func (h *mergeHandler) save(ctx context.Context, read, write keychain.Secret, key []byte) error {
	leave, err := h.preflight.enter()
	if err != nil {
		return err
	}
	defer leave()
	ctx, cancel := context.WithTimeout(ctx, readDeadline)
	defer cancel()
	for _, token := range []keychain.Secret{read, write} {
		if _, err := h.identify(ctx, githubAPI{h.provider, token, h.preflight}); err != nil {
			return err
		}
	}
	return h.store(read, write, key)
}

func (h *mergeHandler) Run(ctx context.Context, j *approval.Journal, v approval.View, write keychain.Secret) error {
	if !j.MayDispatch(v.ID) {
		return stopBeforeDispatch(j, v.ID, h.now())
	}
	m, repoID, err := mergeSnapshot(v.Frozen)
	api := githubAPI{h.provider, write, h.final}
	if err == nil {
		err = h.finalReads(ctx, api, m, repoID)
	}
	if err != nil {
		return h.fail(j, v.ID, "pre_dispatch", err)
	}
	if ctx.Err() != nil || !j.ClaimDispatch(v.ID) {
		return stopBeforeDispatch(j, v.ID, h.now())
	}
	a, err := h.call(ctx, write, nil, http.MethodPut, m.pullPath()+"/merge", nil,
		map[string]string{"sha": m.head, "merge_method": m.method})
	reason := "reply_lost"
	if err == nil {
		var reply struct {
			Merged bool   `json:"merged"`
			SHA    string `json:"sha"`
		}
		switch {
		case a.status == 200 && a.decode(&reply) == nil && reply.Merged && sha40.MatchString(reply.SHA):
			return h.merged(j, v.ID, reply.SHA, "reply")
		case a.status >= 400 && a.status < 500:
			return h.fail(j, v.ID, "reply", refusal(mergeRejection(a.status)))
		case a.status >= 500:
			reason = "server_error"
		default:
			reason = "reply_unclear"
		}
	}
	if err := j.Unknown(v.ID, reason, h.now()); err != nil {
		return err
	}
	return h.Reconcile(ctx, j, v.ID)
}

func mergeSnapshot(f *approval.Frozen) (pinnedMerge, int64, error) {
	if f == nil {
		return pinnedMerge{}, 0, refusal("destination_changed")
	}
	m, err := pinMerge(f.Params)
	if err != nil {
		return m, 0, err
	}
	repo, _ := f.Destination["repository_id"].(int64)
	if repo <= 0 || f.Destination["pr"] != m.number || f.Destination["base"] != m.base {
		return m, 0, refusal("destination_changed")
	}
	return m, repo, nil
}

func (h *mergeHandler) finalReads(ctx context.Context, api githubAPI, m pinnedMerge, repoID int64) error {
	leave, err := api.reads.enter()
	if err != nil {
		return err
	}
	defer leave()
	ctx, cancel := context.WithTimeout(ctx, readDeadline)
	defer cancel()
	if _, err := h.precheck(ctx, api, m, repoID); err != nil {
		return err
	}
	p, err := api.pull(ctx, m)
	if err != nil {
		return err
	}
	return p.matches(m, repoID)
}

func mergeRejection(status int) string {
	switch status {
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 405:
		return "not_mergeable"
	case 409:
		return "head_moved"
	case 422:
		return "rejected"
	default:
		return "github_" + strconv.Itoa(status)
	}
}

func (h *mergeHandler) merged(j *approval.Journal, id, sha, by string) error {
	r := approval.Result{ConfirmedBy: by, Native: "github:merge:" + sha}
	if by == "reply" {
		r.Text = "Merged as " + sha[:7] + "."
	} else {
		r.Evidence = "Read back: merged as " + sha[:7] + " by this account."
	}
	return j.SetResult(id, r, h.now())
}

func (h *mergeHandler) Due(v approval.View) bool {
	if v.Status != "outcome_unknown" || !h.bound(v.Frozen) {
		return false
	}
	a, seen := h.readBacks.get(v.ID)
	if !seen {
		return true // one read after launch, even long after the reply was lost
	}
	at := h.now()
	delay := time.Minute
	if !v.UnknownAt.IsZero() && at.Sub(v.UnknownAt) >= mergeSettle {
		delay = mergeReadBackDelay(a.count)
	}
	return at.Sub(a.last) >= delay
}

func (h *mergeHandler) Reconcile(ctx context.Context, j *approval.Journal, id string) error {
	v, done, ok := h.readView(ctx, j, id, approval.Merge)
	if !ok {
		return nil
	}
	defer done()
	m, repoID, err := mergeSnapshot(v.Frozen)
	if err != nil {
		return nil
	}
	h.readBacks.mark(id, h.now())
	leave, err := h.recovery.enter()
	if err != nil {
		return nil
	}
	defer leave()
	ctx, cancel := context.WithTimeout(ctx, readDeadline)
	defer cancel()
	read, err := h.reader()
	if err != nil {
		return nil
	}
	defer read.Clear()
	api := githubAPI{h.provider, read, h.recovery}
	if _, err := h.identify(ctx, api); err != nil {
		return nil
	}
	p, err := api.pull(ctx, m)
	if err != nil || !p.sameRepo(m, repoID) {
		return nil
	}
	if *p.Merged {
		h.forgetOpen(id)
		// A missing actor, a different actor, or a merge without a commit SHA is not this approval.
		// Leaving it unknown would read it again every minute and spend the shared recovery budget.
		if p.MergedBy.ID == h.user && p.Head.SHA == m.head && p.Base.Ref == m.base && sha40.MatchString(p.MergeCommit) {
			return h.merged(j, id, p.MergeCommit, "read_back")
		}
		return h.fail(j, id, "read_back", refusal("merged_otherwise"))
	}
	at := h.now()
	if p.State != "open" || !v.UnknownAt.IsZero() && at.Sub(v.UnknownAt) >= mergeSettle && h.openSettled(id, at) {
		h.forgetOpen(id)
		return h.fail(j, id, "read_back", refusal("not_merged"))
	}
	return nil
}

func (h *mergeHandler) openSettled(id string, at time.Time) bool {
	h.openMu.Lock()
	defer h.openMu.Unlock()
	if h.firstOpen == nil {
		h.firstOpen = make(map[string]time.Time)
	}
	first, ok := h.firstOpen[id]
	if !ok || at.Before(first) {
		h.firstOpen[id] = at
		return false
	}
	return at.Sub(first) >= mergeSettle
}

func (h *mergeHandler) forgetOpen(id string) {
	h.openMu.Lock()
	defer h.openMu.Unlock()
	delete(h.firstOpen, id)
}
