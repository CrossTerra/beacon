package github

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

// Fields is the source's fixed field list (DEC-6).
var Fields = []string{"repo", "number", "title", "url", "author", "review_state", "body", "base", "head_ref"}

// StateFields is the source's fixed state field list (DEC-28). checks is pass, fail, pending or none.
var StateFields = []string{"state", "draft", "head_sha", "review_decision", "mergeable", "checks", "requested_reviewers"}

// Item types (DEC-18).
const (
	TypeOpened               = "opened"
	TypeReady                = "ready"
	TypeReviewRequested      = "review_requested"
	TypeReviewRequestRemoved = "review_request_removed"
	TypeReviewed             = "reviewed"
	TypeCommented            = "commented"
	TypePushed               = "pushed"
	TypeChecksChanged        = "checks_changed"
	TypeMerged               = "merged"
	TypeClosed               = "closed"
)

var issueKeyPattern = regexp.MustCompile(`[A-Z][A-Z0-9]+-\d+`)

type ghTeam struct {
	Slug string `json:"slug"`
}

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type pull struct {
	ID        int64   `json:"id"`
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	HTMLURL   string  `json:"html_url"`
	State     string  `json:"state"`
	Draft     bool    `json:"draft"`
	Merged    bool    `json:"merged"`
	User      *ghUser `json:"user"`
	Body      string  `json:"body"`
	Created   string  `json:"created_at"`
	Mergeable *bool   `json:"mergeable"`
	Base      *struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head *struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	RequestedReviewers []ghUser `json:"requested_reviewers"`
	RequestedTeams     []struct {
		Slug string `json:"slug"`
	} `json:"requested_teams"`
	Assignees []ghUser `json:"assignees"`
	owner     string
	name      string
}

func (p pull) author() string {
	if p.User == nil {
		return ""
	}
	return p.User.Login
}

func (p pull) headSHA() string {
	if p.Head == nil {
		return ""
	}
	return p.Head.SHA
}

func (p pull) headRef() string {
	if p.Head == nil {
		return ""
	}
	return p.Head.Ref
}

func (p pull) baseRef() string {
	if p.Base == nil {
		return ""
	}
	return p.Base.Ref
}

type timelineEvent struct {
	ID                flexID  `json:"id"`
	Event             string  `json:"event"`
	SHA               string  `json:"sha"`
	CreatedAt         string  `json:"created_at"`
	SubmittedAt       string  `json:"submitted_at"`
	User              *ghUser `json:"user"`
	Actor             *ghUser `json:"actor"`
	State             string  `json:"state"`
	Body              string  `json:"body"`
	ReviewRequester   *ghUser `json:"review_requester"`
	RequestedReviewer *ghUser `json:"requested_reviewer"`
	RequestedTeam     *ghTeam `json:"requested_team"`
	CommitID          string  `json:"commit_id"`
	After             string  `json:"after"`
	Author            *ghUser `json:"author"`
	Committer         *ghUser `json:"committer"`
}

type review struct {
	ID          flexID  `json:"id"`
	User        *ghUser `json:"user"`
	State       string  `json:"state"`
	Body        string  `json:"body"`
	SubmittedAt string  `json:"submitted_at"`
	CommitID    string  `json:"commit_id"`
}

type reviewComment struct {
	ID        flexID  `json:"id"`
	User      *ghUser `json:"user"`
	Body      string  `json:"body"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

type checkSuite struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	UpdatedAt  string `json:"updated_at"`
	HeadSHA    string `json:"head_sha"`
}

type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		s = strings.Trim(s, `"`)
	}
	*f = flexID(s)
	return nil
}

func (f flexID) String() string { return string(f) }

// buildInput is everything one pull request yielded. build turns it into items.
type buildInput struct {
	Name     string
	PR       pull
	Timeline []timelineEvent
	Reviews  []review
	Comments []reviewComment
	Checks   []checkSuite
	Fetched  bool // checks were requested
	Login    string
	Teams    map[string]bool
	Since    time.Time
	First    bool
	Observed time.Time
	Projects map[string]bool
}

