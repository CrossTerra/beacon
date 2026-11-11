package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

// GmailFields is Gmail's fixed field list (DEC-6). body is read only when a rule lists it.
var GmailFields = []string{"from", "to", "subject", "date", "labels", "link", "body"}

// GmailStateFields is Gmail's state field list (DEC-28), over the whole thread.
var GmailStateFields = []string{"unread", "starred", "labels", "last_from"}

// Gmail item types (DEC-18).
const (
	TypeReceived = "received"
	TypeSent     = "sent"
	TypeStarred  = "starred"
)

// gmailLookback is how far back a first run reads (DEC-23).
const gmailLookback = 14 * 24 * time.Hour

// Gmail reads the person's mailbox through the history API.
type Gmail struct {
	common
	quota *gmailQuota
	// limit is the last rate limit Google answered in this collect, for the log.
	limit *source.RateLimit
}

// NewGmail builds the Gmail source. It reads no credential.
func NewGmail(o Options) (*Gmail, error) {
	c, err := newCommon(o, "gmail", GmailBase, gmailPath)
	if err != nil {
		return nil, err
	}
	g := &Gmail{common: c, quota: &gmailQuota{now: c.now, sleep: o.Sleep}}
	// Every request Google sees is counted, a retry included: the HTTP client sends each request once, and Gmail
	// does its own backoff (getJSON). The quota wait comes before the access token is read, so a token that expires
	// during a wait is refreshed, not sent. Calls are made one at a time: a source collects on one goroutine.
	g.http.MaxAttempts = 1
	g.pace = func(ctx context.Context, u *url.URL) error {
		return g.quota.take(ctx, gmailPathUnits(u.Path))
	}
	return g, nil
}

func gmailPath(p string, q url.Values) bool {
	const prefix = "/gmail/v1/users/me/"
	rest, ok := strings.CutPrefix(p, prefix)
	if !ok {
		return false
	}
	switch rest {
	case "profile", "labels":
		return onlyKeys(q)
	case "history":
		return onlyKeys(q, "startHistoryId", "historyTypes", "maxResults", "pageToken")
	case "threads", "messages":
		return onlyKeys(q, "labelIds", "maxResults", "pageToken")
	}
	kind, id, ok := strings.Cut(rest, "/")
	if !ok || !idPattern.MatchString(id) {
		return false
	}
	switch kind {
	case "threads":
		// Headers only.
		return onlyKeys(q, "format", "metadataHeaders") && q.Get("format") == "metadata"
	case "messages":
		// A body, read only when a rule lists body. Never raw, never an attachment.
		return onlyKeys(q, "format") && q.Get("format") == "full"
	}
	return false
}

// Fields implements source.Source.
func (g *Gmail) Fields() []string { return append([]string(nil), GmailFields...) }

// StateFields implements source.Source.
func (g *Gmail) StateFields() []string { return append([]string(nil), GmailStateFields...) }

type gmailMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	InternalDate string   `json:"internalDate"`
	Payload      struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	} `json:"payload"`
}

