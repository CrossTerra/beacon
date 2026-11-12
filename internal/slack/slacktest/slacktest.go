// Package slacktest is a fake Slack Web API for tests: auth.test, search.messages, conversations.replies,
// conversations.history, conversations.info, conversations.members and users.info, with recorded answers for one
// person, U0WINTON in workspace T0ACME, at 2026-11-08T12:00:00Z.
package slacktest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Fixed values the fixtures use.
const (
	Date      = "Sun, 08 Nov 2026 12:00:00 GMT"
	Token     = "xoxp-1111-2222-3333-tokenvalue"
	Workspace = "T0ACME"
	User      = "U0WINTON"
	// PassScopes are the read scopes a pass needs; AllScopes adds the ones display names need.
	PassScopes = "search:read,channels:history,groups:history,im:history,mpim:history,channels:read,groups:read,mpim:read,usergroups:read"
	AllScopes  = PassScopes + ",users:read,im:read"
	// Review is the configured channel.
	Review = "C0REVIEW"
)

// Server is a fake Slack. Every handler can be replaced through Routes before the first request.
type Server struct {
	*httptest.Server
	t testing.TB

	mu sync.Mutex
	// Scopes is the X-OAuth-Scopes header auth.test answers with.
	Scopes string
	// Routes maps a method path ("/api/auth.test") to its handler.
	Routes   map[string]http.HandlerFunc
	requests []*http.Request
	// Deleted holds "<channel>/<ts>" of messages that no longer exist.
	Deleted map[string]bool
}

// New starts a fake Slack with the recorded answers.
func New(t testing.TB) *Server {
	s := &Server{t: t, Scopes: AllScopes, Deleted: map[string]bool{}}
	s.Routes = map[string]http.HandlerFunc{
		"/api/auth.test":             s.auth,
		"/api/search.messages":       s.search,
		"/api/conversations.replies": s.replies,
		"/api/conversations.history": s.history,
		"/api/conversations.info":    s.info,
		"/api/conversations.members": s.members,
		"/api/users.info":            s.user,
		"/api/users.conversations":   Text(`{"ok":true,"channels":[{"id":"G0GROUP","is_mpim":true}],"response_metadata":{"next_cursor":""}}`),
		"/api/usergroups.list":       Text(UserGroups),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Text answers with body.
func Text(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) }
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Date", Date)
	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	s.requests = append(s.requests, r.Clone(r.Context()))
	h := s.Routes[r.URL.Path]
	s.mu.Unlock()
	if r.Method != http.MethodGet {
		s.t.Errorf("slacktest: %s %s; Beacon only reads", r.Method, r.URL.Path)
	}
	if r.Header.Get("Authorization") != "Bearer "+Token {
		io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
		return
	}
	if h == nil {
		s.t.Errorf("slacktest: unexpected request %s", r.URL.Path)
		io.WriteString(w, `{"ok":false,"error":"unknown_method"}`)
		return
	}
	h(w, r)
}

// Requests returns every request made so far.
func (s *Server) Requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...)
}

// Queries returns the query of every request to path, in order.
func (s *Server) Queries(path string) []url.Values {
	var out []url.Values
	for _, r := range s.Requests() {
		if r.URL.Path == path {
			out = append(out, r.URL.Query())
		}
	}
	return out
}

func (s *Server) auth(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	scopes := s.Scopes
	s.mu.Unlock()
	w.Header().Set("X-OAuth-Scopes", scopes)
	io.WriteString(w, `{"ok":true,"url":"https://acme.slack.com/","team":"Acme","user":"winton","team_id":"`+Workspace+`","user_id":"`+User+`"}`)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch query := q.Get("query"); {
	case strings.HasPrefix(query, "is:dm "):
		if q.Get("page") == "2" {
			io.WriteString(w, SearchDMPage2)
			return
		}
		io.WriteString(w, SearchDMPage1)
	case strings.HasPrefix(query, "<@"+User+"> "):
		io.WriteString(w, SearchMentions)
	case strings.HasPrefix(query, "from:<@"+User+"> "):
		io.WriteString(w, SearchOwn)
	default:
		s.t.Errorf("slacktest: unexpected search %q", query)
		io.WriteString(w, `{"ok":false,"error":"invalid_arguments"}`)
	}
}

// Users are the people users.info describes: the full name a title uses.
var Users = map[string]string{
	"U0WINTON": "Winton Welsh", "U0DANA": "Dana Scully", "U0BOB": "Bob Ross", "U0CAROL": "Carol Danvers",
	"U0HR": "Hana Reyes", "U0ERIN": "Erin Burnett", "U0FRANK": "Frank Ocean",
}