func build(in buildInput) ([]source.Item, error) {
	pr := in.PR
	if pr.author() == "" || pr.Number <= 0 || pr.owner == "" || pr.name == "" {
		return nil, fmt.Errorf("github: a pull request is missing its author or repository")
	}
	obs := in.Observed.UTC()
	snap := snapshot(pr, in.Reviews, in.Checks, in.Fetched)
	var items []source.Item
	add := func(it source.Item, err error) error {
		if err != nil {
			return err
		}
		if !it.OccurredAt.IsZero() {
			items = append(items, it)
		}
		return nil
	}
	if in.First {
		if strings.EqualFold(pr.State, "open") && !pr.Merged {
			at, err := parseTime(pr.Created)
			if err != nil {
				return nil, err
			}
			if err := add(in.item(TypeOpened, "opened:"+strconv.FormatInt(pr.ID, 10), pr.author(), at, "", "", mentions(pr.Body, in.Login) || mentions(pr.Title, in.Login))); err != nil {
				return nil, err
			}
			reqs, err := currentRequests(in, obs)
			if err != nil {
				return nil, err
			}
			items = append(items, reqs...)
			if strings.EqualFold(pr.author(), in.Login) && in.Fetched {
				if it, ok := checksItem(in, obs); ok {
					items = append(items, it)
				}
			}
		}
		return stamp(items, snap, obs, in), nil
	}
	created, err := parseTime(pr.Created)
	if err == nil && !created.Before(in.Since) {
		if err := add(in.item(TypeOpened, "opened:"+strconv.FormatInt(pr.ID, 10), pr.author(), created, "", "", mentions(pr.Body, in.Login) || mentions(pr.Title, in.Login))); err != nil {
			return nil, err
		}
	}
	merged := pr.Merged
	for _, e := range in.Timeline {
		if e.Event == "merged" {
			merged = true
		}
	}
	for _, e := range in.Timeline {
		at := eventTime(e)
		if at.IsZero() || at.Before(in.Since) {
			continue
		}
		var err error
		switch e.Event {
		case "ready_for_review":
			err = add(in.item(TypeReady, "timeline:"+e.ID.String(), loginOf(e.Actor), at, "", "", false))
		case "review_requested":
			who := loginOf(e.RequestedReviewer)
			actor := loginOf(e.ReviewRequester)
			if actor == "" {
				actor = loginOf(e.Actor)
			}
			err = add(in.item(TypeReviewRequested, "timeline:"+e.ID.String(), actor, at, "", "", in.addressedRequest(who, teamSlug(e.RequestedTeam))))
		case "review_request_removed":
			actor := loginOf(e.Actor)
			if actor == "" {
				actor = loginOf(e.ReviewRequester)
			}
			err = add(in.item(TypeReviewRequestRemoved, "timeline:"+e.ID.String(), actor, at, "", "", false))
		case "reviewed":
			actor := loginOf(e.User)
			err = add(in.item(TypeReviewed, "review:"+e.ID.String(), actor, at, strings.ToLower(e.State), e.Body, mentions(e.Body, in.Login)))
		case "commented":
			actor := loginOf(e.User)
			err = add(in.item(TypeCommented, "comment:"+e.ID.String(), actor, at, "", e.Body, mentions(e.Body, in.Login)))
		case "committed", "head_ref_force_pushed":
			// Commit author time is often older than the push. Do not filter pushes by that time; emit below.
			continue
		case "merged":
			err = add(in.item(TypeMerged, "timeline:"+e.ID.String(), loginOf(e.Actor), at, "", "", false))
		case "closed":
			if merged {
				continue
			}
			err = add(in.item(TypeClosed, "timeline:"+e.ID.String(), loginOf(e.Actor), at, "", "", false))
		}
		if err != nil {
			return nil, err
		}
	}
	if err := in.addPushes(add); err != nil {
		return nil, err
	}
	for _, c := range in.Comments {
		at, err := parseTime(c.CreatedAt)
		if err != nil || at.Before(in.Since) {
			continue
		}
		if err := add(in.item(TypeCommented, "review-comment:"+c.ID.String(), loginOf(c.User), at, "", c.Body, mentions(c.Body, in.Login))); err != nil {
			return nil, err
		}
	}
	if strings.EqualFold(pr.author(), in.Login) && strings.EqualFold(pr.State, "open") && !pr.Merged && in.Fetched {
		if it, ok := checksItem(in, obs); ok && !it.OccurredAt.Before(in.Since) {
			items = append(items, it)
		}
	}
	return stamp(items, snap, obs, in), nil
}

