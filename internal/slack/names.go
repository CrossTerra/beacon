package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/source"
)

// Fields that hold names. They are set only when names can be read (NameScopes), and leave only when a rule lists them.
const (
	FieldTitle       = "title"
	FieldChannelName = "channel_name"
	FieldAuthorName  = "author_name"
)

const (
	// nameTTL is how long a name read from Slack is used before it is read again; missTTL is how long a user or
	// conversation Slack would not describe waits before it is asked about again.
	nameTTL = 24 * time.Hour
	missTTL = time.Hour
	// groupNames is how many other members a group direct message's title names; the rest are a count.
	groupNames = 3
	// nameMax caps one name, in characters.
	nameMax = 64
	// membersLimit is the one page of members read for a group direct message. Slack caps those at nine people.
	membersLimit = "50"
)

// CacheFile is the name cache's file name for a source, in the state directory.
func CacheFile(name string) string { return "slack-names-" + name + ".json" }

// nameCache holds display names by id, read from Slack, with when each was read. It is kept in the state directory
// (0600) so later passes do not ask again. It holds ids, names and times: no message text, no token.
type nameCache struct {
	Users         map[string]*cachedUser `json:"users"`
	Conversations map[string]*cachedConv `json:"conversations"`
	dirty         bool
}

type cachedUser struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

// cachedConv is a conversation: its kind, its name (channels), the other person (direct messages) and the members
// (group direct messages). Gone is set when Slack would not describe it.
type cachedConv struct {
	Kind    string    `json:"kind,omitempty"`
	Name    string    `json:"name,omitempty"`
	User    string    `json:"user,omitempty"`
	Members []string  `json:"members,omitempty"`
	Gone    bool      `json:"gone,omitempty"`
	At      time.Time `json:"at"`
}

func newCache() *nameCache {
	return &nameCache{Users: map[string]*cachedUser{}, Conversations: map[string]*cachedConv{}}
}

// loadCache reads the cache at path. A missing or unreadable file is an empty cache: it is only a cache. Entries with
// ids or names Beacon would not have written are dropped.
func loadCache(path string, now time.Time) *nameCache {
	c := newCache()
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var read nameCache
	if json.Unmarshal(data, &read) != nil {
		return c
	}
	for id, u := range read.Users {
		if u != nil && userPattern.MatchString(id) && u.Name == cleanName(u.Name, nameMax) && fresh(u.At, u.Name == "", now) {
			c.Users[id] = u
		}
	}
	for id, v := range read.Conversations {
		if v == nil || !channelPattern.MatchString(id) || !kinds[v.Kind] || v.Name != cleanName(v.Name, nameMax) ||
			(v.User != "" && !userPattern.MatchString(v.User)) || !fresh(v.At, v.Gone, now) {
			continue
		}
		ok := true
		for _, m := range v.Members {
			ok = ok && userPattern.MatchString(m)
		}
		if ok {
			c.Conversations[id] = v
		}
	}
	return c
}

func fresh(at time.Time, miss bool, now time.Time) bool {
	ttl := nameTTL
	if miss {
		ttl = missTTL
	}
	return !at.IsZero() && !at.After(now.Add(time.Minute)) && now.Sub(at) < ttl
}

