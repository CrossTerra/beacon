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
type Gmail struct{ common }

// NewGmail builds the Gmail source. It reads no credential.
func NewGmail(o Options) (*Gmail, error) {
	c, err := newCommon(o, "gmail", GmailBase, gmailPath)
	if err != nil {
		return nil, err
	}
	return &Gmail{c}, nil
}

func gmailPath(p string) bool {
	const prefix = "/gmail/v1/users/me/"
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	rest := strings.TrimPrefix(p, prefix)
	switch rest {
	case "profile", "labels", "history", "threads", "messages":
		return true
	}
	kind, id, ok := strings.Cut(rest, "/")
	return ok && (kind == "threads" || kind == "messages") && idPattern.MatchString(id)
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
type ref struct {
	thread  string
	message string
	starred bool
}

// Collect implements source.Source. The cursor is a Gmail history id. A nil cursor reads inbox threads and starred
// mail from the last 14 days (DEC-23).
func (g *Gmail) Collect(ctx context.Context, cursor []byte) (source.Collected, error) {
	scopes, err := g.grant(ctx, []string{ScopeGmailMetadata, ScopeGmailReadonly})
	if err != nil {
		return source.Collected{}, err
	}
	g.clock.Reset()
	if len(cursor) > 0 {
		if _, err := strconv.ParseUint(string(cursor), 10, 64); err != nil {
			return source.Collected{}, errors.New("gmail: the cursor is not a history id")
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
	next := profile.HistoryID
	if len(cursor) == 0 {
		refs, err = g.open(ctx, threads)
	} else {
		var got string
		refs, got, err = g.history(ctx, string(cursor))
		if isStatus(err, http.StatusNotFound) {
			// The history id expired. Read open state again; keys make the overlap harmless.
			refs, err = g.open(ctx, threads)
		} else if got != "" {
			next = got
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
		if bodies && it.Type != TypeStarred {
			body, err := g.body(ctx, r.message)
			if err != nil && !gone(err) {
				return source.Collected{Items: items}, err
			}
			if body != "" {
				it.Fields["body"] = body
			}
		}
		items = append(items, it)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	return source.Collected{Items: items, Next: []byte(next)}, nil
}

func (g *Gmail) url(path string, q url.Values) string {
	raw := g.base + "/gmail/v1/users/me/" + path
	if len(q) > 0 {
		raw += "?" + q.Encode()
	}
	return raw
}

func (g *Gmail) getJSON(ctx context.Context, path string, q url.Values, dest any) error {
	body, err := g.get(ctx, g.url(path, q))
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
// the last 14 days. Both lists are newest first, so paging stops at the first older entry.
func (g *Gmail) open(ctx context.Context, threads map[string]*gmailThread) ([]ref, error) {
	cutoff := g.now().Add(-gmailLookback)
	var refs []ref
	token := ""
inbox:
	for pages := 0; pages < 20; pages++ {
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
			return nil, err
		}
		for _, th := range page.Threads {
			t, err := g.thread(ctx, threads, th.ID)
			if gone(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			// The list is ordered by a thread's newest message of any kind, drafts included. Paging stops only when
			// that is older than the window; a thread whose newest real message is older is skipped.
			if t.newest().Before(cutoff) {
				break inbox
			}
			live := t.live()
			if len(live) == 0 {
				continue
			}
			last := live[len(live)-1]
			if last.at().Before(cutoff) {
				continue
			}
			refs = append(refs, ref{thread: t.ID, message: last.ID})
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	token = ""
	for pages := 0; pages < 20; pages++ {
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
			return nil, err
		}
		for _, m := range page.Messages {
			t, err := g.thread(ctx, threads, m.ThreadID)
			if gone(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			msg := t.find(m.ID)
			if msg == nil {
				continue
			}
			if msg.at().Before(cutoff) {
				return refs, nil
			}
			refs = append(refs, ref{thread: t.ID, message: m.ID, starred: true})
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	return refs, nil
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
	own := m.has("SENT") || (from != "" && from == me)
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
		"link":    "https://mail.google.com/mail/u/" + url.PathEscape(me) + "/#all/" + t.ID,
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