func currentRequests(in buildInput, obs time.Time) ([]source.Item, error) {
	pending := map[string]bool{}
	for _, u := range in.PR.RequestedReviewers {
		pending[strings.ToLower(u.Login)] = true
	}
	pendingTeams := map[string]bool{}
	for _, t := range in.PR.RequestedTeams {
		if t.Slug != "" {
			pendingTeams[strings.ToLower(t.Slug)] = true
		}
	}
	var items []source.Item
	seen := map[string]bool{}
	for _, e := range in.Timeline {
		if e.Event != "review_requested" {
			continue
		}
		who := loginOf(e.RequestedReviewer)
		team := teamSlug(e.RequestedTeam)
		userPending := who != "" && pending[strings.ToLower(who)] && !seen[strings.ToLower(who)]
		teamPending := team != "" && pendingTeams[strings.ToLower(team)] && in.Teams[strings.ToLower(team)] && !seen["team:"+strings.ToLower(team)]
		if !userPending && !teamPending {
			continue
		}
		if who != "" {
			seen[strings.ToLower(who)] = true
		}
		if team != "" {
			seen["team:"+strings.ToLower(team)] = true
		}
		at := eventTime(e)
		if at.IsZero() {
			at = obs
		}
		actor := loginOf(e.ReviewRequester)
		it, err := in.item(TypeReviewRequested, "timeline:"+e.ID.String(), actor, at, "", "", in.addressedRequest(who, team))
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

func checksItem(in buildInput, obs time.Time) (source.Item, bool) {
	conclusion := aggregateChecks(in.Checks)
	if conclusion == "none" && len(in.Checks) == 0 {
		return source.Item{}, false
	}
	sha := in.PR.headSHA()
	if sha == "" {
		return source.Item{}, false
	}
	at := time.Time{}
	for _, c := range in.Checks {
		if t, err := parseTime(c.UpdatedAt); err == nil && (at.IsZero() || t.After(at)) {
			at = t
		}
	}
	if at.IsZero() {
		at = obs
	}
	id := "checks:" + sha + ":" + conclusion
	// No actor and not own: a check finishing is not the person's act, and must not close their asks.
	it, err := in.item(TypeChecksChanged, id, "", at, "", "", false)
	if err != nil {
		return source.Item{}, false
	}
	return it, true
}

func stamp(items []source.Item, snap map[string]string, obs time.Time, in buildInput) []source.Item {
	refs := references(in.PR.Title, in.PR.headRef(), in.Projects)
	seen := map[string]bool{}
	var out []source.Item
	for _, it := range items {
		if it.Key == "" || seen[it.Key] {
			continue
		}
		seen[it.Key] = true
		it.State = snap
		it.ObservedAt = &obs
		it.References = refs
		out = append(out, it)
	}
	return out
}

func (in buildInput) item(typ, changeID, actorLogin string, at time.Time, reviewState, body string, addressed bool) (source.Item, error) {
	if changeID == "" {
		return source.Item{}, fmt.Errorf("github: a %s has no id", typ)
	}
	pr := in.PR
	fields := map[string]string{
		"repo":   pr.owner + "/" + pr.name,
		"number": strconv.Itoa(pr.Number),
		"title":  pr.Title,
		"url":    pr.HTMLURL,
		"author": pr.author(),
	}
	if pr.baseRef() != "" {
		fields["base"] = pr.baseRef()
	}
	if pr.headRef() != "" {
		fields["head_ref"] = pr.headRef()
	}
	if reviewState != "" {
		fields["review_state"] = reviewState
	}
	if body != "" {
		fields["body"] = body
	}
	actor := ""
	if actorLogin != "" && !strings.Contains(actorLogin, "@") {
		actor = "github:user:" + actorLogin
	} else if strings.Contains(actorLogin, "@") {
		actor = "github:email:" + actorLogin
	}
	own := actorLogin != "" && strings.EqualFold(actorLogin, in.Login)
	return source.Item{
		Source:     in.Name,
		Subject:    fmt.Sprintf("github:pr:%s/%s#%d", pr.owner, pr.name, pr.Number),
		Key:        fmt.Sprintf("github:%s/%s#%d:%s", pr.owner, pr.name, pr.Number, changeID),
		Type:       typ,
		OccurredAt: at.UTC(),
		Actor:      actor,
		Own:        own,
		Addressed:  addressed,
		Fields:     fields,
		Version:    changeID,
	}, nil
}

func snapshot(pr pull, reviews []review, checks []checkSuite, fetched bool) map[string]string {
	state := strings.ToLower(pr.State)
	if pr.Merged {
		state = "merged"
	}
	draft := "false"
	if pr.Draft {
		draft = "true"
	}
	mergeable := "unknown"
	if pr.Mergeable != nil {
		mergeable = strconv.FormatBool(*pr.Mergeable)
	}
	var reviewers []string
	for _, u := range pr.RequestedReviewers {
		if u.Login != "" {
			reviewers = append(reviewers, u.Login)
		}
	}
	sort.Strings(reviewers)
	st := map[string]string{
		"state":               state,
		"draft":               draft,
		"head_sha":            pr.headSHA(),
		"mergeable":           mergeable,
		"requested_reviewers": strings.Join(reviewers, ","),
	}
	if d := reviewDecision(reviews, len(reviewers)); d != "" {
		st["review_decision"] = d
	}
	if fetched {
		st["checks"] = aggregateChecks(checks)
	}
	return st
}

func reviewDecision(reviews []review, requested int) string {
	latest := map[string]string{}
	for _, r := range reviews {
		login := loginOf(r.User)
		if login == "" || strings.EqualFold(r.State, "pending") {
			continue
		}
		latest[strings.ToLower(login)] = strings.ToLower(r.State)
	}
	changes, approved := false, false
	for _, st := range latest {
		switch st {
		case "changes_requested":
			changes = true
		case "approved":
			approved = true
		}
	}
	switch {
	case changes:
		return "changes_requested"
	case approved:
		return "approved"
	case requested > 0:
		return "review_required"
	default:
		return ""
	}
}

func aggregateChecks(suites []checkSuite) string {
	if len(suites) == 0 {
		return "none"
	}
	fail, pending, pass := false, false, false
	for _, s := range suites {
		if !strings.EqualFold(s.Status, "completed") {
			pending = true
			continue
		}
		switch strings.ToLower(s.Conclusion) {
		case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
			fail = true
		case "success", "neutral", "skipped":
			pass = true
		default:
			pending = true
		}
	}
	switch {
	case fail:
		return "fail"
	case pending:
		return "pending"
	case pass:
		return "pass"
	default:
		return "none"
	}
}

func (in buildInput) addressedRequest(login, team string) bool {
	if strings.EqualFold(login, in.Login) {
		return true
	}
	return team != "" && in.Teams[strings.ToLower(team)]
}

func teamSlug(t *ghTeam) string {
	if t == nil {
		return ""
	}
	return t.Slug
}

func inTeam(t *ghTeam, teams map[string]bool) bool {
	slug := teamSlug(t)
	return slug != "" && teams[strings.ToLower(slug)]
}

// addPushes emits a push when the head moved after the person's last review, ignoring git commit time. A rebased
// commit's author date is often before the cursor even though the push is new. The key is the sha, so a later pass
// that sees it again does not send twice.
func (in buildInput) addPushes(add func(source.Item, error) error) error {
	if !personReviewed(in.Timeline, in.Reviews, in.Login) {
		return nil
	}
	last := -1
	lastSHA := ""
	for i, e := range in.Timeline {
		if e.Event == "reviewed" && strings.EqualFold(loginOf(e.User), in.Login) && !strings.EqualFold(e.State, "pending") {
			last = i
			if e.CommitID != "" {
				lastSHA = e.CommitID
			}
		}
	}
	for _, r := range in.Reviews {
		if strings.EqualFold(loginOf(r.User), in.Login) && r.CommitID != "" {
			lastSHA = r.CommitID
		}
	}
	seen := map[string]bool{}
	emit := func(sha, actor string, at time.Time) error {
		if sha == "" || seen[sha] || sha == lastSHA {
			return nil
		}
		seen[sha] = true
		if at.IsZero() {
			at = in.Observed
		}
		return add(in.item(TypePushed, "push:"+sha, actor, at, "", "", false))
	}
	for i, e := range in.Timeline {
		if last >= 0 && i <= last {
			continue
		}
		switch e.Event {
		case "committed":
			if err := emit(e.SHA, loginOf(e.Author), eventTime(e)); err != nil {
				return err
			}
		case "head_ref_force_pushed":
			if err := emit(e.After, loginOf(e.Actor), eventTime(e)); err != nil {
				return err
			}
		}
	}
	if head := in.PR.headSHA(); head != "" && head != lastSHA {
		return emit(head, "", in.Observed)
	}
	return nil
}

func reviewRequested(pr pull, login string, teams map[string]bool) bool {
	for _, u := range pr.RequestedReviewers {
		if strings.EqualFold(u.Login, login) {
			return true
		}
	}
	for _, t := range pr.RequestedTeams {
		if teams[strings.ToLower(t.Slug)] {
			return true
		}
	}
	return false
}

// involved reports whether the person is an author, assignee, reviewer, requested reviewer, mention, or commenter.
func involved(pr pull, events []timelineEvent, reviews []review, comments []reviewComment, login string, teams map[string]bool) bool {
	if strings.EqualFold(pr.author(), login) || reviewRequested(pr, login, teams) {
		return true
	}
	for _, u := range pr.Assignees {
		if strings.EqualFold(u.Login, login) {
			return true
		}
	}
	if mentions(pr.Title, login) || mentions(pr.Body, login) {
		return true
	}
	if personReviewed(events, reviews, login) {
		return true
	}
	for _, e := range events {
		if e.Event == "commented" && strings.EqualFold(loginOf(e.User), login) {
			return true
		}
		if e.Event == "review_requested" && (strings.EqualFold(loginOf(e.RequestedReviewer), login) || inTeam(e.RequestedTeam, teams)) {
			return true
		}
		if e.Event == "commented" && mentions(e.Body, login) {
			return true
		}
		if e.Event == "reviewed" && mentions(e.Body, login) {
			return true
		}
	}
	for _, c := range comments {
		if strings.EqualFold(loginOf(c.User), login) || mentions(c.Body, login) {
			return true
		}
	}
	return false
}

func personReviewed(events []timelineEvent, reviews []review, login string) bool {
	for _, e := range events {
		if e.Event == "reviewed" && strings.EqualFold(loginOf(e.User), login) && !strings.EqualFold(e.State, "pending") {
			return true
		}
	}
	for _, r := range reviews {
		if strings.EqualFold(loginOf(r.User), login) && !strings.EqualFold(r.State, "pending") {
			return true
		}
	}
	return false
}

func loginOf(u *ghUser) string {
	if u == nil {
		return ""
	}
	if u.Login != "" {
		return u.Login
	}
	return u.Email
}

func eventTime(e timelineEvent) time.Time {
	for _, s := range []string{e.SubmittedAt, e.CreatedAt, dateOf(e.Author), dateOf(e.Committer)} {
		if t, err := parseTime(s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func dateOf(u *ghUser) string {
	if u == nil {
		return ""
	}
	return u.Date
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty")
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05.000-0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time")
}

func mentions(text, login string) bool {
	if text == "" || login == "" {
		return false
	}
	re := regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9-])@` + regexp.QuoteMeta(login) + `(?:[^A-Za-z0-9-]|$)`)
	return re.MatchString(text)
}

func references(title, head string, projects map[string]bool) []string {
	if len(projects) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, text := range []string{title, head} {
		for _, m := range issueKeyPattern.FindAllString(text, -1) {
			proj := m[:strings.LastIndex(m, "-")]
			if !projects[proj] || seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, "jira:"+m)
		}
	}
	sort.Strings(out)
	return out
}
