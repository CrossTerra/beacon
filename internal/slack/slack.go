// Package slack is the Slack source: direct and group direct messages, mentions, replies in threads the person posted
// in or was mentioned in, the person's own messages, review requests in the channels the config names, and the
// deletion of a message that addressed the person. It reads with the person's user token, which is read from the
// keychain at the start of each pass and set only as an Authorization header.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/source"
)

// Fields is Slack's fixed field list (DEC-6). body is set only when a rule lists it. title, channel_name and
// author_name are display names, read only when a rule lists one of them and the token holds NameScopes.
var Fields = []string{"channel", "channel_type", "ts", "thread_ts", "author", "permalink", "body",
	FieldTitle, FieldChannelName, FieldAuthorName}

// StateFields is empty: Slack has no object state Beacon reads.
var StateFields = []string{}

// Slack item types (DEC-18).
const (
	TypeOwn           = "own_message"
	TypeMentioned     = "mentioned"
	TypeDM            = "dm_received"
	TypeThreadReply   = "thread_reply"
	TypeReviewRequest = "review_request"
	TypeDeleted       = "message_deleted"
)

// Conversation kinds, the channel_type field. Every kind but kindChannel is personal.
const (
	kindIM      = "im"
	kindMPIM    = "mpim"
	kindPrivate = "private"
	kindChannel = "channel"
)

// PersonalKinds are the channel_type values whose items are under PersonalStem.
var PersonalKinds = []string{kindIM, kindMPIM, kindPrivate}

// PersonalStem is the stem of personal subjects (DEC-10): direct messages, group direct messages and private
// channels. Public channel subjects are not personal.
const PersonalStem = "slack:dm:"

const (
	// firstWindow is how far back a first run reads (DEC-23); mentions are always searched that far back. followFor is
	// how long a thread is followed after the person's latest post or mention there; an ask is checked for deletion as
	// long.
	firstWindow = 7 * 24 * time.Hour
	followFor   = 7 * 24 * time.Hour
	// Calls per pass that wait for a later pass when there are more, longest unread first, each from its own
	// watermark.
	threadsPerPass  = 40
	groupDMsPerPass = 20
	checksPerPass   = 20
	// A thread or group direct message with a message in the last activeFor is read every pass; a quieter one, and a
	// deletion check, every quietEvery.
	activeFor  = time.Hour
	quietEvery = 15 * time.Minute
	// groupsEvery is how often the person's user groups are read again.
	groupsEvery  = time.Hour
	searchPages  = 20
	historyPages = 10
	repliesPages = 10
	listPages    = 10
	// searchBackoff widens search's date-only after: so a message near midnight in any time zone is not missed. Only
	// messages inside the window are kept.
	searchBackoff = 48 * time.Hour
)

var personPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// Options configures a Slack source.
type Options struct {
	Name string
	// Person is the person's Signal handle without "@". Personal subjects carry it (DEC-10).
	Person string
	// Workspace and User are the token's workspace and user ids; auth.test must report the same.
	Workspace string
	User      string
	// Channels are channel ids whose top-level messages are read for review requests and @here/@channel.
	Channels []string
	// Identity is person.identities for this source, slack:user:<id>. Empty skips that check.
	Identity string
	Token    func() (keychain.Secret, error)
	// Cache is the name cache's path, in the state directory. Empty keeps names in memory only.
	Cache string
	// Base replaces https://slack.com (tests).
	Base  string
	HTTP  *http.Client
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
	// Location is the zone whose days group a direct message's top-level messages into one subject. Nil is the
	// machine's.
	Location *time.Location
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
	loc       *time.Location
	bodies    bool
	// groups are the user groups the person is in, read at groupsAt.
	groups   map[string]bool
	groupsAt time.Time
	// groupDMs are the group direct messages the person is in, as of this pass.
	groupDMs map[string]bool
	// names is set when a rule lists title, channel_name or author_name. persist is set when a pass may write the
	// name cache (a send pass). cache is read from cachePath on the first collect.
	names     bool
	persist   bool
	cachePath string
	cache     *nameCache
	titling   *titling
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
		return nil, errors.New("slack: person.handle must be an @handle; personal subjects carry it")
	}
	if !idPattern.MatchString(o.Workspace) || !idPattern.MatchString(o.User) {
		return nil, errors.New("slack: workspace and user are required")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	loc := o.Location
	if loc == nil {
		loc = time.Local
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
	s := &Source{
		name: name, person: o.Person, workspace: o.Workspace, user: o.User, identity: o.Identity,
		channels: channels, token: o.Token, api: a, now: now, loc: loc, groupDMs: map[string]bool{}, cachePath: o.Cache,
	}
	a.configured = func(c string) bool { return allowed[c] }
	a.groupDM = func(c string) bool { return s.groupDMs[c] }
	a.user = o.User
	return s, nil
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

// SetNames says whether a rule lists title, channel_name or author_name for this source. Names are read only then.
func (s *Source) SetNames(b bool) { s.names = b }

// SetPersist says whether a pass may write the name cache to the state directory. A dry run does not.
func (s *Source) SetPersist(b bool) { s.persist = b }

// PersonalStem is slack:dm:. A send pass holds personal items until Signal restricts slack:dm:*.
func (s *Source) PersonalStem() string { return PersonalStem }

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
	// kind is the conversation's kind, when a read said: im, mpim, private or channel.
	kind string
	// followed is set when the message was read from a followed thread; joined is when the person joined it.
	followed bool
	joined   string
	// history is set when the message was read from a configured channel's or group direct message's top level.
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

// conversation is the message's kind. A D… id is a direct message whatever a read said. A kind no read reported is
// private: unknown is treated as personal, which can only hold more back.
func (m *message) conversation() string {
	switch {
	case strings.HasPrefix(m.channel, "D"):
		return kindIM
	case m.kind != "":
		return m.kind
	}
	return kindPrivate
}

func (m *message) personal() bool { return m.conversation() != kindChannel }

func (s *Source) own(m *message) bool { return m.user == s.user && m.botID == "" }

var kindRank = map[string]int{"": 0, kindChannel: 1, kindPrivate: 2, kindMPIM: 3, kindIM: 4}

type pool map[string]*message

// add merges m into the pool. Flags accumulate, the more personal kind wins, and a field already known is kept.
func (p pool) add(m *message) {
	if !channelPattern.MatchString(m.channel) || !tsPattern.MatchString(m.ts) || (m.threadTS != "" && !tsPattern.MatchString(m.threadTS)) {
		return
	}
	old, ok := p[m.key()]
	if !ok {
		p[m.key()] = m
		return
	}
	if kindRank[m.kind] > kindRank[old.kind] {
		old.kind = m.kind
	}
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

// pass is one collect.
type pass struct {
	tok      keychain.Secret
	cur      cursorData
	msgs     pool
	now      time.Time
	floor    time.Time
	floorTS  string
	observed string
	cut      bool
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
	s.titling = nil
	if s.cache == nil {
		s.cache = loadCache(s.cachePath, s.now().UTC())
	}
	first := len(raw) == 0
	cur := cursorData{}
	if !first {
		if cur, err = parseCursor(raw); err != nil {
			return source.Collected{}, err
		}
	}
	cur.init()
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
	p := &pass{tok: tok, cur: cur, msgs: pool{}, now: s.now().UTC()}
	var since time.Time
	if first {
		p.floor = p.now.Add(-firstWindow)
	} else {
		since, _ = time.Parse(time.RFC3339, cur.Since)
		p.floor = since.Add(-source.Lookback)
	}
	p.floorTS = formatTS(p.floor)
	p.observed = s.observed().Format(time.RFC3339)
	if err := s.collect(ctx, p); err != nil {
		return source.Collected{}, err
	}
	items, dropped := s.items(p.msgs, id.URL)
	warning := s.nameItems(ctx, p, id, items)
	if ctx.Err() != nil {
		return source.Collected{}, ctx.Err()
	}
	s.flush()
	s.track(p, items)
	s.closes(p, items)
	deleted, err := s.checkDeleted(ctx, p)
	if err != nil {
		return source.Collected{}, err
	}
	items = append(items, deleted...)
	var times []time.Time
	for _, it := range items {
		if it.Type != TypeDeleted {
			times = append(times, it.OccurredAt)
		}
	}
	next, err := s.api.clock.Next(since, s.now(), times)
	if err != nil {
		return source.Collected{}, err
	}
	p.cur.Since = string(next)
	data, err := json.Marshal(p.cur)
	if err != nil {
		return source.Collected{}, err
	}
	// A retitle after this pass reads with the same token, and only when names may be read.
	s.titling = &titling{tok: tok, names: s.names && len(id.MissingNames) == 0}
	return source.Collected{Items: items, Next: data, Cut: p.cut, Dropped: dropped, Warning: warning}, nil
}

// collect runs every read of a pass into p.msgs and moves p.cur's watermarks.
func (s *Source) collect(ctx context.Context, p *pass) error {
	if err := s.loadGroups(ctx, p); err != nil {
		return err
	}
	after := p.floor.Add(-searchBackoff).UTC().Format("2006-01-02")
	me := "<@" + s.user + ">"
	for _, q := range []string{"is:dm", "from:" + me} {
		if err := s.search(ctx, p, q+" after:"+after, p.floor, func(*message) bool { return true }); err != nil {
			return err
		}
	}
	// Mentions are searched over the whole 7 days every pass. One that was not seen before, though older than the
	// window, is a mention added by an edit or indexed late, and is kept.
	window := p.now.Add(-firstWindow)
	seen := map[string]bool{}
	err := s.search(ctx, p, me+" after:"+window.Add(-searchBackoff).UTC().Format("2006-01-02"), window, func(m *message) bool {
		seen[m.key()] = true
		_, before := p.cur.Mentions[m.key()]
		return !before || !tsTime(m.ts).Before(p.floor)
	})
	if err != nil {
		return err
	}
	for k, ts := range p.cur.Mentions {
		if tsTime(ts).Before(window) {
			delete(p.cur.Mentions, k)
		}
	}
	for k := range seen {
		_, ts, _ := strings.Cut(k, "/")
		p.cur.Mentions[k] = ts
	}
	if err := s.readChannels(ctx, p); err != nil {
		return err
	}
	if err := s.readGroupDMs(ctx, p); err != nil {
		return err
	}
	for _, m := range p.msgs {
		s.follow(p, m)
	}
	return s.readThreads(ctx, p)
}

func (s *Source) observed() time.Time {
	if t := s.api.clock.First(); !t.IsZero() {
		return t
	}
	return s.now().UTC()
}

// follow records a thread the person posted in or was mentioned in, outside one-to-one direct messages (search reads
// every message there). A thread followed for the first time is read from this pass's window, so a reply that comes
// before a later pass reads it is not lost.
func (s *Source) follow(p *pass, m *message) {
	if m.conversation() == kindIM {
		return
	}
	if !s.own(m) && !(s.mentions(m) && m.botID == "") {
		return
	}
	k := m.channel + "/" + m.root()
	t := p.cur.Threads[k]
	if t == nil {
		t = &thread{Joined: m.ts, Last: m.ts, Seen: p.floorTS}
		p.cur.Threads[k] = t
	}
	if kindRank[m.conversation()] > kindRank[t.Kind] {
		t.Kind = m.conversation()
	}
	if tsLess(m.ts, t.Joined) {
		t.Joined = m.ts
	}
	if tsLess(t.Last, m.ts) {
		t.Last = m.ts
	}
}

// readThreads reads the followed threads that are due.
func (s *Source) readThreads(ctx context.Context, p *pass) error {
	for k, t := range p.cur.Threads {
		if tsTime(t.Last).Before(p.now.Add(-followFor)) {
			delete(p.cur.Threads, k)
		}
	}
	for _, k := range dueOrder(p.cur.Threads, p.now, threadsPerPass) {
		t := p.cur.Threads[k]
		channel, root, _ := strings.Cut(k, "/")
		oldest := t.Seen
		if oldest == "" {
			oldest = p.floorTS
		}
		got, err := s.replies(ctx, p.tok, channel, root, oldest)
		if errors.Is(err, errGone) {
			delete(p.cur.Threads, k)
			continue
		}
		if err != nil {
			return err
		}
		for _, m := range got {
			if !tsLess(oldest, m.ts) {
				continue
			}
			m.followed, m.joined, m.kind = true, t.Joined, t.Kind
			if tsLess(t.Seen, m.ts) {
				t.Seen = m.ts
			}
			p.msgs.add(m)
			s.follow(p, m)
		}
		t.Polled = p.observed
	}
	return nil
}

// readChannels reads each configured channel's top level from its watermark. conversations.info says whether it is
// private; a private channel's messages are personal.
func (s *Source) readChannels(ctx context.Context, p *pass) error {
	keep := map[string]*watermark{}
	for _, c := range s.channels {
		w := p.cur.Channels[c]
		if w == nil {
			w = &watermark{Seen: p.floorTS}
		}
		keep[c] = w
		kind, err := s.info(ctx, p.tok, c)
		if errors.Is(err, errGone) {
			// Unreadable this pass (archived, or the person left). Its watermark stays, so nothing is skipped if it
			// comes back.
			continue
		}
		if err != nil {
			return err
		}
		w.Kind = kind
		if err := s.readHistory(ctx, p, c, w); err != nil && !errors.Is(err, errGone) {
			return err
		}
	}
	for c, w := range p.cur.Channels {
		if w.Kind == kindMPIM {
			keep[c] = w
		}
	}
	p.cur.Channels = keep
	return nil
}

// readGroupDMs lists the group direct messages the person is in and reads those that are due from their watermarks.
func (s *Source) readGroupDMs(ctx context.Context, p *pass) error {
	ids, err := s.listGroupDMs(ctx, p.tok)
	if err != nil {
		return err
	}
	s.groupDMs = map[string]bool{}
	marks := map[string]*watermark{}
	for _, c := range ids {
		s.groupDMs[c] = true
		w := p.cur.Channels[c]
		if w == nil || w.Kind != kindMPIM {
			w = &watermark{Seen: p.floorTS, Kind: kindMPIM}
		}
		marks[c] = w
	}
	for c, w := range p.cur.Channels {
		if w.Kind != kindMPIM {
			marks[c] = w
		}
	}
	p.cur.Channels = marks
	group := map[string]*watermark{}
	for _, c := range ids {
		group[c] = marks[c]
	}
	for _, c := range dueOrder(group, p.now, groupDMsPerPass) {
		if err := s.readHistory(ctx, p, c, marks[c]); err != nil && !errors.Is(err, errGone) {
			return err
		}
		marks[c].Polled = p.observed
	}
	return nil
}

// readHistory reads one conversation's top level after its watermark. Slack returns history newest first, so a read
// that stops at its page limit leaves a gap below what it read: the watermark stays, Gap is the oldest message read
// and Head the newest, and the next pass reads the gap before anything newer. Nothing is skipped.
func (s *Source) readHistory(ctx context.Context, p *pass, channel string, w *watermark) error {
	got, more, err := s.history(ctx, p.tok, channel, w.Seen, w.Gap)
	if err != nil {
		return err
	}
	newest, oldestRead := "", ""
	for _, m := range got {
		if !tsLess(w.Seen, m.ts) || (w.Gap != "" && !tsLess(m.ts, w.Gap)) {
			continue
		}
		m.history, m.kind = true, w.Kind
		if tsLess(newest, m.ts) {
			newest = m.ts
		}
		if oldestRead == "" || tsLess(m.ts, oldestRead) {
			oldestRead = m.ts
		}
		p.msgs.add(m)
	}
	switch {
	case more && oldestRead != "":
		if w.Gap == "" {
			w.Head = newest
		}
		w.Gap = oldestRead
	case w.Gap != "":
		w.Seen, w.Gap, w.Head = w.Head, "", ""
	case newest != "":
		w.Seen = newest
	}
	return nil
}