// save writes the cache atomically, 0600, when it changed.
func (c *nameCache) save(path string) error {
	if path == "" || !c.dirty {
		return nil
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
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
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

// cleanName keeps a name to one line of printable characters: control and format characters (bidirectional
// overrides, zero-width marks) are dropped, runs of space become one, and it is cut at max characters.
func cleanName(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == utf8.RuneError, unicode.Is(unicode.Cf, r):
			return -1
		case unicode.IsControl(r), unicode.IsSpace(r):
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = strings.TrimSpace(string([]rune(s)[:max]))
	}
	return s
}

// errNamesScope is a token without a scope names need. errNamesFailed is any other lookup failure. Neither fails a
// pass: titles and names are left out.
var (
	errNamesScope  = errors.New("slack: the token cannot read names")
	errNamesFailed = errors.New("slack: names could not be read")
)

// resolver reads names for one pass, through the cache. After the first failure it stops calling Slack and answers
// "" for anything it does not already hold.
type resolver struct {
	s      *Source
	tok    keychain.Secret
	now    time.Time
	failed error
}

func (r *resolver) fail(ctx context.Context, err error) {
	if r.failed != nil {
		return
	}
	switch {
	case ctx.Err() != nil:
		r.failed = ctx.Err()
	case errors.Is(err, source.ErrUnauthorized):
		// missing_scope, from a token whose scopes auth.test did not list.
		r.failed = errNamesScope
	default:
		r.failed = errNamesFailed
	}
}

// warning is the fixed code a pass reports for the first failure, or "".
func (r *resolver) warning() string {
	switch {
	case r.failed == nil:
		return ""
	case errors.Is(r.failed, errNamesScope):
		return source.WarnNamesScope
	}
	return source.WarnNamesFailed
}

// user is a user's display name: the full name, else the display name, else the handle. "" for a bot, an unknown
// user, or after a failure.
func (r *resolver) user(ctx context.Context, id string) string {
	if !userPattern.MatchString(id) {
		return ""
	}
	cache := r.s.cache
	if u := cache.Users[id]; u != nil && fresh(u.At, u.Name == "", r.now) {
		return u.Name
	}
	if r.failed != nil {
		return ""
	}
	var resp struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				RealName    string `json:"real_name"`
				DisplayName string `json:"display_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	name := ""
	_, err := r.s.api.call(ctx, r.tok, methodUser, url.Values{"user": {id}}, &resp)
	switch {
	case errors.Is(err, errGone):
	case err != nil:
		r.fail(ctx, err)
		return ""
	default:
		for _, n := range []string{resp.User.Profile.RealName, resp.User.RealName, resp.User.Profile.DisplayName, resp.User.Name} {
			if name = cleanName(n, nameMax); name != "" {
				break
			}
		}
	}
	cache.Users[id] = &cachedUser{Name: name, At: r.now}
	cache.dirty = true
	return name
}

// conv describes a conversation: conversations.info, and conversations.members for a group direct message. Nil for
// one Slack would not describe, or after a failure.
func (r *resolver) conv(ctx context.Context, id string) *cachedConv {
	cache := r.s.cache
	if c := cache.Conversations[id]; c != nil && fresh(c.At, c.Gone, r.now) {
		if c.Gone {
			return nil
		}
		return c
	}
	if r.failed != nil {
		return nil
	}
	var resp struct {
		Channel struct {
			Name      string `json:"name"`
			User      string `json:"user"`
			IsIM      bool   `json:"is_im"`
			IsMPIM    bool   `json:"is_mpim"`
			IsPrivate bool   `json:"is_private"`
		} `json:"channel"`
	}
	_, err := r.s.api.call(ctx, r.tok, methodInfo, url.Values{"channel": {id}}, &resp)
	if errors.Is(err, errGone) {
		cache.Conversations[id] = &cachedConv{Gone: true, At: r.now}
		cache.dirty = true
		return nil
	}
	if err != nil {
		r.fail(ctx, err)
		return nil
	}
	c := &cachedConv{At: r.now}
	ch := resp.Channel
	switch {
	case ch.IsIM || strings.HasPrefix(id, "D"):
		c.Kind = kindIM
		if userPattern.MatchString(ch.User) {
			c.User = ch.User
		}
	case ch.IsMPIM:
		c.Kind = kindMPIM
		var members struct {
			Members []string `json:"members"`
		}
		_, err := r.s.api.call(ctx, r.tok, methodMembers, url.Values{"channel": {id}, "limit": {membersLimit}}, &members)
		if err != nil && !errors.Is(err, errGone) {
			r.fail(ctx, err)
			return nil
		}
		for _, m := range members.Members {
			if userPattern.MatchString(m) {
				c.Members = append(c.Members, m)
			}
		}
		sort.Strings(c.Members)
	case ch.IsPrivate:
		c.Kind, c.Name = kindPrivate, cleanName(ch.Name, nameMax)
	default:
		c.Kind, c.Name = kindChannel, cleanName(ch.Name, nameMax)
	}
	cache.Conversations[id] = c
	cache.dirty = true
	return c
}

// people names the others in a direct or group direct message: the other person, or up to groupNames members
// other than the person, then how many more. "" when no name could be read.
func (r *resolver) people(ctx context.Context, c *cachedConv) string {
	if c.Kind == kindIM {
		return r.user(ctx, c.User)
	}
	var others []string
	for _, m := range c.Members {
		if m != r.s.user {
			others = append(others, m)
		}
	}
	var names []string
	for _, m := range others {
		if len(names) == groupNames {
			break
		}
		if n := r.user(ctx, m); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	out := strings.Join(names, ", ")
	if more := len(others) - len(names); more > 0 {
		out += " +" + strconv.Itoa(more)
	}
	return out
}

// conversationName is the channel_name field: a channel's name, the other person in a direct message, the others in
// a group direct message.
func (r *resolver) conversationName(ctx context.Context, c *cachedConv) string {
	switch c.Kind {
	case kindIM, kindMPIM:
		return r.people(ctx, c)
	}
	return c.Name
}

// titleFor is a thread title for a conversation, from who and where, never what was said. root is the thread's root
// message when it was read, else nil. It is "" when the names it needs could not be read.
func (s *Source) titleFor(ctx context.Context, r *resolver, c *cachedConv, root *message) string {
	if c == nil {
		return ""
	}
	own := root != nil && s.own(root)
	switch c.Kind {
	case kindIM, kindMPIM:
		who := r.people(ctx, c)
		switch {
		case who == "":
			return ""
		case own:
			return "You to " + who
		case c.Kind == kindIM:
			return "DM with " + who
		}
		return "Group DM: " + who
	}
	if c.Name == "" {
		return ""
	}
	where := "#" + c.Name
	switch {
	case root == nil:
		return where + " thread"
	case own:
		return "You in " + where
	case root.botID != "":
		return where + " thread"
	case s.mentions(root):
		if who := r.user(ctx, root.user); who != "" {
			return where + ": " + who + " mentioned you"
		}
	case s.api.configured(root.channel) && !root.inThread() && reviewLink.MatchString(root.text) && reviewWording.MatchString(root.text):
		if who := r.user(ctx, root.user); who != "" {
			return where + ": " + who + " asked for a review"
		}
	}
	return where + " thread"
}

// nameItems adds title, channel_name and author_name to each item when a rule lists one of them. The title is the
// conversation's, so every item on a subject carries the same one. It returns the warning to report, or "".
func (s *Source) nameItems(ctx context.Context, p *pass, id Identity, items []source.Item) string {
	if !s.names || len(items) == 0 {
		return ""
	}
	if len(id.MissingNames) > 0 {
		return source.WarnNamesScope
	}
	named := map[string]bool{}
	for _, it := range items {
		if it.Type != TypeDeleted {
			named[it.Fields["channel"]] = true
		}
	}
	s.api.named = func(c string) bool { return named[c] }
	defer func() { s.api.named = func(string) bool { return false } }()
	r := &resolver{s: s, tok: p.tok, now: p.now}
	titles := map[string]string{}
	for i := range items {
		it := &items[i]
		if it.Type == TypeDeleted {
			continue
		}
		channel := it.Fields["channel"]
		c := r.conv(ctx, channel)
		if c == nil {
			continue
		}
		// After a failed lookup a name may be missing, and what is built from it would be a fallback ("#eng
		// thread") or a partial list. A thread opened with that title would never be retitled, so it is left out:
		// the thread is named by its event, and a later pass can retitle it.
		if n := r.conversationName(ctx, c); n != "" && r.failed == nil {
			it.Fields[FieldChannelName] = n
		}
		if !it.Own {
			if n := r.user(ctx, it.Fields["author"]); n != "" {
				it.Fields[FieldAuthorName] = n
			}
		}
		t, ok := titles[it.Subject]
		if !ok {
			root := p.msgs[channel+"/"+rootOf(it)]
			t = s.titleFor(ctx, r, c, root)
			if r.failed != nil {
				t = ""
			}
			titles[it.Subject] = t
		}
		if t != "" {
			it.Fields[FieldTitle] = t
		}
	}
	return r.warning()
}

func rootOf(it *source.Item) string {
	if t := it.Fields["thread_ts"]; t != "" {
		return t
	}
	return it.Fields["ts"]
}

// titling is what a pass leaves for ThreadTitle: the token it read and whether names may be read. It lasts until the
// next Collect.
type titling struct {
	tok   keychain.Secret
	names bool
}

// ThreadTitle is the title an item on subject would carry now, for a thread Beacon opened before titles existed. It
// reads the thread's root message and the conversation's names, with the token the last Collect read. "" with a nil
// error means names cannot be read now (no scope, a failed lookup, or no pass ran): ask again later.
func (s *Source) ThreadTitle(ctx context.Context, subject string) (string, error) {
	t := s.titling
	if t == nil || !t.names {
		return "", nil
	}
	channel, root, ok := s.parseSubject(subject)
	if !ok {
		return "", source.ErrNoTitle
	}
	s.api.named = func(c string) bool { return c == channel }
	defer func() { s.api.named = func(string) bool { return false } }()
	r := &resolver{s: s, tok: t.tok, now: s.now().UTC()}
	defer s.flush()
	c := r.conv(ctx, channel)
	if r.failed != nil {
		return "", ctx.Err()
	}
	if c == nil {
		return "", source.ErrNoTitle
	}
	var rootMsg *message
	q := url.Values{"channel": {channel}, "ts": {root}, "oldest": {root}, "inclusive": {"true"}, "limit": {"1"}}
	var resp struct {
		Messages []apiMessage `json:"messages"`
	}
	_, err := s.api.call(ctx, t.tok, methodReplies, q, &resp)
	switch {
	case errors.Is(err, errMessageGone):
	case errors.Is(err, errGone):
		return "", source.ErrNoTitle
	case err != nil:
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// A rate limit or an error says nothing about the thread. Try it on a later pass.
		return "", nil
	}
	for _, m := range resp.Messages {
		if m.TS == root && m.Subtype != "tombstone" {
			rootMsg = m.message(channel)
		}
	}
	title := s.titleFor(ctx, r, c, rootMsg)
	if r.failed != nil {
		return "", ctx.Err()
	}
	return title, nil
}

// flush writes the name cache when this pass may write state. The cache is only a cache: a write that fails costs a
// lookup later, not a pass.
func (s *Source) flush() {
	if s.persist {
		_ = s.cache.save(s.cachePath)
	}
}

// parseSubject reads one of this source's subjects: slack:<channel>:<root>, or slack:dm:<person>:<channel>:<root>.
func (s *Source) parseSubject(subject string) (channel, root string, ok bool) {
	rest, personal := strings.CutPrefix(subject, PersonalStem)
	if personal {
		person, after, found := strings.Cut(rest, ":")
		if !found || person != s.person {
			return "", "", false
		}
		rest = after
	} else if rest, ok = strings.CutPrefix(subject, "slack:"); !ok {
		return "", "", false
	}
	channel, root, ok = strings.Cut(rest, ":")
	if !ok || !channelPattern.MatchString(channel) || !tsPattern.MatchString(root) {
		return "", "", false
	}
	if !personal && !strings.HasPrefix(channel, "C") {
		return "", "", false
	}
	return channel, root, true
}
