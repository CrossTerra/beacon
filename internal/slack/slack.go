// Package slack is the Slack source: direct messages, mentions, replies in threads the person posted in, the person's
// own messages, and review requests in the channels the config names. It reads with the person's user token, which is
// read from the keychain at the start of each pass and set only as an Authorization header.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/source"
)

// Fields is Slack's fixed field list (DEC-6). body is set only when a rule lists it.
var Fields = []string{"channel", "ts", "thread_ts", "author", "permalink", "body"}

// StateFields is empty: Slack has no object state Beacon reads.
var StateFields = []string{}

// Slack item types (DEC-18).
const (
	TypeOwn           = "own_message"
	TypeMentioned     = "mentioned"
	TypeDM            = "dm_received"
	TypeThreadReply   = "thread_reply"
	TypeReviewRequest = "review_request"
)

// PersonalStem is the stem of direct-message subjects (DEC-10). Channel subjects are not personal.
const PersonalStem = "slack:dm:"

const (
	// firstWindow is how far back a first run reads (DEC-23), and followFor how long a thread is followed after the
	// person's latest post in it.
	firstWindow = 7 * 24 * time.Hour
	followFor   = 7 * 24 * time.Hour
	// threadsPerPass bounds conversations.replies calls in one pass. Threads not read wait for a later pass, oldest
	// read first, each from its own watermark.
	threadsPerPass = 40
	// A thread with a message in the last activeFor is read every pass; a quieter one every quietEvery.
	activeFor    = time.Hour
	quietEvery   = 15 * time.Minute
	searchPages  = 20
	historyPages = 10
	repliesPages = 10
	// searchBackoff widens search's date-only after: so a message near midnight in any time zone is not missed. Only
	// messages inside the window are kept.
	searchBackoff = 48 * time.Hour
)

var personPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// Options configures a Slack source.
type Options struct {
	Name string
	// Person is the person's Signal handle without "@". Direct-message subjects carry it (DEC-10).
	Person string
	// Workspace and User are the token's workspace and user ids; auth.test must report the same.
	Workspace string
	User      string
	// Channels are channel ids whose top-level messages are read for review requests.
	Channels []string
	// Identity is person.identities for this source, slack:user:<id>. Empty skips that check.
	Identity string
	Token    func() (keychain.Secret, error)
	// Base replaces https://slack.com (tests).
	Base  string
	HTTP  *http.Client
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// Source reads Slack.
type Source struct {
	name      string
	person    string
	workspace string
	user      string
	identity  string
	channels  []string
	token     func() (keychain.Secret, error)
	api       *api
	now       func() time.Time
	bodies    bool
}

// New builds a Slack source. It reads no credential.
func New(o Options) (*Source, error) {
	name := o.Name
	if name == "" {
		name = "slack"
	}
	if !source.ValidName(name) {
		return nil, fmt.Errorf("slack: name %q is invalid", name)
	}
	if !personPattern.MatchString(o.Person) || strings.HasPrefix(o.Person, "act_") {
		return nil, errors.New("slack: person.handle must be an @handle; direct-message subjects carry it")
	}
	if !idPattern.MatchString(o.Workspace) || !idPattern.MatchString(o.User) {
		return nil, errors.New("slack: workspace and user are required")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	a, err := newAPI(o.Base, o, now)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	var channels []string
	for _, c := range o.Channels {
		if !strings.HasPrefix(c, "C") || !channelPattern.MatchString(c) {
			return nil, fmt.Errorf("slack: channel %q is not a channel id", c)
		}
		if !allowed[c] {
			allowed[c] = true
			channels = append(channels, c)
		}
	}
	sort.Strings(channels)
	a.allowHistory = func(c string) bool { return allowed[c] }
	a.user = o.User
	return &Source{
		name: name, person: o.Person, workspace: o.Workspace, user: o.User, identity: o.Identity,
		channels: channels, token: o.Token, api: a, now: now,
	}, nil
}

// Name implements source.Source.
func (s *Source) Name() string { return s.name }

// Fields implements source.Source.
func (s *Source) Fields() []string { return append([]string(nil), Fields...) }

// StateFields implements source.Source.
func (s *Source) StateFields() []string { return append([]string(nil), StateFields...) }

// Hosts implements source.Source: slack.com only.
func (s *Source) Hosts() []string { return []string{s.api.hostPort} }

// SetHTTP sets the client used to collect. The app sets it after the egress list, which is built from Hosts, exists.
func (s *Source) SetHTTP(c *http.Client) { s.api.http.HTTP = c }

// SetBodies says whether a rule lists body for this source. Message text leaves only then.
func (s *Source) SetBodies(b bool) { s.bodies = b }

// PersonalStem is slack:dm:. A send pass holds direct-message items until Signal restricts slack:dm:*.
func (s *Source) PersonalStem() string { return PersonalStem }

// cursorData is a pass's position: when it ran, the threads the person posted in, and each configured channel's
// watermark. It holds ids and timestamps, no text.
type cursorData struct {
	// Since is the first server Date of the pass that stored it. Search reads from 15 minutes before it.
	Since string `json:"since"`
	// Threads are keyed by "<channel>/<thread ts>".
	Threads map[string]*thread `json:"threads,omitempty"`
	// Channels maps a configured channel to the newest message ts read there.
	Channels map[string]string `json:"channels,omitempty"`
}

// thread is a thread the person posted in.
type thread struct {
	// DM is true when the thread is in a group direct message: its subjects are personal.
	DM bool `json:"dm,omitempty"`
	// Joined and Last are the ts of the person's first and latest post seen there.
	Joined string `json:"joined"`
	Last   string `json:"last"`
	// Seen is the newest message ts read in the thread.
	Seen string `json:"seen,omitempty"`
	// Polled is when the thread was last read.
	Polled string `json:"polled,omitempty"`
}

func parseCursor(b []byte) (cursorData, error) {
	var c cursorData
	if err := json.Unmarshal(b, &c); err != nil {
		return cursorData{}, errors.New("slack: the cursor could not be read")
	}
	if _, err := time.Parse(time.RFC3339, c.Since); err != nil {
		return cursorData{}, errors.New("slack: the cursor time could not be read")
	}
	for k, t := range c.Threads {
		ch, root, ok := strings.Cut(k, "/")
		if !ok || t == nil || !channelPattern.MatchString(ch) || !tsPattern.MatchString(root) || !tsPattern.MatchString(t.Joined) || !tsPattern.MatchString(t.Last) || (t.Seen != "" && !tsPattern.MatchString(t.Seen)) {
			return cursorData{}, errors.New("slack: the cursor holds a thread it cannot read")
		}
	}
	for ch, ts := range c.Channels {
		if !channelPattern.MatchString(ch) || !tsPattern.MatchString(ts) {
			return cursorData{}, errors.New("slack: the cursor holds a channel it cannot read")
		}
	}
	return c, nil
}

// message is one Slack message, merged from every read that returned it.
type message struct {
	channel   string
	ts        string
	threadTS  string
	user      string
	botID     string
	subtype   string
	text      string
	permalink string
	// im and mpim are what search said about the conversation. A D… channel is a direct message whatever it said.
	im   bool
	mpim bool
	// followed is set when the message was read from a thread the person posted in; joined is their first post there.
	followed bool
	joined   string
	// history is set when the message was read from a configured channel's top level.
	history bool
}

func (m *message) key() string { return m.channel + "/" + m.ts }

// root is the thread's root ts: thread_ts, or the message's own ts when it is not in a thread.
func (m *message) root() string {
	if m.threadTS != "" {
		return m.threadTS
	}
	return m.ts
}

func (m *message) inThread() bool { return m.threadTS != "" && m.threadTS != m.ts }

func (m *message) direct() bool { return m.im || strings.HasPrefix(m.channel, "D") }

// personal is a direct or group direct message. Only a C… channel search did not mark as one is a channel; anything
// else is treated as personal, which can only hold more back.
func (m *message) personal() bool {
	return m.direct() || m.mpim || !strings.HasPrefix(m.channel, "C")
}

func (s *Source) own(m *message) bool { return m.user == s.user && m.botID == "" }

type pool map[string]*message

// add merges m into the pool. Flags accumulate; a field already known is kept.
func (p pool) add(m *message) {
	if !channelPattern.MatchString(m.channel) || !tsPattern.MatchString(m.ts) || (m.threadTS != "" && !tsPattern.MatchString(m.threadTS)) {
		return
	}
	old, ok := p[m.key()]
	if !ok {
		p[m.key()] = m
		return
	}
	old.im = old.im || m.im
	old.mpim = old.mpim || m.mpim
	old.followed = old.followed || m.followed
	old.history = old.history || m.history
	for _, f := range []struct {
		dst *string
		v   string
	}{
		{&old.threadTS, m.threadTS}, {&old.user, m.user}, {&old.botID, m.botID}, {&old.text, m.text},
		{&old.permalink, m.permalink}, {&old.joined, m.joined},
	} {
		if *f.dst == "" {
			*f.dst = f.v
		}
	}
}

// Collect implements source.Source. A nil cursor reads the last 7 days (DEC-23).
func (s *Source) Collect(ctx context.Context, raw []byte) (source.Collected, error) {
	if s.token == nil || s.api.http.HTTP == nil {
		return source.Collected{}, errors.New("slack: the source has no token or client")
	}
	tok, err := s.token()
	if err != nil {
		return source.Collected{}, err
	}
	s.api.clock.Reset()
	first := len(raw) == 0
	cur := cursorData{}
	if !first {
		if cur, err = parseCursor(raw); err != nil {
			return source.Collected{}, err
		}
	}
	if cur.Threads == nil {
		cur.Threads = map[string]*thread{}
	}
	id, err := s.api.identity(ctx, tok)
	if err != nil {
		return source.Collected{}, err
	}
	if len(id.Missing) > 0 {
		return source.Collected{}, source.ErrUnauthorized
	}
	if id.Workspace != s.workspace || id.User != s.user || (s.identity != "" && s.identity != "slack:user:"+id.User) {
		return source.Collected{}, source.ErrIdentity
	}
	now := s.now().UTC()
	var since, floor time.Time
	if first {
		floor = now.Add(-firstWindow)
	} else {
		since, _ = time.Parse(time.RFC3339, cur.Since)
		floor = since.Add(-source.Lookback)
	}
	floorTS := formatTS(floor)
	msgs := pool{}
	cut := false
	after := floor.Add(-searchBackoff).UTC().Format("2006-01-02")
	me := "<@" + s.user + ">"
	for _, q := range []string{"is:dm", me, "from:" + me} {
		c, err := s.search(ctx, tok, q+" after:"+after, floor, msgs)
		if err != nil {
			return source.Collected{}, err
		}
		cut = cut || c
	}
	// A thread the person posted in is followed for 7 days after their latest post there. One-to-one direct messages
	// are not: search already reads every message in them.
	for _, m := range msgs {
		s.follow(cur.Threads, m)
	}
	for k, t := range cur.Threads {
		if tsTime(t.Last).Before(now.Add(-followFor)) {
			delete(cur.Threads, k)
		}
	}
	observed := s.observed()
	for _, k := range pollOrder(cur.Threads, now) {
		t := cur.Threads[k]
		channel, root, _ := strings.Cut(k, "/")
		oldest := t.Seen
		if oldest == "" {
			oldest = floorTS
		}
		got, err := s.replies(ctx, tok, channel, root, oldest)
		if errors.Is(err, errGone) {
			delete(cur.Threads, k)
			continue
		}
		if err != nil {
			return source.Collected{}, err
		}
		for _, m := range got {
			if !tsLess(oldest, m.ts) {
				continue
			}
			m.followed, m.joined, m.mpim = true, t.Joined, t.DM
			if tsLess(t.Seen, m.ts) {
				t.Seen = m.ts
			}
			msgs.add(m)
			s.follow(cur.Threads, m)
		}
		t.Polled = observed.Format(time.RFC3339)
	}
	channels := map[string]string{}
	for _, c := range s.channels {
		oldest := cur.Channels[c]
		if oldest == "" {
			oldest = floorTS
		}
		got, c2, err := s.history(ctx, tok, c, oldest)
		if errors.Is(err, errGone) {
			// Unreadable this pass (archived, or the person left). Keep its watermark, so nothing is skipped if it
			// comes back.
			if w, ok := cur.Channels[c]; ok {
				channels[c] = w
			}
			continue
		}
		if err != nil {
			return source.Collected{}, err
		}
		cut = cut || c2
		newest := oldest
		for _, m := range got {
			if !tsLess(oldest, m.ts) {
				continue
			}
			m.history = true
			if tsLess(newest, m.ts) {
				newest = m.ts
			}
			msgs.add(m)
			s.follow(cur.Threads, m)
		}
		channels[c] = newest
	}
	cur.Channels = channels
	items, dropped := s.items(msgs, id.URL)
	var times []time.Time
	for _, it := range items {
		times = append(times, it.OccurredAt)
	}
	next, err := s.api.clock.Next(since, s.now(), times)
	if err != nil {
		return source.Collected{}, err
	}
	cur.Since = string(next)
	data, err := json.Marshal(cur)
	if err != nil {
		return source.Collected{}, err
	}
	return source.Collected{Items: items, Next: data, Cut: cut, Dropped: dropped}, nil
}

func (s *Source) observed() time.Time {
	if t := s.api.clock.First(); !t.IsZero() {
		return t
	}
	return s.now().UTC()
}

// follow records a thread the person posted in, outside one-to-one direct messages.
func (s *Source) follow(threads map[string]*thread, m *message) {
	if !s.own(m) || m.direct() {
		return
	}
	k := m.channel + "/" + m.root()
	t := threads[k]
	if t == nil {
		t = &thread{Joined: m.ts, Last: m.ts}
		threads[k] = t
	}
	t.DM = t.DM || m.personal()
	if tsLess(m.ts, t.Joined) {
		t.Joined = m.ts
	}
	if tsLess(t.Last, m.ts) {
		t.Last = m.ts
	}
}

// pollOrder is the threads this pass reads: those that are due, read longest ago first, at most threadsPerPass. A
// thread with a message in the last hour is due every pass; a quieter one every 15 minutes. Each is read from its own
// watermark, so one that waits loses nothing.
func pollOrder(threads map[string]*thread, now time.Time) []string {
	keys := make([]string, 0, len(threads))
	for k, t := range threads {
		if t.Polled != "" {
			polled, err := time.Parse(time.RFC3339, t.Polled)
			latest := t.Last
			if tsLess(latest, t.Seen) {
				latest = t.Seen
			}
			quiet := tsTime(latest).Before(now.Add(-activeFor))
			if err == nil && quiet && now.Sub(polled) < quietEvery {
				continue
			}
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := threads[keys[i]].Polled, threads[keys[j]].Polled
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	if len(keys) > threadsPerPass {
		keys = keys[:threadsPerPass]
	}
	return keys
}

// apiMessage is a message as conversations.replies and conversations.history return it.
type apiMessage struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
}

func (a apiMessage) message(channel string) *message {
	return &message{channel: channel, ts: a.TS, threadTS: a.ThreadTS, user: a.User, botID: a.BotID, subtype: a.Subtype, text: a.Text}
}

// search reads one query, newest first, and adds the matches inside the window. cut reports that it stopped at its
// page limit with older matches in the window unread.
func (s *Source) search(ctx context.Context, tok keychain.Secret, query string, floor time.Time, msgs pool) (bool, error) {
	for page := 1; page <= searchPages; page++ {
		q := url.Values{"query": {query}, "count": {"100"}, "page": {strconv.Itoa(page)}, "sort": {"timestamp"}, "sort_dir": {"desc"}}
		var resp struct {
			Messages struct {
				Matches []struct {
					Channel struct {
						ID     string `json:"id"`
						IsIM   bool   `json:"is_im"`
						IsMPIM bool   `json:"is_mpim"`
					} `json:"channel"`
					User      string `json:"user"`
					BotID     string `json:"bot_id"`
					Subtype   string `json:"subtype"`
					TS        string `json:"ts"`
					ThreadTS  string `json:"thread_ts"`
					Text      string `json:"text"`
					Permalink string `json:"permalink"`
				} `json:"matches"`
				Paging struct {
					Pages int `json:"pages"`
				} `json:"paging"`
			} `json:"messages"`
		}
		if _, err := s.api.call(ctx, tok, methodSearch, q, &resp); err != nil {
			return false, err
		}
		older := false
		for _, r := range resp.Messages.Matches {
			if !tsPattern.MatchString(r.TS) {
				continue
			}
			if tsTime(r.TS).Before(floor) {
				older = true
				continue
			}
			threadTS := r.ThreadTS
			if threadTS == "" {
				threadTS = permalinkThread(r.Permalink)
			}
			msgs.add(&message{
				channel: r.Channel.ID, ts: r.TS, threadTS: threadTS, user: r.User, botID: r.BotID, subtype: r.Subtype,
				text: r.Text, permalink: r.Permalink, im: r.Channel.IsIM, mpim: r.Channel.IsMPIM,
			})
		}
		if older || page >= resp.Messages.Paging.Pages {
			return false, nil
		}
	}
	return true, nil
}

// permalinkThread is the thread_ts a search permalink names for a reply. Search does not always return thread_ts.
func permalinkThread(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if t := u.Query().Get("thread_ts"); tsPattern.MatchString(t) {
		return t
	}
	return ""
}

// replies reads a thread from oldest, oldest first. A long thread stops at the page limit; the watermark moves only
// as far as was read, so the next pass goes on from there.
func (s *Source) replies(ctx context.Context, tok keychain.Secret, channel, root, oldest string) ([]*message, error) {
	var out []*message
	next := ""
	for page := 0; page < repliesPages; page++ {
		q := url.Values{"channel": {channel}, "ts": {root}, "oldest": {oldest}, "limit": {"200"}}
		if next != "" {
			q.Set("cursor", next)
		}
		var resp struct {
			Messages []apiMessage `json:"messages"`
			envelope
		}
		if _, err := s.api.call(ctx, tok, methodReplies, q, &resp); err != nil {
			return nil, err
		}
		for _, m := range resp.Messages {
			out = append(out, m.message(channel))
		}
		next = resp.Metadata.NextCursor
		if next == "" {
			break
		}
	}
	return out, nil
}

// history reads a configured channel's top-level messages after oldest. Slack returns them newest first, so a channel
// over the page limit loses its oldest; cut says so.
func (s *Source) history(ctx context.Context, tok keychain.Secret, channel, oldest string) ([]*message, bool, error) {
	var out []*message
	next := ""
	for page := 0; page < historyPages; page++ {
		q := url.Values{"channel": {channel}, "oldest": {oldest}, "limit": {"200"}}
		if next != "" {
			q.Set("cursor", next)
		}
		var resp struct {
			Messages []apiMessage `json:"messages"`
			envelope
		}
		if _, err := s.api.call(ctx, tok, methodHistory, q, &resp); err != nil {
			return nil, false, err
		}
		for _, m := range resp.Messages {
			out = append(out, m.message(channel))
		}
		next = resp.Metadata.NextCursor
		if next == "" {
			return out, false, nil
		}
	}
	return out, true, nil
}

// formatTS writes a time as a Slack ts.
func formatTS(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// tsTime reads a Slack ts. A malformed one is the zero time.
func tsTime(ts string) time.Time {
	sec, micro, ok := splitTS(ts)
	if !ok {
		return time.Time{}
	}
	return time.Unix(sec, micro*1000).UTC()
}

func splitTS(ts string) (int64, int64, bool) {
	a, b, ok := strings.Cut(ts, ".")
	if !ok {
		return 0, 0, false
	}
	sec, err1 := strconv.ParseInt(a, 10, 64)
	micro, err2 := strconv.ParseInt(b, 10, 64)
	return sec, micro, err1 == nil && err2 == nil
}

// tsLess reports whether a is before b. An empty a is before everything.
func tsLess(a, b string) bool {
	if a == "" {
		return b != ""
	}
	as, am, _ := splitTS(a)
	bs, bm, _ := splitTS(b)
	return as < bs || (as == bs && am < bm)
}
