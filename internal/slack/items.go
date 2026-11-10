package slack

import (
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/CrossTerra/beacon/internal/source"
)

var (
	// A pull request or merge request link, and wording that asks for a review. Slack writes a link as <url|label>.
	reviewLink    = regexp.MustCompile(`https://[^\s|>]+/(pull|merge_requests)/[0-9]+`)
	reviewWording = regexp.MustCompile(`(?i)\b(review|reviews|reviewing|look|approve|approval|ptal|eyes)\b`)
	githubPull    = regexp.MustCompile(`https://github\.com/([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9._-]+)/pull/([0-9]+)`)
	jiraIssue     = regexp.MustCompile(`https://[a-z0-9][a-z0-9-]*\.atlassian\.net/browse/([A-Z][A-Z0-9]+-[0-9]+)`)
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
		// thread, or a configured channel. A top-level message elsewhere answers no ask Beacon recorded.
		if m.personal() || m.inThread() || s.api.allowHistory(m.channel) {
			return TypeOwn
		}
		return ""
	}
	switch {
	case s.mentions(m.text):
		return TypeMentioned
	case m.direct():
		return TypeDM
	case m.followed && m.inThread() && tsLess(m.joined, m.ts):
		return TypeThreadReply
	case m.history && !m.inThread() && reviewLink.MatchString(m.text) && reviewWording.MatchString(m.text):
		return TypeReviewRequest
	}
	return ""
}

func (s *Source) mentions(text string) bool {
	return strings.Contains(text, "<@"+s.user+">") || strings.Contains(text, "<@"+s.user+"|")
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
	subject := "slack:" + m.channel + ":" + m.root()
	if m.personal() {
		subject = PersonalStem + s.person + ":" + m.channel + ":" + m.root()
	}
	fields := map[string]string{"channel": m.channel, "ts": m.ts, "author": author}
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
		Subject:    subject,
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
		// A top-level reply in a one-to-one direct message answers the earlier messages there, each its own subject.
		it.CloseSiblings = m.direct() && !m.inThread()
	case TypeDM, TypeMentioned:
		it.Addressed = true
	}
	return it, true
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