func (m *gmailMessage) header(name string) string {
	for _, h := range m.Payload.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

func (m *gmailMessage) has(label string) bool {
	for _, l := range m.LabelIDs {
		if l == label {
			return true
		}
	}
	return false
}

func (m *gmailMessage) at() time.Time {
	ms, err := strconv.ParseInt(m.InternalDate, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// skipped is mail that is not a message anyone sent or received: drafts, spam, trash, chats.
func (m *gmailMessage) skipped() bool {
	return m.has("DRAFT") || m.has("SPAM") || m.has("TRASH") || m.has("CHAT")
}

type gmailThread struct {
	ID       string         `json:"id"`
	Messages []gmailMessage `json:"messages"`
}

// live is the thread's messages that count, oldest first.
func (t *gmailThread) live() []*gmailMessage {
	var out []*gmailMessage
	for i := range t.Messages {
		if !t.Messages[i].skipped() {
			out = append(out, &t.Messages[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at().Before(out[j].at()) })
	return out
}

// ref is one change the history or the first run found: a message added, or starred.
// body is set when the open-state scan already read it, before the thread was marked done.
type ref struct {
	thread  string
	message string
	starred bool
	body    string
}

// Collect implements source.Source. The cursor is a Gmail history id. A nil cursor reads inbox threads and starred
// mail from the last 14 days (DEC-23).
func (g *Gmail) Collect(ctx context.Context, cursor []byte) (source.Collected, error) {
	scopes, err := g.grant(ctx, []string{ScopeGmailMetadata, ScopeGmailReadonly})
	if err != nil {
		return source.Collected{}, err
	}
	g.clock.Reset()
	g.limit = nil
	var cur gmailCursor
	if len(cursor) > 0 {
		if cur, err = parseGmailCursor(cursor); err != nil {
			return source.Collected{}, err
		}
	}
	var profile struct {
		EmailAddress string `json:"emailAddress"`
		HistoryID    string `json:"historyId"`
	}
	if err := g.getJSON(ctx, "profile", nil, &profile); err != nil {
		return source.Collected{}, err
	}
	me := strings.ToLower(profile.EmailAddress)
	if me == "" || profile.HistoryID == "" {
		return source.Collected{}, errors.New("gmail: the profile could not be read")
	}
	if err := g.checkIdentity(me); err != nil {
		return source.Collected{}, err
	}
	labels, err := g.labels(ctx)
	if err != nil {
		return source.Collected{}, err
	}
	threads := map[string]*gmailThread{}
	var refs []ref
	gap, cut := false, false
	var openCut bool
	next := gmailCursor{HistoryID: profile.HistoryID, At: cur.At}
	// A scan cursor is an open-state read that stopped at a rate limit. It is not a history cursor.
	if cur.Scan != nil && cur.Scan.By != scanBySend {
		// A resume cursor a dry run left, or one this process did not write on a send pass, is not progress.
		// Trusting it would skip threads that were printed and never sent. Start the scan again.
		cur = gmailCursor{}
		cursor = nil
	}
	openState := len(cursor) == 0 || cur.Scan != nil
	var scan *gmailScan
	if openState {
		hist := profile.HistoryID
		if cur.Scan != nil && cur.HistoryID != "" {
			// Keep the history id read before the scan started, so mail that arrives while it runs is not missed.
			hist = cur.HistoryID
		}
		next.HistoryID = hist
		refs, openCut, scan, err = g.open(ctx, threads, cur.Scan, hist, readBodies(g, scopes))
	} else {
		var got string
		refs, got, err = g.history(ctx, cur.HistoryID)
		if isStatus(err, http.StatusNotFound) {
			// The history id expired, so the changes since it are gone from the API. Read open state again, and
			// every inbox and sent message since the last pass, so earlier messages and the person's own acts in
			// the gap are not lost. Keys make the overlap harmless. The gap is reported.
			gap = true
			var gapScan *gmailScan
			refs, openCut, gapScan, err = g.open(ctx, threads, nil, "", false)
			if gapScan != nil {
				// A gap re-read that hits a rate limit fails the pass. The history cursor stays, so the next pass
				// retries the gap. This is not a first-run resume.
				err = g.limitErr()
			}
			if err == nil {
				var more []ref
				more, cut, err = g.since(ctx, threads, cur.At)
				refs = append(refs, more...)
			}
		} else if got != "" {
			next.HistoryID = got
		}
	}
	if err != nil {
		return source.Collected{}, err
	}
	bodies := g.bodies && scopes[ScopeGmailReadonly] && !scopes[ScopeGmailMetadata]
	var items []source.Item
	seen := map[string]bool{}
	for _, r := range refs {
		t, err := g.thread(ctx, threads, r.thread)
		if errors.Is(err, source.ErrRateLimited) && scan != nil {
			// The resume point is already known. Do not fail the collect and drop it.
			break
		}
		if gone(err) {
			continue
		}
		if err != nil {
			return source.Collected{Items: items}, err
		}
		it, ok := g.item(t, r, me, labels)
		if !ok || seen[it.Key] {
			continue
		}
		seen[it.Key] = true
		if r.body != "" {
			it.Fields["body"] = r.body
		} else if bodies && scan == nil && it.Type != TypeStarred {
			// History, or a finished open scan that did not read bodies (a gap re-read). A body that one message
			// will not give up (gone, or a permission 403) is left out; the item is still emitted. A rate limit
			// or a refused grant fails the pass. An open-state scan reads the body before it marks the thread
			// done, so a pause cannot record the item without it.
			body, err := g.body(ctx, r.message)
			if err != nil && !unavailable(err) {
				return source.Collected{Items: items}, err
			}
			if body != "" {
				it.Fields["body"] = body
			}
		}
		items = append(items, it)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	if scan != nil {
		return g.partial(next, scan, items)
	}
	// The time of this pass bounds a later gap read. It is the server's Date, else the newest message seen.
	if t := g.clock.First(); !t.IsZero() {
		next.At = t.Format(time.RFC3339)
	} else {
		newest, _ := parseTime(next.At)
		for _, it := range items {
			if it.OccurredAt.After(newest) {
				newest = it.OccurredAt
			}
		}
		if !newest.IsZero() {
			next.At = newest.UTC().Format(time.RFC3339)
		}
	}
	data, err := json.Marshal(next)
	if err != nil {
		return source.Collected{Items: items}, err
	}
	return source.Collected{Items: items, Next: data, Gap: gap, Cut: cut || openCut, ClearResume: cur.Scan != nil}, nil
}

// partial is a collect that stopped at a rate limit. Next is where to continue, not a history cursor.
func (g *Gmail) partial(next gmailCursor, scan *gmailScan, items []source.Item) (source.Collected, error) {
	if scan.HistoryID == "" {
		scan.HistoryID = next.HistoryID
	}
	scan.By = scanBySend
	next.HistoryID = scan.HistoryID
	next.Scan = scan
	data, err := json.Marshal(next)
	if err != nil {
		return source.Collected{Items: items}, err
	}
	return source.Collected{Items: items, Next: data, Partial: true, Cut: scan.InboxCut, Limit: g.limit}, nil
}

// limitErr is the rate limit this collect last met, as an error.
func (g *Gmail) limitErr() error {
	if g.limit == nil {
		return source.ErrRateLimited
	}
	return &source.RateLimitError{RateLimit: *g.limit}
}

// gmailCursor is the history id to read from, and when the pass that stored it ran. The time bounds what a pass
// re-reads when the history id has expired. Scan, when set, is an open-state read that stopped at a rate limit.
// It is not a history cursor: the next pass continues the scan, and history is read only once the scan finishes.
type gmailCursor struct {
	HistoryID string     `json:"history_id"`
	At        string     `json:"at,omitempty"`
	Scan      *gmailScan `json:"scan,omitempty"`
}

// gmailScan is where an open-state read stopped. It holds ids and a page token, not message content.
type gmailScan struct {
	HistoryID string `json:"history_id"`
	// Cutoff is the window start, so a later pass uses the same 14 days.
	Cutoff string `json:"cutoff"`
	// Phase is "inbox" or "starred".
	Phase string `json:"phase"`
	// Page is the page token of the list page not yet finished. Empty is the first page of Phase.
	Page string `json:"page,omitempty"`
	// Skip are ids already fetched on Page, so the next pass does not spend quota on them again.
	Skip []string `json:"skip,omitempty"`
	// Pages is how many list pages of Phase were already finished. A resume continues the same cap
	// (gapPages), it does not start the count again.
	Pages int `json:"pages,omitempty"`
	// InboxCut is true when the inbox list already hit its cap, so a later pause does not forget read_cut.
	InboxCut bool `json:"inbox_cut,omitempty"`
	// By is "send" only on a resume a send pass stored. Any other value is discarded: a dry run writes no cursor.
	By string `json:"by,omitempty"`
}

const scanBySend = "send"

func parseGmailCursor(b []byte) (gmailCursor, error) {
	var c gmailCursor
	if err := json.Unmarshal(b, &c); err != nil || c.HistoryID == "" {
		// A bare history id, with no time.
		c = gmailCursor{HistoryID: string(b)}
	}
	if _, err := strconv.ParseUint(c.HistoryID, 10, 64); err != nil {
		return gmailCursor{}, errors.New("gmail: the cursor is not a history id")
	}
	if c.At != "" {
		if _, err := parseTime(c.At); err != nil {
			return gmailCursor{}, errors.New("gmail: the cursor time could not be read")
		}
	}
	if c.Scan != nil {
		if c.Scan.Phase != "inbox" && c.Scan.Phase != "starred" {
			return gmailCursor{}, errors.New("gmail: the cursor could not be read")
		}
		if _, err := parseTime(c.Scan.Cutoff); err != nil {
			return gmailCursor{}, errors.New("gmail: the cursor could not be read")
		}
	}
	return c, nil
}

// gapPages is how many pages of 100 a gap read lists per label, and an open-state read per list.
const gapPages = 50

// since lists inbox and sent messages, newest first, back to 15 minutes before at (14 days when at is unknown). It
// uses labels, not a search: gmail.metadata cannot search. cut reports that a label had more than gapPages pages in
// the gap and the rest was not listed. The pass still succeeds: failing it would leave the expired history id in the
// cursor and stop at the same place on every pass.
func (g *Gmail) since(ctx context.Context, threads map[string]*gmailThread, at string) ([]ref, bool, error) {
	cutoff := g.now().Add(-gmailLookback)
	if t, err := parseTime(at); err == nil && t.Add(-source.Lookback).After(cutoff) {
		cutoff = t.Add(-source.Lookback)
	}
	var refs []ref
	cut := false
	for _, label := range []string{"INBOX", "SENT"} {
		token := ""
		done := false
	pages:
		for pages := 0; pages < gapPages; pages++ {
			q := url.Values{"labelIds": {label}, "maxResults": {"100"}}
			if token != "" {
				q.Set("pageToken", token)
			}
			var page struct {
				Messages []struct {
					ID       string `json:"id"`
					ThreadID string `json:"threadId"`
				} `json:"messages"`
				NextPageToken string `json:"nextPageToken"`
			}
			if err := g.getJSON(ctx, "messages", q, &page); err != nil {
				return nil, false, err
			}
			for _, m := range page.Messages {
				t, err := g.thread(ctx, threads, m.ThreadID)
				if gone(err) {
					continue
				}
				if err != nil {
					return nil, false, err
				}
				msg := t.find(m.ID)
				if msg == nil {
					continue
				}
				if msg.at().Before(cutoff) {
					done = true
					break pages
				}
				refs = append(refs, ref{thread: t.ID, message: m.ID})
			}
			if page.NextPageToken == "" {
				done = true
				break
			}
			token = page.NextPageToken
		}
		cut = cut || !done
	}
	return refs, cut, nil
}

func (g *Gmail) url(path string, q url.Values) string {
	raw := g.base + "/gmail/v1/users/me/" + path
	if len(q) > 0 {
		raw += "?" + q.Encode()
	}
	return raw
}

func (g *Gmail) getJSON(ctx context.Context, path string, q url.Values, dest any) error {
	// The quota paces every attempt before its token is read (NewGmail, call). A rate limit from Google means something else
	// spent this person's minute too: wait out the Retry-After, or a whole minute when it names none, and try once
	// more. A second rate limit is returned; an open-state scan turns it into a resume cursor instead of failing.
	var body []byte
	var err error
	for attempt := 0; ; attempt++ {
		body, err = g.get(ctx, g.url(path, q))
		limit := source.LimitOf(err)
		if limit == nil {
			break
		}
		limit.Units = g.quota.recent()
		g.limit = limit
		err = &source.RateLimitError{RateLimit: *limit}
		wait := limit.RetryAfter
		if wait < 0 {
			wait = gmailLimitWait
		}
		if attempt > 0 || wait > gmailMaxLimitWait {
			break
		}
		g.quota.hold(wait)
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return errors.New("gmail: a response could not be read")
	}
	return nil
}

func (g *Gmail) labels(ctx context.Context) (map[string]string, error) {
	var resp struct {
		Labels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := g.getJSON(ctx, "labels", nil, &resp); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, l := range resp.Labels {
		if l.ID != "" && l.Name != "" {
			out[l.ID] = l.Name
		}
	}
	return out, nil
}

// history reads every change since start: messages added, and the STARRED and SENT labels added.
func (g *Gmail) history(ctx context.Context, start string) ([]ref, string, error) {
	var refs []ref
	token := ""
	latest := ""
	for pages := 0; pages < 50; pages++ {
		q := url.Values{"startHistoryId": {start}, "historyTypes": {"messageAdded", "labelAdded"}, "maxResults": {"500"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			History []struct {
				MessagesAdded []struct {
					Message gmailMessage `json:"message"`
				} `json:"messagesAdded"`
				LabelsAdded []struct {
					Message  gmailMessage `json:"message"`
					LabelIDs []string     `json:"labelIds"`
				} `json:"labelsAdded"`
			} `json:"history"`
			NextPageToken string `json:"nextPageToken"`
			HistoryID     string `json:"historyId"`
		}
		if err := g.getJSON(ctx, "history", q, &page); err != nil {
			return nil, "", err
		}
		for _, h := range page.History {
			for _, a := range h.MessagesAdded {
				if a.Message.ID != "" && a.Message.ThreadID != "" && !a.Message.skipped() {
					refs = append(refs, ref{thread: a.Message.ThreadID, message: a.Message.ID})
				}
			}
			for _, a := range h.LabelsAdded {
				if a.Message.ID == "" || a.Message.ThreadID == "" {
					continue
				}
				for _, l := range a.LabelIDs {
					switch l {
					case "STARRED":
						refs = append(refs, ref{thread: a.Message.ThreadID, message: a.Message.ID, starred: true})
					case "SENT":
						// A draft that is sent may keep its id and gain SENT rather than arrive as a new message.
						refs = append(refs, ref{thread: a.Message.ThreadID, message: a.Message.ID})
					}
				}
			}
		}
		if page.HistoryID != "" {
			latest = page.HistoryID
		}
		if page.NextPageToken == "" {
			return refs, latest, nil
		}
		token = page.NextPageToken
	}
	return nil, "", errors.New("gmail: history pagination did not end")
}

// open reads open state: the latest message of each inbox thread from the last 14 days, and starred messages from
// the last 14 days. Both lists are newest first, so paging stops at the first older entry. cut reports that a list
// had more than gapPages pages inside the window and the rest was not read; the pass still succeeds and says so.
// start, when set, continues a scan that stopped at a rate limit. A rate limit with progress returns that point
// instead of an error, so the next pass resumes there.
// readBodies is true when a rule lists body and the grant can read one. gmail.metadata cannot, even beside
// another Gmail scope.
func readBodies(g *Gmail, scopes map[string]bool) bool {
	return g.bodies && scopes[ScopeGmailReadonly] && !scopes[ScopeGmailMetadata]
}

func (g *Gmail) open(ctx context.Context, threads map[string]*gmailThread, start *gmailScan, historyID string, bodies bool) ([]ref, bool, *gmailScan, error) {
	cutoff := g.now().Add(-gmailLookback)
	phase := "inbox"
	page := ""
	skip := map[string]bool{}
	if start != nil {
		if t, err := parseTime(start.Cutoff); err == nil {
			cutoff = t
		}
		if start.Phase != "" {
			phase = start.Phase
		}
		page = start.Page
		for _, id := range start.Skip {
			skip[id] = true
		}
		if start.HistoryID != "" {
			historyID = start.HistoryID
		}
	}
	cutoffStr := cutoff.UTC().Format(time.RFC3339)
	inboxCut := false
	startPages := 0
	if start != nil {
		inboxCut = start.InboxCut
		if start.Phase == phase {
			startPages = start.Pages
		}
	}
	var refs []ref
	if phase == "inbox" {
		var stopped *gmailScan
		var err error
		refs, inboxCut, stopped, err = g.scanInbox(ctx, threads, cutoff, cutoffStr, page, skip, historyID, startPages, bodies)
		if err != nil || stopped != nil {
			return refs, false, stopped, err
		}
		page = ""
		skip = map[string]bool{}
		startPages = 0
	}
	starRefs, starCut, stopped, err := g.scanStarred(ctx, threads, cutoff, cutoffStr, page, skip, historyID, startPages, inboxCut)
	if err != nil || stopped != nil {
		return append(refs, starRefs...), false, stopped, err
	}
	return append(refs, starRefs...), inboxCut || starCut, nil, nil
}

// stopScan turns a rate limit into a resume point when the scan has read something. With no progress it is the
// error, so the next pass starts the scan again. pages is how many list pages of this phase were already finished.
func stopScan(phase, page, cutoff, hist string, pages int, inboxCut bool, skip []string, refs []ref, err error) ([]ref, bool, *gmailScan, error) {
	if !errors.Is(err, source.ErrRateLimited) {
		return nil, false, nil, err
	}
	if phase == "inbox" && page == "" && pages == 0 && len(skip) == 0 && len(refs) == 0 && !inboxCut {
		return nil, false, nil, err
	}
	return refs, false, &gmailScan{
		HistoryID: hist, Cutoff: cutoff, Phase: phase, Page: page, Skip: skip, Pages: pages, InboxCut: inboxCut,
	}, nil
}

func (g *Gmail) scanInbox(ctx context.Context, threads map[string]*gmailThread, cutoff time.Time, cutoffStr, token string, skip map[string]bool, hist string, startPages int, bodies bool) ([]ref, bool, *gmailScan, error) {
	var refs []ref
	cut := true
	pages := startPages
	if pages < 0 {
		pages = 0
	}
	for ; pages < gapPages; pages++ {
		q := url.Values{"labelIds": {"INBOX"}, "maxResults": {"100"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Threads []struct {
				ID string `json:"id"`
			} `json:"threads"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := g.getJSON(ctx, "threads", q, &page); err != nil {
			return stopScan("inbox", token, cutoffStr, hist, pages, false, skipIDs(skip), refs, err)
		}
		fetched := skipIDs(skip)
		for _, th := range page.Threads {
			if skip[th.ID] {
				continue
			}
			t, err := g.thread(ctx, threads, th.ID)
			if errors.Is(err, source.ErrRateLimited) {
				return stopScan("inbox", token, cutoffStr, hist, pages, false, fetched, refs, err)
			}
			if gone(err) {
				fetched = append(fetched, th.ID)
				continue
			}
			if err != nil {
				return nil, false, nil, err
			}
			// The list is ordered by a thread's newest message of any kind, drafts included. Paging stops only when
			// that is older than the window; a thread whose newest real message is older is skipped.
			if t.newest().Before(cutoff) {
				return refs, false, nil, nil
			}
			live := t.live()
			if len(live) == 0 || live[len(live)-1].at().Before(cutoff) {
				fetched = append(fetched, th.ID)
				continue
			}
			last := live[len(live)-1]
			// Read the body before the thread is marked done. A rate limit here leaves it out of the resume
			// cursor, so the next pass collects it again with the body. A send cannot record it without one.
			var bodyText string
			if bodies {
				bodyText, err = g.body(ctx, last.ID)
				if errors.Is(err, source.ErrRateLimited) {
					return stopScan("inbox", token, cutoffStr, hist, pages, false, fetched, refs, err)
				}
				if err != nil && !unavailable(err) {
					return nil, false, nil, err
				}
			}
			fetched = append(fetched, th.ID)
			refs = append(refs, ref{thread: t.ID, message: last.ID, body: bodyText})
		}
		if page.NextPageToken == "" {
			return refs, false, nil, nil
		}
		token = page.NextPageToken
		skip = map[string]bool{}
	}
	return refs, cut, nil, nil
}

func (g *Gmail) scanStarred(ctx context.Context, threads map[string]*gmailThread, cutoff time.Time, cutoffStr, token string, skip map[string]bool, hist string, startPages int, inboxCut bool) ([]ref, bool, *gmailScan, error) {
	var refs []ref
	cut := true
	pages := startPages
	if pages < 0 {
		pages = 0
	}
	for ; pages < gapPages; pages++ {
		q := url.Values{"labelIds": {"STARRED"}, "maxResults": {"100"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Messages []struct {
				ID       string `json:"id"`
				ThreadID string `json:"threadId"`
			} `json:"messages"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := g.getJSON(ctx, "messages", q, &page); err != nil {
			return stopScan("starred", token, cutoffStr, hist, pages, inboxCut, skipIDs(skip), refs, err)
		}
		fetched := skipIDs(skip)
		for _, m := range page.Messages {
			if skip[m.ID] {
				continue
			}
			t, err := g.thread(ctx, threads, m.ThreadID)
			if errors.Is(err, source.ErrRateLimited) {
				return stopScan("starred", token, cutoffStr, hist, pages, inboxCut, fetched, refs, err)
			}
			if gone(err) {
				fetched = append(fetched, m.ID)
				continue
			}
			if err != nil {
				return nil, false, nil, err
			}
			fetched = append(fetched, m.ID)
			msg := t.find(m.ID)
			if msg == nil {
				continue
			}
			if msg.at().Before(cutoff) {
				return refs, false, nil, nil
			}
			refs = append(refs, ref{thread: t.ID, message: m.ID, starred: true})
		}
		if page.NextPageToken == "" {
			return refs, false, nil, nil
		}
		token = page.NextPageToken
		skip = map[string]bool{}
	}
	return refs, cut, nil, nil
}

func skipIDs(skip map[string]bool) []string {
	if len(skip) == 0 {
		return nil
	}
	out := make([]string, 0, len(skip))
	for id := range skip {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// newest is the time of the thread's newest message of any kind.
func (t *gmailThread) newest() time.Time {
	var n time.Time
	for i := range t.Messages {
		if at := t.Messages[i].at(); at.After(n) {
			n = at
		}
	}
	return n
}

// gone is a thread or message deleted since the history named it. It has nothing left to report, so the pass goes
// on. Any other failure, a 403 included, fails the pass: the history cursor has no lookback, so a skipped message
// would never be read again.
func gone(err error) bool { return isStatus(err, http.StatusNotFound) }

func (t *gmailThread) find(id string) *gmailMessage {
	for i := range t.Messages {
		if t.Messages[i].ID == id {
			return &t.Messages[i]
		}
	}
	return nil
}

// thread reads a thread's headers once per pass. Only From, To and Subject are asked for.
func (g *Gmail) thread(ctx context.Context, cache map[string]*gmailThread, id string) (*gmailThread, error) {
	if t, ok := cache[id]; ok {
		return t, nil
	}
	if !idPattern.MatchString(id) {
		return nil, &statusError{http.StatusNotFound}
	}
	q := url.Values{"format": {"metadata"}, "metadataHeaders": {"From", "To", "Subject"}}
	var t gmailThread
	if err := g.getJSON(ctx, "threads/"+id, q, &t); err != nil {
		return nil, err
	}
	if t.ID == "" {
		t.ID = id
	}
	cache[id] = &t
	return &t, nil
}

func (g *Gmail) item(t *gmailThread, r ref, me string, labels map[string]string) (source.Item, bool) {
	m := t.find(r.message)
	if m == nil || m.skipped() {
		return source.Item{}, false
	}
	live := t.live()
	pos := 0
	for i, lm := range live {
		if lm.ID == m.ID {
			pos = i
		}
	}
	from := firstAddress(m.header("From"))
	// Only the SENT label says the person sent it. A From header is the sender's claim, and anyone can write the
	// person's address there; it must not make an own act that closes their asks.
	own := m.has("SENT")
	typ := TypeReceived
	if own {
		typ = TypeSent
	}
	if r.starred {
		if !m.has("STARRED") {
			return source.Item{}, false
		}
		typ = TypeStarred
	}
	to := addresses(m.header("To"))
	addressed := false
	for _, a := range to {
		addressed = addressed || a == me
	}
	at := m.at()
	if at.IsZero() {
		return source.Item{}, false
	}
	fields := map[string]string{
		"from":    from,
		"to":      strings.Join(to, ", "),
		"subject": oneLine(live[0].header("Subject")),
		"date":    at.Format(time.RFC3339),
		"labels":  labelNames(m.LabelIDs, labels),
		"link":    "https://mail.google.com/mail/u/0/#all/" + t.ID,
	}
	state := g.threadState(live, labels)
	keyID := t.ID + ":" + m.ID
	if typ == TypeStarred {
		keyID = t.ID + ":starred:" + m.ID
	}
	key, version := source.FitKey(g.name+":"+keyID, m.ID)
	obs := g.observed()
	it := source.Item{
		Source:     g.name,
		Subject:    "gmail:" + g.person + ":thread:" + t.ID,
		Key:        key,
		Type:       typ,
		EventTitle: gmailTitle(typ, pos > 0),
		OccurredAt: at,
		Fields:     fields,
		State:      state,
		ObservedAt: &obs,
		Version:    version,
	}
	switch typ {
	case TypeSent:
		it.Own = true
		it.Actor = "email:" + me
	case TypeReceived:
		it.Addressed = addressed
		if from != "" {
			it.Actor = "email:" + from
		}
	}
	return it, true
}

func (g *Gmail) threadState(live []*gmailMessage, labels map[string]string) map[string]string {
	unread, starred := false, false
	set := map[string]bool{}
	var ids []string
	for _, m := range live {
		unread = unread || m.has("UNREAD")
		starred = starred || m.has("STARRED")
		for _, l := range m.LabelIDs {
			if !set[l] {
				set[l] = true
				ids = append(ids, l)
			}
		}
	}
	st := map[string]string{
		"unread":  strconv.FormatBool(unread),
		"starred": strconv.FormatBool(starred),
		"labels":  labelNames(ids, labels),
	}
	if len(live) > 0 {
		if f := firstAddress(live[len(live)-1].header("From")); f != "" {
			st["last_from"] = f
		}
	}
	return st
}

// gmailTitle describes the change. It never uses the subject, a name or the body.
func gmailTitle(typ string, reply bool) string {
	switch typ {
	case TypeReceived:
		if reply {
			return "Reply received"
		}
		return "New message"
	case TypeSent:
		if reply {
			return "Replied"
		}
		return "Sent"
	case TypeStarred:
		return "Starred"
	}
	return ""
}

func labelNames(ids []string, names map[string]string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		n := names[id]
		if n == "" {
			n = id
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// addresses returns the lowercased addresses in a header. A header that does not parse yields the parts that look
// like addresses.
func addresses(h string) []string {
	if strings.TrimSpace(h) == "" {
		return nil
	}
	list, err := mail.ParseAddressList(h)
	if err == nil {
		out := make([]string, 0, len(list))
		for _, a := range list {
			out = append(out, strings.ToLower(a.Address))
		}
		return out
	}
	var out []string
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if i, j := strings.LastIndex(part, "<"), strings.LastIndex(part, ">"); i >= 0 && j > i {
			part = part[i+1 : j]
		}
		if strings.Contains(part, "@") && !strings.ContainsAny(part, " \t") {
			out = append(out, strings.ToLower(part))
		}
	}
	return out
}

func firstAddress(h string) string {
	if a := addresses(h); len(a) > 0 {
		return a[0]
	}
	return ""
}

// body reads a message's text/plain part. It needs gmail.readonly, and is called only when a rule lists body.
func (g *Gmail) body(ctx context.Context, id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", nil
	}
	var m struct {
		Payload gmailPart `json:"payload"`
	}
	if err := g.getJSON(ctx, "messages/"+id, url.Values{"format": {"full"}}, &m); err != nil {
		return "", err
	}
	return m.Payload.text(), nil
}

type gmailPart struct {
	MimeType string `json:"mimeType"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

func (p gmailPart) text() string {
	if strings.HasPrefix(strings.ToLower(p.MimeType), "text/plain") && p.Body.Data != "" {
		data := strings.TrimRight(p.Body.Data, "=")
		b, err := base64.RawURLEncoding.DecodeString(data)
		if err != nil {
			return ""
		}
		return string(b)
	}
	for _, sub := range p.Parts {
		if t := sub.text(); t != "" {
			return t
		}
	}
	return ""
}