// Conversations are what conversations.info answers, by id.
var Conversations = map[string]string{
	"C0ENG":    `{"id":"C0ENG","name":"eng","is_channel":true,"is_private":false}`,
	"C0RANDOM": `{"id":"C0RANDOM","name":"random","is_channel":true,"is_private":false}`,
	"C0REVIEW": `{"id":"C0REVIEW","name":"review","is_channel":true,"is_private":false}`,
	"C0SECRET": `{"id":"C0SECRET","name":"secret","is_channel":true,"is_private":true}`,
	"D0DANA":   `{"id":"D0DANA","is_im":true,"user":"U0DANA"}`,
	"D0BOT":    `{"id":"D0BOT","is_im":true,"user":"USLACKBOT"}`,
	"G0GROUP":  `{"id":"G0GROUP","name":"mpdm-winton--dana--carol-1","is_mpim":true,"is_private":true}`,
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	if c, ok := Conversations[r.URL.Query().Get("channel")]; ok {
		io.WriteString(w, `{"ok":true,"channel":`+c+`}`)
		return
	}
	io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
}

func (s *Server) members(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("channel") == "G0GROUP" {
		io.WriteString(w, `{"ok":true,"members":["U0WINTON","U0DANA","U0CAROL"],"response_metadata":{"next_cursor":""}}`)
		return
	}
	io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
}

func (s *Server) user(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("user")
	name, ok := Users[id]
	if !ok {
		io.WriteString(w, `{"ok":false,"error":"user_not_found"}`)
		return
	}
	first, _, _ := strings.Cut(name, " ")
	io.WriteString(w, `{"ok":true,"user":{"id":"`+id+`","name":"`+strings.ToLower(first)+`","real_name":"`+name+`","profile":{"real_name":"`+name+`","display_name":"`+strings.ToLower(first)+`"}}}`)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("channel") {
	case Review:
		io.WriteString(w, HistoryReview)
	case "G0GROUP":
		io.WriteString(w, HistoryGroup)
	default:
		io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
	}
}

func (s *Server) replies(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("inclusive") == "true" {
		// A check that one message still exists.
		s.mu.Lock()
		gone := s.Deleted[q.Get("channel")+"/"+q.Get("oldest")]
		s.mu.Unlock()
		if gone {
			io.WriteString(w, `{"ok":false,"error":"thread_not_found"}`)
			return
		}
		if m, ok := known()[q.Get("channel")+"/"+q.Get("oldest")]; ok {
			io.WriteString(w, `{"ok":true,"messages":[`+m+`]}`)
			return
		}
		io.WriteString(w, `{"ok":true,"messages":[{"type":"message","user":"U0X","text":"x","ts":"`+q.Get("oldest")+`"}]}`)
		return
	}
	switch q.Get("channel") + "/" + q.Get("ts") {
	case "C0ENG/1794120000.000500":
		io.WriteString(w, RepliesEng)
	case "C0ENG/1794133800.000400":
		io.WriteString(w, RepliesMention)
	case "C0RANDOM/1794137000.001300":
		io.WriteString(w, RepliesRandom)
	case "C0REVIEW/1794136200.001100":
		io.WriteString(w, RepliesReview)
	default:
		io.WriteString(w, `{"ok":true,"messages":[]}`)
	}
}

var (
	knownOnce sync.Once
	knownMsgs map[string]string
)

// known is every fixture message by "<channel>/<ts>", as conversations.replies returns one.
func known() map[string]string {
	knownOnce.Do(func() {
		knownMsgs = map[string]string{}
		type msg struct {
			Channel struct {
				ID string `json:"id"`
			} `json:"channel"`
			User     string `json:"user,omitempty"`
			BotID    string `json:"bot_id,omitempty"`
			Text     string `json:"text"`
			TS       string `json:"ts"`
			ThreadTS string `json:"thread_ts,omitempty"`
		}
		add := func(channel string, m msg) {
			if channel == "" {
				channel = m.Channel.ID
			}
			out, _ := json.Marshal(map[string]string{"type": "message", "user": m.User, "bot_id": m.BotID, "text": m.Text, "ts": m.TS, "thread_ts": m.ThreadTS})
			knownMsgs[channel+"/"+m.TS] = string(out)
		}
		for _, page := range []string{SearchDMPage1, SearchDMPage2, SearchMentions, SearchOwn} {
			var r struct {
				Messages struct {
					Matches []msg `json:"matches"`
				} `json:"messages"`
			}
			_ = json.Unmarshal([]byte(page), &r)
			for _, m := range r.Messages.Matches {
				add("", m)
			}
		}
		for channel, list := range map[string]string{"C0ENG": RepliesEng, "C0ENG ": RepliesMention, "G0GROUP": HistoryGroup,
			"C0RANDOM": RepliesRandom, "C0REVIEW": HistoryReview} {
			var r struct {
				Messages []msg `json:"messages"`
			}
			_ = json.Unmarshal([]byte(list), &r)
			for _, m := range r.Messages {
				add(strings.TrimSpace(channel), m)
			}
		}
	})
	return knownMsgs
}

