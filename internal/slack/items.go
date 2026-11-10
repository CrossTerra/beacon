package slack

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

var (
	// A pull request or merge request link, and wording that asks for a review. Slack writes a link as <url|label>.
	reviewLink    = regexp.MustCompile(`https://[^\s|>]+/(pull|merge_requests)/[0-9]+`)
	reviewWording = regexp.MustCompile(`(?i)\b(review|reviews|reviewing|look|approve|approval|ptal|eyes)\b`)
	githubPull    = regexp.MustCompile(`https://github\.com/([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9._-]+)/pull/([0-9]+)`)
	jiraIssue     = regexp.MustCompile(`https://[a-z0-9][a-z0-9-]*\.atlassian\.net/browse/([A-Z][A-Z0-9]+-[0-9]+)`)
	// A user group mention is <!subteam^ID> or <!subteam^ID|@name>; a broadcast is <!here>, <!channel> or <!everyone>.
	groupMention     = regexp.MustCompile(`<!subteam\^([A-Z0-9]+)[|>]`)
	broadcast        = regexp.MustCompile(`<!(here|channel|everyone)[|>]`)
	printableSubject = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)
)

// Message subtypes that are messages someone wrote. Joins, edits, deletions, topic changes and the rest are not events.
var written = map[string]bool{"": true, "thread_broadcast": true, "file_share": true, "me_message": true, "bot_message": true}

