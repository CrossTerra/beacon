package slack

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
)

// searchChannel is the conversation a search match is in. IsPrivate is a pointer: a match that does not say is not
// treated as public.
type searchChannel struct {
	ID        string `json:"id"`
	IsIM      bool   `json:"is_im"`
	IsMPIM    bool   `json:"is_mpim"`
	IsPrivate *bool  `json:"is_private"`
}

func (c searchChannel) kind() string {
	switch {
	case c.IsIM || strings.HasPrefix(c.ID, "D"):
		return kindIM
	case c.IsMPIM:
		return kindMPIM
	case c.IsPrivate == nil || *c.IsPrivate || !strings.HasPrefix(c.ID, "C"):
		return kindPrivate
	}
	return kindChannel
}

// search reads one query, newest first, and adds the matches at or after floor that keep accepts. A query that stops
// at its page limit with older matches in the window unread marks the pass cut.
func (s *Source) search(ctx context.Context, p *pass, query string, floor time.Time, keep func(*message) bool) error {
	for page := 1; page <= searchPages; page++ {
		q := url.Values{"query": {query}, "count": {"100"}, "page": {strconv.Itoa(page)}, "sort": {"timestamp"}, "sort_dir": {"desc"}}
		var resp struct {
			Messages struct {
				Matches []struct {
					Channel   searchChannel `json:"channel"`
					User      string        `json:"user"`
					BotID     string        `json:"bot_id"`
					Subtype   string        `json:"subtype"`
					TS        string        `json:"ts"`
					ThreadTS  string        `json:"thread_ts"`
					Text      string        `json:"text"`
					Permalink string        `json:"permalink"`
				} `json:"matches"`
				Paging struct {
					Pages int `json:"pages"`
				} `json:"paging"`
			} `json:"messages"`
		}
		if _, err := s.api.call(ctx, p.tok, methodSearch, q, &resp); err != nil {
			return err
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
			m := &message{
				channel: r.Channel.ID, ts: r.TS, threadTS: threadTS, user: r.User, botID: r.BotID, subtype: r.Subtype,
				text: r.Text, permalink: r.Permalink, kind: r.Channel.kind(),
			}
			if keep(m) {
				p.msgs.add(m)
			}
		}
		if older || page >= resp.Messages.Paging.Pages {
			return nil
		}
	}
	p.cut = true
	return nil
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

// apiMessage is a message as conversations.replies and conversations.history return it.
type apiMessage struct {
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

// replies reads a thread after oldest, oldest first. A long thread stops at the page limit; the watermark moves only
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

// exists reads one message: ts in the thread rooted at root (root is ts for a top-level message). It reports false
// when Slack no longer has it, or has only its tombstone.
func (s *Source) exists(ctx context.Context, tok keychain.Secret, channel, root, ts string) (bool, error) {
	q := url.Values{"channel": {channel}, "ts": {root}, "oldest": {ts}, "inclusive": {"true"}, "limit": {"2"}}
	var resp struct {
		Messages []apiMessage `json:"messages"`
	}
	if _, err := s.api.call(ctx, tok, methodReplies, q, &resp); err != nil {
		return false, err
	}
	for _, m := range resp.Messages {
		if m.TS == ts {
			return m.Subtype != "tombstone", nil
		}
	}
	return false, nil
}

// history reads a conversation's top level after oldest and, when latest is set, before it, newest first. more
// reports that it stopped at its page limit.
func (s *Source) history(ctx context.Context, tok keychain.Secret, channel, oldest, latest string) ([]*message, bool, error) {
	var out []*message
	next := ""
	for page := 0; page < historyPages; page++ {
		q := url.Values{"channel": {channel}, "oldest": {oldest}, "limit": {"200"}}
		if latest != "" {
			q.Set("latest", latest)
		}
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

// info reads a configured channel's kind. A private channel is personal.
func (s *Source) info(ctx context.Context, tok keychain.Secret, channel string) (string, error) {
	var resp struct {
		Channel struct {
			IsPrivate bool `json:"is_private"`
			IsMPIM    bool `json:"is_mpim"`
			IsIM      bool `json:"is_im"`
		} `json:"channel"`
	}
	if _, err := s.api.call(ctx, tok, methodInfo, url.Values{"channel": {channel}}, &resp); err != nil {
		return "", err
	}
	switch {
	case resp.Channel.IsIM:
		return kindIM, nil
	case resp.Channel.IsMPIM:
		return kindMPIM, nil
	case resp.Channel.IsPrivate:
		return kindPrivate, nil
	}
	return kindChannel, nil
}

// listGroupDMs lists the open group direct messages the person is in.
func (s *Source) listGroupDMs(ctx context.Context, tok keychain.Secret) ([]string, error) {
	var out []string
	next := ""
	for page := 0; page < listPages; page++ {
		q := url.Values{"types": {"mpim"}, "exclude_archived": {"true"}, "limit": {"200"}}
		if next != "" {
			q.Set("cursor", next)
		}
		var resp struct {
			Channels []struct {
				ID string `json:"id"`
			} `json:"channels"`
			envelope
		}
		if _, err := s.api.call(ctx, tok, methodGroupDMs, q, &resp); err != nil {
			return nil, err
		}
		for _, c := range resp.Channels {
			if channelPattern.MatchString(c.ID) && !strings.HasPrefix(c.ID, "D") {
				out = append(out, c.ID)
			}
		}
		next = resp.Metadata.NextCursor
		if next == "" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("slack: the group direct message list did not end")
}

// loadGroups reads the user groups the person is in, once an hour. A message that mentions one mentions the person.
func (s *Source) loadGroups(ctx context.Context, p *pass) error {
	if s.groups != nil && p.now.Sub(s.groupsAt) < groupsEvery && !p.now.Before(s.groupsAt) {
		return nil
	}
	var resp struct {
		Groups []struct {
			ID    string   `json:"id"`
			Users []string `json:"users"`
		} `json:"usergroups"`
	}
	if _, err := s.api.call(ctx, p.tok, methodUserGroups, url.Values{"include_users": {"true"}}, &resp); err != nil {
		return err
	}
	groups := map[string]bool{}
	for _, g := range resp.Groups {
		for _, u := range g.Users {
			if u == s.user && idPattern.MatchString(g.ID) {
				groups[g.ID] = true
			}
		}
	}
	s.groups, s.groupsAt = groups, p.now
	return nil
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