// Messages. 2026-11-08: 1794132000 is 10:00 UTC, 1794139200 is 12:00.
const (
	// One-to-one direct messages, newest first, over two pages. D0DANA: Dana asks twice, then Winton answers at the
	// top level. D0BOT is an app. The last match on page 2 is older than any window, so paging stops there.
	SearchDMPage1 = `{"ok":true,"query":"is:dm","messages":{"paging":{"count":100,"total":4,"page":1,"pages":2},"matches":[
  {"type":"message","channel":{"id":"D0DANA","is_im":true,"is_mpim":false},"user":"U0WINTON","username":"winton","ts":"1794133200.000300","text":"On it","permalink":"https://acme.slack.com/archives/D0DANA/p1794133200000300"},
  {"type":"message","channel":{"id":"D0DANA","is_im":true,"is_mpim":false},"user":"U0DANA","username":"dana","ts":"1794132600.000200","text":"also the budget numbers","permalink":"https://acme.slack.com/archives/D0DANA/p1794132600000200"},
  {"type":"message","channel":{"id":"D0DANA","is_im":true,"is_mpim":false},"user":"U0DANA","username":"dana","ts":"1794132000.000100","text":"Can you check the deploy?","permalink":"https://acme.slack.com/archives/D0DANA/p1794132000000100"}
]}}`

	SearchDMPage2 = `{"ok":true,"query":"is:dm","messages":{"paging":{"count":100,"total":4,"page":2,"pages":2},"matches":[
  {"type":"message","channel":{"id":"D0BOT","is_im":true,"is_mpim":false},"user":"","bot_id":"B0JIRA","ts":"1794131000.000050","text":"Ticket updated https://acme.atlassian.net/browse/ENG-1","permalink":"https://acme.slack.com/archives/D0BOT/p1794131000000050"},
  {"type":"message","channel":{"id":"D0DANA","is_im":true,"is_mpim":false},"user":"U0DANA","username":"dana","ts":"1793400000.000001","text":"old","permalink":"https://acme.slack.com/archives/D0DANA/p1793400000000001"}
]}}`

	// Mentions: one in a group direct message, one top-level in a public channel, one in a private channel.
	SearchMentions = `{"ok":true,"messages":{"paging":{"count":100,"total":3,"page":1,"pages":1},"matches":[
  {"type":"message","channel":{"id":"C0SECRET","is_im":false,"is_mpim":false,"is_private":true},"user":"U0HR","ts":"1794134000.000450","text":"<@U0WINTON> the salary numbers https://github.com/acme/hr/pull/3","permalink":"https://acme.slack.com/archives/C0SECRET/p1794134000000450"},
  {"type":"message","channel":{"id":"G0GROUP","is_im":false,"is_mpim":true},"user":"U0DANA","ts":"1794136800.001200","text":"<@U0WINTON> thoughts on the offsite?","permalink":"https://acme.slack.com/archives/G0GROUP/p1794136800001200"},
  {"type":"message","channel":{"id":"C0ENG","is_im":false,"is_mpim":false,"is_private":false},"user":"U0BOB","ts":"1794133800.000400","text":"<@U0WINTON> can you look at the alerts?","permalink":"https://acme.slack.com/archives/C0ENG/p1794133800000400"}
]}}`

	// Winton's own messages: top-level in a channel that is not configured (no item; the thread is followed), in the
	// configured channel, in Dana's direct message, and a reply in a C0ENG thread, whose thread_ts is only in the
	// permalink.
	SearchOwn = `{"ok":true,"messages":{"paging":{"count":100,"total":4,"page":1,"pages":1},"matches":[
  {"type":"message","channel":{"id":"C0RANDOM","is_im":false,"is_mpim":false,"is_private":false},"user":"U0WINTON","ts":"1794137000.001300","text":"hello all","permalink":"https://acme.slack.com/archives/C0RANDOM/p1794137000001300"},
  {"type":"message","channel":{"id":"C0REVIEW","is_im":false,"is_mpim":false,"is_private":false},"user":"U0WINTON","ts":"1794136200.001100","text":"Merged the fix","permalink":"https://acme.slack.com/archives/C0REVIEW/p1794136200001100"},
  {"type":"message","channel":{"id":"D0DANA","is_im":true,"is_mpim":false},"user":"U0WINTON","ts":"1794133200.000300","text":"On it","permalink":"https://acme.slack.com/archives/D0DANA/p1794133200000300"},
  {"type":"message","channel":{"id":"C0ENG","is_im":false,"is_mpim":false,"is_private":false},"user":"U0WINTON","ts":"1794130000.000600","text":"I can take this","permalink":"https://acme.slack.com/archives/C0ENG/p1794130000000600?thread_ts=1794120000.000500&cid=C0ENG"}
]}}`

	// The C0ENG thread Carol started: her reply before Winton joined is not an item; Bob's after it is.
	RepliesEng = `{"ok":true,"has_more":false,"messages":[
  {"type":"message","user":"U0CAROL","text":"Who can take the alerts rotation?","ts":"1794120000.000500","thread_ts":"1794120000.000500","reply_count":3},
  {"type":"message","user":"U0CAROL","text":"bump","ts":"1794125000.000800","thread_ts":"1794120000.000500"},
  {"type":"message","user":"U0WINTON","text":"I can take this","ts":"1794130000.000600","thread_ts":"1794120000.000500"},
  {"type":"message","user":"U0BOB","text":"Thanks, also see https://github.com/acme/app/pull/7","ts":"1794134400.000700","thread_ts":"1794120000.000500"}
],"response_metadata":{"next_cursor":""}}`

	// Bob's mention in C0ENG started a thread; Carol answered it.
	RepliesMention = `{"ok":true,"messages":[
  {"type":"message","user":"U0BOB","text":"<@U0WINTON> can you look at the alerts?","ts":"1794133800.000400","thread_ts":"1794133800.000400"},
  {"type":"message","user":"U0CAROL","text":"I'll handle it","ts":"1794134500.000750","thread_ts":"1794133800.000400"}
]}`

	// The group direct message Winton is in: Dana's mention (also found by search), Carol's message, Winton's answer.
	HistoryGroup = `{"ok":true,"messages":[
  {"type":"message","user":"U0WINTON","text":"sure","ts":"1794137600.001500"},
  {"type":"message","user":"U0CAROL","text":"Lunch at noon?","ts":"1794137500.001400"},
  {"type":"message","user":"U0DANA","text":"<@U0WINTON> thoughts on the offsite?","ts":"1794136800.001200"}
]}`

	// UserGroups: Winton is in oncall, not in design.
	UserGroups = `{"ok":true,"usergroups":[{"id":"S0ONCALL","users":["U0WINTON","U0BOB"]},{"id":"S0DESIGN","users":["U0ERIN"]}]}`

	RepliesRandom = `{"ok":true,"has_more":false,"messages":[
  {"type":"message","user":"U0WINTON","text":"hello all","ts":"1794137000.001300"}
]}`

	RepliesReview = `{"ok":true,"has_more":false,"messages":[
  {"type":"message","user":"U0WINTON","text":"Merged the fix","ts":"1794136200.001100"}
]}`

	// The configured channel, newest first: Winton's own message, a mention of a user group he is not in, one he is in,
	// an @here, chatter, a join, and Erin's review request.
	HistoryReview = `{"ok":true,"has_more":false,"messages":[
  {"type":"message","user":"U0WINTON","text":"Merged the fix","ts":"1794136200.001100"},
  {"type":"message","user":"U0BOB","text":"<!subteam^S0DESIGN|@design> mockups are up","ts":"1794135900.001070"},
  {"type":"message","user":"U0BOB","text":"<!subteam^S0ONCALL|@oncall> the pager is noisy","ts":"1794135800.001060"},
  {"type":"message","user":"U0FRANK","text":"<!here> deploy freeze at five","ts":"1794135700.001050"},
  {"type":"message","user":"U0FRANK","text":"lunch anyone? I can look for a table","ts":"1794135600.001000"},
  {"type":"message","subtype":"channel_join","user":"U0NEW","text":"<@U0NEW> has joined the channel","ts":"1794135300.000950"},
  {"type":"message","user":"U0ERIN","text":"PTAL <https://github.com/acme/app/pull/42|acme/app#42>, needs a review before the release","ts":"1794135000.000900"}
],"response_metadata":{"next_cursor":""}}`
)