// items turns the pool into items, one per message. web is the workspace's address, for permalinks Slack did not give.
func (s *Source) items(msgs pool, web string) ([]source.Item, int) {
	var out []source.Item
	dropped := 0
	for _, m := range msgs {
		if !written[m.subtype] {
			continue
		}
		typ := s.typeOf(m)
		if typ == "" {
			continue
		}
		it, ok := s.item(m, typ, web)
		if !ok {
			dropped++
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, dropped
}

// typeOf picks the item type for a message, or "" when it is not an item. The order is the documented precedence.
func (s *Source) typeOf(m *message) string {
	if s.own(m) {
		// The person's own message is an event in a conversation Beacon follows: a direct or group direct message, a
		// private channel, a thread, or a configured channel. A top-level message elsewhere answers no ask Beacon
		// recorded.
		if m.personal() || m.inThread() || s.api.configured(m.channel) {
			return TypeOwn
		}
		return ""
	}
	kind := m.conversation()
	switch {
	case s.mentions(m):
		return TypeMentioned
	case kind == kindIM || kind == kindMPIM:
		return TypeDM
	case m.followed && m.inThread() && tsLess(m.joined, m.ts):
		return TypeThreadReply
	case m.history && s.api.configured(m.channel) && !m.inThread() && reviewLink.MatchString(m.text) && reviewWording.MatchString(m.text):
		return TypeReviewRequest
	}
	return ""
}

// mentions reports whether a message names the person: directly, through a user group they are in, or, in a
// configured channel only, through @here, @channel or @everyone.
func (s *Source) mentions(m *message) bool {
	if strings.Contains(m.text, "<@"+s.user+">") || strings.Contains(m.text, "<@"+s.user+"|") {
		return true
	}
	for _, g := range groupMention.FindAllStringSubmatch(m.text, -1) {
		if s.groups[g[1]] {
			return true
		}
	}
	return s.api.configured(m.channel) && broadcast.MatchString(m.text)
}

func (s *Source) subject(channel, root, kind string) string {
	if kind != kindChannel {
		return PersonalStem + s.person + ":" + channel + ":" + root
	}
	return "slack:" + channel + ":" + root
}

func (s *Source) item(m *message, typ, web string) (source.Item, bool) {
	author, actor := m.user, "slack:user:"+m.user
	if m.botID != "" {
		author, actor = m.botID, "slack:bot:"+m.botID
	}
	if !idPattern.MatchString(author) {
		return source.Item{}, false
	}
	at := tsTime(m.ts)
	if at.IsZero() {
		return source.Item{}, false
	}
	kind := m.conversation()
	fields := map[string]string{"channel": m.channel, "channel_type": kind, "ts": m.ts, "author": author}
	if m.inThread() {
		fields["thread_ts"] = m.threadTS
	}
	if p := permalink(m, web); p != "" {
		fields["permalink"] = p
	}
	if s.bodies && m.text != "" {
		fields["body"] = m.text
	}
	key, version := source.FitKey(s.name+":"+m.channel+":"+m.ts, m.ts)
	it := source.Item{
		Source:     s.name,
		Subject:    s.subject(m.channel, m.root(), kind),
		Key:        key,
		Type:       typ,
		EventTitle: title(typ, m.inThread()),
		OccurredAt: at,
		Actor:      actor,
		Fields:     fields,
		References: references(m.text),
		Version:    version,
	}
	switch typ {
	case TypeOwn:
		it.Own = true
		// A top-level reply in a one-to-one direct message answers the earlier top-level messages there, each its own
		// subject. Not later ones, and not replies inside a thread.
		if kind == kindIM && !m.inThread() {
			it.CloseSiblingsBefore = m.ts
		}
	case TypeDM, TypeMentioned:
		it.Addressed = true
	}
	return it, true
}

// track keeps every message that addressed the person, for 7 days, to notice its deletion. Seen this pass, it exists.
func (s *Source) track(p *pass, items []source.Item) {
	for _, it := range items {
		if !it.Addressed {
			continue
		}
		root := it.Fields["thread_ts"]
		p.cur.Asked[it.Fields["channel"]+"/"+it.Fields["ts"]] = &asked{Subject: it.Subject, Root: root, Kind: it.Fields["channel_type"], Checked: p.observed}
	}
	for k, a := range p.cur.Asked {
		if p.msgs[k] != nil {
			a.Checked = p.observed
		}
	}
}

// checkDeleted reads the tracked asks that are due, at most checksPerPass, and yields message_deleted for each that
// is gone, on the ask's subject, so a close_by_others rule can close it.
func (s *Source) checkDeleted(ctx context.Context, p *pass) ([]source.Item, error) {
	for k := range p.cur.Asked {
		_, ts, _ := strings.Cut(k, "/")
		if tsTime(ts).Before(p.now.Add(-followFor)) {
			delete(p.cur.Asked, k)
		}
	}
	var keys []string
	for k := range p.cur.Asked {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := p.cur.Asked[keys[i]].Checked, p.cur.Asked[keys[j]].Checked
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	var out []source.Item
	checks := 0
	for _, k := range keys {
		a := p.cur.Asked[k]
		if checked, err := time.Parse(time.RFC3339, a.Checked); err == nil && p.now.Sub(checked) < quietEvery {
			continue
		}
		if checks == checksPerPass {
			break
		}
		checks++
		channel, ts, _ := strings.Cut(k, "/")
		root := a.Root
		if root == "" {
			root = ts
		}
		alive, err := s.exists(ctx, p.tok, channel, root, ts)
		switch {
		case errors.Is(err, errMessageGone):
			alive = false
		case errors.Is(err, errGone):
			// The conversation cannot be read now; that says nothing about the message. Try again in 15 minutes.
			a.Checked = p.observed
			continue
		case err != nil:
			return nil, err
		}
		if alive {
			a.Checked = p.observed
			continue
		}
		delete(p.cur.Asked, k)
		obs := s.observed()
		fields := map[string]string{"channel": channel, "channel_type": a.Kind, "ts": ts}
		if a.Root != "" {
			fields["thread_ts"] = a.Root
		}
		key, version := source.FitKey(s.name+":"+channel+":"+ts+":deleted", "deleted")
		out = append(out, source.Item{
			Source: s.name, Subject: a.Subject, Key: key, Type: TypeDeleted, EventTitle: "Deleted",
			OccurredAt: obs, Fields: fields, Version: version,
		})
	}
	return out, nil
}

// title describes the change. It never holds message text or a name.
func title(typ string, inThread bool) string {
	switch typ {
	case TypeOwn:
		if inThread {
			return "Replied"
		}
		return "Sent"
	case TypeMentioned:
		return "Mentioned"
	case TypeDM:
		return "Direct message"
	case TypeThreadReply:
		return "Thread reply"
	case TypeReviewRequest:
		return "Review requested"
	}
	return ""
}

// permalink is Slack's, when it is an https link on slack.com, else one built from the workspace address.
func permalink(m *message, web string) string {
	if u, err := url.Parse(m.permalink); err == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" && strings.HasSuffix(strings.ToLower(u.Hostname()), ".slack.com") {
		return m.permalink
	}
	if web == "" {
		return ""
	}
	p := web + "/archives/" + m.channel + "/p" + strings.Replace(m.ts, ".", "", 1)
	if m.inThread() {
		p += "?thread_ts=" + m.threadTS + "&cid=" + m.channel
	}
	return p
}

// references are explicit links to other subjects (DEC-17): GitHub pull requests and Jira issues.
func references(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range githubPull.FindAllStringSubmatch(text, -1) {
		seen["github:pr:"+g[1]+"/"+g[2]+"#"+g[3]] = true
	}
	for _, j := range jiraIssue.FindAllStringSubmatch(text, -1) {
		seen["jira:"+j[1]] = true
	}
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}
