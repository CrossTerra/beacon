package actionexec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
)

const (
	fixtureTeam    = "T0WORKSPACE"
	fixtureUser    = "U0PERSON"
	fixtureChannel = "C0GENERAL"
	fixtureRoot    = "1799999800.000001"
	fixturePosted  = "1800000000.000200"
	fixtureDraft   = "*Follow up* on <https://example.test|docs> for <@U0OTHER|Not their name>\nCafe\u0301 <!here>\t👍"
	slackReadToken = "xoxp-fixture-read"
	slackPostToken = "xoxp-fixture-write"
)

type slackCall struct {
	verb, method string
	write        bool
	query        url.Values
	deadline     time.Duration
}

type fakeSlack struct {
	t                                       *testing.T
	calls                                   []slackCall
	posts                                   []slackPostBody
	readAuth, writeAuth, conversation, root approval.Object
	readScopes, writeScopes                 string
	members                                 []string
	memberPage                              func(string) ([]string, string)
	names                                   map[string]string
	namesRefused, noRoot                    bool
	mode, needed                            string
	pages                                   func(url.Values, []approval.Object) approval.Object
	echo                                    approval.Object
	hide                                    bool
	rateMethod                              slackMethod
	rateBackOnly                            bool
	retryAfter                              string
	redirect                                bool
	hook                                    func(*http.Request)
	interrupt                               func(*http.Request) error
}

func (f *fakeSlack) handle(r *http.Request) (*http.Response, error) {
	if f.hook != nil {
		f.hook(r)
	}
	if f.interrupt != nil {
		if err := f.interrupt(r); err != nil {
			return nil, err
		}
	}
	if r.URL.Scheme+"://"+r.URL.Host != slackOrigin || !strings.HasPrefix(r.URL.Path, "/api/") {
		f.t.Error("contacted a host other than the configured Slack API")
		return jsonReply(500, nil), nil
	}
	auth := r.Header.Get("Authorization")
	write := auth == "Bearer "+slackPostToken
	m := slackMethod(strings.TrimPrefix(r.URL.Path, "/api/"))
	q := r.URL.Query()
	if !write && auth != "Bearer "+slackReadToken && auth != "Bearer xoxp-fixture-other" ||
		m != slackPost && r.Method != "GET" || m == slackPost && (r.Method != "POST" || !write || r.GetBody != nil) {
		f.t.Error("wrong Slack credential, HTTP method, or replayable write")
	}
	deadline, ok := r.Context().Deadline()
	if !ok {
		f.t.Error("Slack request had no deadline")
	}
	f.calls = append(f.calls, slackCall{r.Method, string(m), write, q, time.Until(deadline)})
	if m == f.rateMethod && (!f.rateBackOnly || q.Get("oldest") != "") {
		res := jsonReply(429, approval.Object{"ok": false, "error": "ratelimited"})
		res.Header.Set("Retry-After", f.retryAfter)
		return res, nil
	}
	slackOK := func(fields approval.Object) (*http.Response, error) {
		fields["ok"] = true
		return jsonReply(200, fields), nil
	}
	slackError := func(code string) (*http.Response, error) {
		return jsonReply(200, approval.Object{"ok": false, "error": code, "needed": f.needed, "provided": "not-in-the-result"}), nil
	}
	switch m {
	case slackAuth:
		if f.redirect {
			res := jsonReply(302, nil)
			res.Header.Set("Location", "https://slack.com/api/users.info")
			return res, nil
		}
		id := approval.Object{"ok": true, "team_id": fixtureTeam, "user_id": fixtureUser, "team": "Fixture Workspace", "user": "fixture-user"}
		patch, scopes := f.readAuth, f.readScopes
		if write {
			patch, scopes = f.writeAuth, f.writeScopes
		} else if auth == "Bearer xoxp-fixture-other" {
			id["user_id"] = "U0OTHER"
		}
		maps.Copy(id, patch)
		res := jsonReply(200, id)
		if scopes != "" {
			res.Header.Set("X-OAuth-Scopes", scopes)
		}
		return res, nil
	case slackInfo:
		ch := approval.Object{"id": q.Get("channel"), "name": "general", "is_channel": true,
			"is_private": false, "is_member": true, "context_team_id": fixtureTeam}
		maps.Copy(ch, f.conversation)
		return slackOK(approval.Object{"channel": ch})
	case slackMembers:
		ids, next := f.members, ""
		if f.memberPage != nil {
			ids, next = f.memberPage(q.Get("cursor"))
		}
		return slackOK(approval.Object{"members": ids, "response_metadata": approval.Object{"next_cursor": next}})
	case slackUser:
		id := q.Get("user")
		if f.namesRefused || f.names[id] == "" {
			return slackError("user_not_found")
		}
		return slackOK(approval.Object{"user": approval.Object{"id": id, "name": "nick", "real_name": f.names[id]}})
	case slackReplies, slackHistory:
		if q.Get("oldest") == "" {
			if m != slackReplies || q.Get("limit") != "1" || q.Get("ts") == "" {
				f.t.Error("invalid Slack root lookup")
			}
			root := approval.Object{"type": "message", "ts": q.Get("ts"), "text": "Parent's first line\nNot in the preview"}
			maps.Copy(root, f.root)
			roots := []approval.Object{}
			if !f.noRoot {
				roots = append(roots, root)
			}
			return slackOK(approval.Object{"messages": roots})
		}
		if write || q.Get("limit") != "15" || q.Get("inclusive") != "true" || q.Get("include_all_metadata") != "true" ||
			!slackTS.MatchString(q.Get("oldest")) || !slackTS.MatchString(q.Get("latest")) || m == slackReplies && q.Get("ts") != fixtureRoot {
			f.t.Error("Slack recovery was not a bounded read with the unattended credential")
		}
		messages := []approval.Object{}
		if !f.hide {
			for _, post := range f.posts {
				msg := approval.Object{"type": "message", "ts": fixturePosted, "channel": post.Channel,
					"user": fixtureUser, "text": post.Text, "client_msg_id": post.ClientID, "metadata": post.Metadata}
				if post.Thread != "" {
					msg["thread_ts"] = post.Thread
				}
				maps.Copy(msg, f.echo)
				messages = append(messages, msg)
			}
		}
		if f.pages != nil {
			return slackOK(f.pages(q, messages))
		}
		return slackOK(approval.Object{"messages": messages})
	case slackPost:
		var p slackPostBody
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			f.t.Error("could not decode the fixture post")
			return jsonReply(500, nil), nil
		}
		f.posts = append(f.posts, p)
		if strings.HasPrefix(f.mode, "refuse:") {
			return slackError(strings.TrimPrefix(f.mode, "refuse:"))
		}
		switch f.mode {
		case "lost":
			return nil, io.ErrUnexpectedEOF
		case "unclear":
			return slackOK(approval.Object{"channel": "C0WRONG", "ts": fixturePosted})
		case "5xx":
			return jsonReply(503, nil), nil
		case "wrong_thread":
			return slackOK(approval.Object{"channel": p.Channel, "ts": fixturePosted,
				"message": approval.Object{"thread_ts": "1799999800.000009"}})
		default:
			return slackOK(approval.Object{"channel": p.Channel, "ts": fixturePosted})
		}
	}
	f.t.Errorf("unexpected Slack fixture call: %s %s", r.Method, r.URL.Path)
	return jsonReply(404, nil), nil
}

type slackFixture struct {
	t  *testing.T
	f  *fakeSlack
	h  *postHandler
	p  *Providers
	j  *approval.Journal
	at time.Time
}

func slackTest(t *testing.T) *slackFixture {
	t.Helper()
	s := &slackFixture{t: t, at: time.Unix(1_800_000_000, 0)}
	s.f = &fakeSlack{t: t, readScopes: strings.Join(slackReadScopes, ","), writeScopes: "chat:write," + strings.Join(slackReadScopes, ","),
		members: []string{fixtureUser, "U0OTHER", "U0FOX"}, names: map[string]string{"U0OTHER": "Dana Scully", "U0FOX": "Fox Mulder"}}
	dir := filepath.Join(t.TempDir(), "state")
	var err error
	s.j, err = approval.OpenJournal(dir, s.at, "")
	requireOK(t, err)
	t.Cleanup(func() { s.j.Close() })
	s.configure(dir)
	return s
}

func (s *slackFixture) configure(dir string) {
	s.t.Helper()
	var err error
	s.p, err = New(Settings{Slack: &SlackSettings{Workspace: fixtureTeam, UserID: fixtureUser}}, dir, func() time.Time { return s.at },
		func([]string) http.RoundTripper { return tripFunc(s.f.handle) })
	requireOK(s.t, err)
	s.h = s.p.slack
	s.h.read = func() (keychain.Secret, error) { return secret(slackReadToken), nil }
	s.h.write = func([]byte) (keychain.Secret, error) { return secret(slackPostToken), nil }
}

func requestPost(ch string, ts any) approval.Request {
	return approval.Request{Type: approval.Post, Agent: "fixture", Reason: "Fixture post.", Context: "slack:C0UNTRUSTED:1800000000.000001",
		Params: approval.Object{"workspace": fixtureTeam, "channel": ch, "thread_ts": ts, "text": fixtureDraft}}
}

func (s *slackFixture) propose(r approval.Request, peer string) approval.Proposal {
	s.t.Helper()
	pl, err := s.h.place(s.t.Context(), r, peer)
	requireOK(s.t, err)
	p, err := s.j.AddSlack(r, approval.PeerLabel{}, s.at, pl)
	requireOK(s.t, err)
	return p
}

func (s *slackFixture) start(p approval.Proposal) approval.View {
	s.t.Helper()
	r, err := s.h.Resolve(s.t.Context(), p)
	requireOK(s.t, err)
	f, err := approval.Freeze(p, r.Binding, "")
	requireOK(s.t, err)
	requireOK(s.t, s.j.ApproveAll([]approval.Frozen{f}, []byte("fixture-credential"), nil, s.at))
	v, err := s.j.StartAsserted(p.ID, s.at)
	requireOK(s.t, err)
	return v
}

func (s *slackFixture) run(v approval.View) {
	s.t.Helper()
	write, err := s.h.Unlock(v.Frozen, make([]byte, 32))
	requireOK(s.t, err)
	defer write.Clear()
	requireOK(s.t, s.h.Run(s.t.Context(), s.j, v, write))
}

func (s *slackFixture) view(id string) approval.View {
	s.t.Helper()
	v, _, err := s.j.Get(id, s.at)
	requireOK(s.t, err)
	return v
}

func (s *slackFixture) want(id, status, by, code string) approval.Result {
	s.t.Helper()
	v := s.view(id)
	var r approval.Result
	if v.Outcome != nil {
		r = *v.Outcome
	}
	if v.Status != status || r.ConfirmedBy != by || r.Error != code {
		s.t.Fatalf("got %s/%+v, want %s/%s/%s", v.Status, r, status, by, code)
	}
	if status == "executed" && r.Native != "slack:message:"+v.Params["channel"].(string)+":"+fixturePosted {
		s.t.Fatal("wrong native id")
	}
	return r
}

func (s *slackFixture) restart() {
	s.t.Helper()
	dir := s.h.dir
	requireOK(s.t, s.j.Close())
	j, err := approval.OpenJournal(dir, s.at, "")
	requireOK(s.t, err)
	s.j = j
	s.configure(dir)
}

func TestSlackSettingsVerifyTwoUserTokensAndScopes(t *testing.T) {
	for _, tc := range []struct {
		code  string
		reads int
		setup func(*fakeSlack) (string, string)
	}{
		{"same_credential", 0, func(*fakeSlack) (string, string) { return slackReadToken, slackReadToken }},
		{"not_a_user", 0, func(*fakeSlack) (string, string) { return slackReadToken, "xoxb-fixture-bot" }},
		{"not_a_user", 2, func(f *fakeSlack) (string, string) { f.writeAuth = approval.Object{"bot_id": "B0BOT"}; return "", "" }},
		{"workspace_mismatch", 2, func(f *fakeSlack) (string, string) {
			f.writeAuth = approval.Object{"team_id": "T0OTHER"}
			return "", ""
		}},
		{"account_mismatch", 1, func(f *fakeSlack) (string, string) { f.readAuth = approval.Object{"user_id": "U0OTHER"}; return "", "" }},
		{"missing_scope", 2, func(f *fakeSlack) (string, string) { f.writeScopes = f.readScopes; return "", "" }},
		{"missing_scope", 1, func(f *fakeSlack) (string, string) { f.readScopes = ""; return "", "" }},
		{"read_scope_not_allowed", 1, func(f *fakeSlack) (string, string) { f.readScopes += ",chat:write"; return "", "" }},
	} {
		t.Run(tc.code, func(t *testing.T) {
			s := slackTest(t)
			r, w := tc.setup(s.f)
			if r == "" {
				r, w = slackReadToken, slackPostToken
			}
			read, write := secret(r), secret(w)
			defer read.Clear()
			defer write.Clear()
			err := s.p.Save(t.Context(), "slack", read, write, make([]byte, 32))
			if code, _ := reason(err); code != tc.code || len(s.f.calls) != tc.reads {
				t.Fatalf("settings got %v, %d reads; want %s, %d reads", err, len(s.f.calls), tc.code, tc.reads)
			}
			if _, err := keychain.ReadAction(s.h.dir, s.h.binding); err == nil {
				t.Fatal("saved an unattended credential before verification")
			}
		})
	}
}

func TestSlackIntakeLabelsAreVerifiedCachedAndOptional(t *testing.T) {
	s := slackTest(t)
	ctx := t.Context()
	s.f.conversation = approval.Object{"is_im": true, "user": "U0OTHER"}
	r := requestPost("D0DANA", nil)
	a := s.propose(r, "peer1")
	b := s.propose(r, "peer1")
	resolved, err := s.h.Resolve(ctx, a)
	requireOK(t, err)
	if a.Slack == nil || b.Slack == nil || *a.Slack != *b.Slack || a.Slack.Conversation != "DM with Dana Scully" ||
		resolved.DestinationLabel != a.Slack.Conversation || resolved.AccountLabel != "fixture-user" || resolved.Facts["Visibility"] != "DM" ||
		callCount(s.f.calls, slackAuth) != 2 || callCount(s.f.calls, slackUser) != 1 {
		t.Fatal("wrong Slack names or cached identity reused for window preflight")
	}
	s.at = s.at.Add(601 * time.Second)
	s.f.names["U0OTHER"] = "New name"
	c := s.propose(r, "peer1")
	if c.Slack.Conversation != "DM with New name" || callCount(s.f.calls, slackAuth) != 3 || callCount(s.f.calls, slackUser) != 2 {
		t.Fatal("names or intake identity kept past expiry")
	}
	s.f.conversation["user"] = fixtureUser
	if self := s.propose(requestPost("D0SELF", nil), "peer1"); self.Slack.Conversation != "DM with yourself" || callCount(s.f.calls, slackUser) != 2 {
		t.Fatal("self DM used a name lookup")
	}
	s.f.conversation = approval.Object{"is_mpim": true, "name": "opaque-name", "is_member": nil}
	g := s.propose(requestPost("G0GROUP", nil), "peer1")
	resolved, err = s.h.Resolve(ctx, g)
	requireOK(t, err)
	if g.Slack.Visibility != approval.SlackGroupDM || g.Slack.Conversation != "Group DM with Fox Mulder, New name" ||
		resolved.DestinationLabel != g.Slack.Conversation || callCount(s.f.calls, slackMembers) != 2 {
		t.Fatal("group name or membership was not verified using its members")
	}
	s.f.namesRefused = true
	s.f.conversation = approval.Object{"is_im": true, "user": "U0MISSING"}
	u := s.propose(requestPost("D0GONE", nil), "peer1")
	resolved, err = s.h.Resolve(ctx, u)
	requireOK(t, err)
	if u.Slack.Conversation != "unknown channel D0GONE" || resolved.DestinationLabel != u.Slack.Conversation {
		t.Fatal("failed name lookup hid the destination")
	}
	s.f.conversation = approval.Object{"is_private": true, "name": "secret"}
	if p := s.propose(requestPost("C0PRIVATE", nil), "peer1"); p.Slack.Visibility != approval.SlackPrivate || p.Slack.Conversation != "#secret" {
		t.Fatal("did not display the private channel label")
	}
	s.h.read = func() (keychain.Secret, error) { return secret("xoxp-fixture-other"), nil }
	if _, err := s.h.place(ctx, r, "peer1"); err == nil || err.Error() != "account_mismatch" {
		t.Fatal("used cached identity with a replacement credential", err)
	}
}

func callCount(calls []slackCall, method slackMethod) int {
	n := 0
	for _, c := range calls {
		if c.method == string(method) {
			n++
		}
	}
	return n
}

func TestSlackPreflightAndFinalReadsRefuseChangedDestination(t *testing.T) {
	for _, tc := range []struct {
		code   string
		change func(*fakeSlack)
	}{
		{"channel_archived", func(f *fakeSlack) { f.conversation = approval.Object{"is_archived": true} }},
		{"channel_archived", func(f *fakeSlack) { f.conversation = approval.Object{"is_frozen": true} }},
		{"not_in_channel", func(f *fakeSlack) { f.conversation = approval.Object{"is_member": false} }},
		{"workspace_mismatch", func(f *fakeSlack) { f.conversation = approval.Object{"context_team_id": "T0OTHER"} }},
		{"workspace_mismatch", func(f *fakeSlack) {
			f.readAuth = approval.Object{"team_id": "T0OTHER"}
			f.writeAuth = f.readAuth
		}},
		{"channel_not_found", func(f *fakeSlack) { f.conversation = approval.Object{"id": "C0OTHER"} }},
		{"destination_changed", func(f *fakeSlack) { f.conversation = approval.Object{"is_private": true} }},
		{"thread_not_found", func(f *fakeSlack) { f.noRoot = true }},
		{"thread_not_found", func(f *fakeSlack) { f.root = approval.Object{"thread_ts": "1799999700.000001"} }},
		{"thread_not_found", func(f *fakeSlack) { f.root = approval.Object{"subtype": "message_deleted"} }},
		{"account_mismatch", func(f *fakeSlack) {
			f.readAuth = approval.Object{"user_id": "U0OTHER"}
			f.writeAuth = f.readAuth
		}},
	} {
		t.Run(tc.code, func(t *testing.T) {
			s := slackTest(t)
			p := s.propose(requestPost(fixtureChannel, fixtureRoot), "")
			v := s.start(p)
			tc.change(s.f)
			if _, err := s.h.Resolve(t.Context(), p); err == nil || err.Error() != tc.code {
				t.Fatal("should not have allowed freeze", err)
			}
			s.run(v)
			s.want(v.ID, "failed", "pre_dispatch", tc.code)
			if len(s.f.posts) != 0 {
				t.Fatal("wrote despite a changed destination")
			}
		})
	}
}

func TestSlackIntakeAndReadsAreBounded(t *testing.T) {
	s := slackTest(t)
	r := requestPost(fixtureChannel, fixtureRoot)
	ctx := t.Context()
	r.Params["workspace"] = "T0OTHER"
	if _, err := s.h.place(ctx, r, ""); err == nil || err.Error() != "workspace_mismatch" || len(s.f.calls) != 0 {
		t.Fatal("proposer changed the configured workspace", err)
	}
	r.Params["workspace"] = fixtureTeam
	s.f.conversation = approval.Object{"is_private": nil}
	if _, err := s.h.place(ctx, r, ""); err == nil || err.Error() != "visibility_unknown" {
		t.Fatal("visibility had to be explicit", err)
	}
	s.f.conversation = nil
	s.h.intakePerPeer = 2
	p := s.propose(r, "first-peer")
	_ = s.propose(r, "first-peer")
	if _, err := s.h.place(ctx, r, "first-peer"); err == nil || err.Error() != "busy" {
		t.Fatal("per-peer budget was not enforced", err)
	}
	_ = s.propose(r, "other-peer")
	s.h.preflight = newBudget(3, 2)
	resolved, err := s.h.Resolve(ctx, p)
	requireOK(t, err)
	if resolved.Facts["Thread root preview"] != "Parent's first line" {
		t.Fatal("did not shape root preview")
	}
	if _, err := s.h.Resolve(ctx, p); err == nil || err.Error() != "busy" {
		t.Fatal("window budget counted incorrectly", err)
	}
	s.at = s.at.Add(time.Hour)
	s.f.redirect = true
	n := len(s.f.calls)
	if _, err := s.h.place(ctx, r, "first-peer"); err == nil || len(s.f.calls) != n+1 || s.f.calls[n].method != string(slackAuth) {
		t.Fatal("Slack redirect followed", err)
	}
	s.f.redirect = false
	r.Params["text"] = strings.Repeat("🙂", 2001)
	p = s.propose(r, "other-peer")
	if _, err := s.h.Resolve(ctx, p); err == nil || err.Error() != "message_too_long" || len(s.f.posts) != 0 {
		t.Fatal("oversize draft offered for approval", err)
	}
}

func TestSlackMembershipPagesAreBoundedAndMetered(t *testing.T) {
	s := slackTest(t)
	s.f.conversation = approval.Object{"is_member": nil}
	s.f.memberPage = func(cursor string) ([]string, string) {
		if cursor == "" {
			return []string{"U0OTHER"}, "next=2"
		}
		return []string{fixtureUser}, ""
	}
	p := s.propose(requestPost(fixtureChannel, nil), "")
	v := s.start(p)
	s.run(v)
	s.want(v.ID, "executed", "reply", "")
	if callCount(s.f.calls, slackMembers) != 4 || s.f.posts[0].Thread != "" {
		t.Fatal("bad membership pagination or sent a thread on a top-level post")
	}
	s.f.memberPage = func(string) ([]string, string) { return []string{"U0OTHER"}, "same-cursor" }
	n := len(s.f.calls)
	if _, err := s.h.Resolve(t.Context(), p); err == nil || len(s.f.calls) != n+4 {
		t.Fatal("a repeated membership page was trusted", err)
	}
}

func TestSlackRepliesAndLostReplyAreOneShot(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		by       string
		drop     string
		autoRead bool
	}{
		{"", "reply", "", false},
		{"lost", "read_back", "client_msg_id", true},
		{"lost", "read_back", "metadata", true},
		{"5xx", "read_back", "", true},
		{"unclear", "read_back", "", true},
		{"wrong_thread", "read_back", "", true},
	} {
		t.Run(tc.mode+tc.drop, func(t *testing.T) {
			s := slackTest(t)
			v := s.start(s.propose(requestPost(fixtureChannel, fixtureRoot), ""))
			s.f.mode = tc.mode
			if tc.drop != "" {
				s.f.echo = approval.Object{tc.drop: nil}
			}
			s.f.pages = func(q url.Values, posts []approval.Object) approval.Object {
				if q.Get("cursor") == "" {
					return approval.Object{"messages": []approval.Object{}, "has_more": true, "response_metadata": approval.Object{"next_cursor": "page-2=="}}
				}
				if q.Get("cursor") != "page-2==" {
					t.Error("cursor did not make it through typed query")
				}
				return approval.Object{"messages": posts}
			}
			before := len(s.f.calls)
			s.run(v)
			s.want(v.ID, "executed", tc.by, "")
			if len(s.f.posts) != 1 {
				t.Fatalf("want one Slack POST, got %d", len(s.f.posts))
			}
			p := s.f.posts[0]
			if !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(v.SlackOperation) || p.ClientID != v.SlackOperation || p.Channel != fixtureChannel ||
				p.Thread != fixtureRoot || p.Text != fixtureDraft || p.Metadata.Type != "beacon_post" ||
				!maps.Equal(p.Metadata.Payload, map[string]string{"op": v.SlackOperation}) || !p.Mrkdwn || p.UnfurlLinks || p.UnfurlMedia || p.Parse != "none" {
				t.Fatal("POST did not use the exact draft and durable UUID")
			}
			calls := s.f.calls[before:]
			if len(calls) < 4 || !calls[0].write || !calls[1].write || !calls[2].write || calls[3].verb != "POST" {
				t.Fatal("did not re-read with the write credential before making the single POST")
			}
			if tc.autoRead != (len(calls) == 8 && calls[7].query.Get("cursor") == "page-2==") {
				t.Fatal("lost reply was not read back through all pages")
			}
			if err := s.h.Run(t.Context(), s.j, v, secret(slackPostToken)); !errors.Is(err, approval.ErrTransition) {
				t.Fatal("approval could run twice", err)
			}
		})
	}
}

func TestSlackDefiniteRefusalsDoNotSearchOrResend(t *testing.T) {
	s := slackTest(t)
	for _, tc := range []struct{ slack, code string }{
		{"is_archived", "channel_archived"}, {"not_in_channel", "not_in_channel"}, {"missing_scope", "missing_scope"},
		{"restricted_action", "slack_refused"}, {"msg_too_long", "message_too_long"}, {"message_limit_exceeded", "slack_refused"},
		{"invalid_blocks", "slack_refused"}, {"too_many_attachments", "slack_refused"}, {"metadata_must_be_sent_from_app", "metadata_refused"},
	} {
		v := s.start(s.propose(requestPost(fixtureChannel, fixtureRoot), ""))
		s.f.mode, s.f.needed = "refuse:"+tc.slack, "chat:write"
		s.run(v)
		r := s.want(v.ID, "failed", "reply", tc.code)
		if tc.code == "missing_scope" && r.Text != "The token lacks the Slack scope chat:write." || strings.Contains(r.Text, "not-in-the-result") {
			t.Fatal("scope diagnostic was missing or unsafe")
		}
		requireOK(t, s.h.Reconcile(t.Context(), s.j, v.ID))
	}
	if len(s.f.posts) != 9 || slices.ContainsFunc(s.f.calls, func(c slackCall) bool { return c.query.Get("oldest") != "" }) {
		t.Fatal("a definite refusal was retried or reconciled")
	}
	for _, code := range []string{"internal_error", "fatal_error", "request_timeout", "something_new"} {
		if slackRejection(slackEnvelope{Error: code}) != nil {
			t.Fatal("mistook an ambiguous reply for a definite failure")
		}
	}
	_, detail := reason(slackRejection(slackEnvelope{Error: "missing_scope", Needed: "xoxp-fixture-do-not-display"}))
	if strings.Contains(detail, "fixture") {
		t.Fatal("echoed an unrecognized scope")
	}
}

func TestSlackRetryAfterOnlyAllowsNewWritesAfterDeadline(t *testing.T) {
	s := slackTest(t)
	r := requestPost(fixtureChannel, nil)
	first := s.start(s.propose(r, ""))
	second := s.start(s.propose(r, ""))
	s.f.rateMethod, s.f.retryAfter = slackPost, "17"
	s.run(first)
	out := s.want(first.ID, "failed", "reply", "rate_limited")
	if out.Text != "Slack is rate limiting requests. Try again in 17 seconds." {
		t.Fatal("did not use Slack's Retry-After")
	}
	s.f.rateMethod = ""
	s.run(second)
	s.want(second.ID, "failed", "pre_dispatch", "rate_limited")
	third := s.propose(r, "") // the post cooldown doesn't apply to intake
	s.at = s.at.Add(16 * time.Second)
	_, err := s.h.Resolve(t.Context(), third)
	if code, detail := reason(err); code != "rate_limited" || detail != "Slack is rate limiting requests. Try again in 1 seconds." {
		t.Fatal("preflight stopped honoring the cooldown early", err)
	}
	s.at = s.at.Add(time.Second)
	s.run(s.start(third))
	s.want(third.ID, "executed", "reply", "")
	write := secret(slackPostToken)
	defer write.Clear()
	if err := s.h.Run(t.Context(), s.j, first, write); !errors.Is(err, approval.ErrTransition) {
		t.Fatal("could retry an old write", err)
	}
	if callCount(s.f.calls, slackPost) != 2 || slices.ContainsFunc(s.f.calls, func(c slackCall) bool { return c.query.Get("oldest") != "" }) {
		t.Fatal("a rate-limited post was retried or searched for")
	}
}

func TestSlackRecoveryUsesSeparateReadBudgetAndDeadlines(t *testing.T) {
	s := slackTest(t)
	s.h.recovery = newBudget(2, 1)
	v := s.start(s.propose(requestPost(fixtureChannel, fixtureRoot), ""))
	s.f.mode = "lost"
	start := len(s.f.calls)
	s.run(v)
	s.want(v.ID, "outcome_unknown", "", "")
	if len(s.f.calls[start:]) != 6 || s.f.calls[start+4].method != string(slackAuth) || s.f.calls[start+5].method != string(slackInfo) {
		t.Fatal("expected three final reads, one write, and just two recovery reads")
	}
	// Only the first two calls were intake. Every later phase gets its own, longer deadline.
	for i, c := range s.f.calls {
		if i < 2 && c.deadline > slackIntakeDeadline || i >= 2 && c.deadline <= slackIntakeDeadline {
			t.Fatalf("unexpected deadline for %s in phase at call %d", c.method, i)
		}
	}
	if _, err := s.h.Resolve(t.Context(), s.propose(requestPost(fixtureChannel, fixtureRoot), "")); err != nil {
		t.Fatal("recovery took the window's reads", err)
	}
}

func TestSlackRecoveryBacksOffAndKeepsRateLimitsSeparate(t *testing.T) {
	s := slackTest(t)
	v := s.start(s.propose(requestPost(fixtureChannel, fixtureRoot), ""))
	s.f.mode, s.f.hide = "unclear", true
	s.run(v)
	due := func(want bool) {
		t.Helper()
		if s.h.Due(s.view(v.ID)) != want {
			t.Fatalf("read-back due = %t, want %t", !want, want)
		}
	}
	reconcile := func() { requireOK(t, s.h.Reconcile(t.Context(), s.j, v.ID)) }
	for _, delay := range []time.Duration{15, 30, 60, 120} {
		s.at = s.at.Add(delay*time.Second - time.Second)
		due(false)
		s.at = s.at.Add(time.Second)
		due(true)
		reconcile()
	}
	s.f.rateMethod, s.f.rateBackOnly, s.f.retryAfter = slackReplies, true, "600"
	reconcile()
	s.f.rateMethod = ""
	before := callCount(s.f.calls, slackReplies)
	reconcile()
	if callCount(s.f.calls, slackReplies) != before {
		t.Fatal("Read again bypassed the read-back cooldown")
	}
	s.start(s.propose(requestPost(fixtureChannel, fixtureRoot), "")) // even this same method is available for preflight
	s.at = v.StartedAt.Add(slackSearchWindow + time.Minute + time.Second)
	due(false)
	s.restart()
	due(true)
	reconcile()
	s.at = s.at.Add(time.Hour)
	due(false)
	s.f.hide = false
	reconcile() // manual read-back is still possible after automatic backoff ends
	s.want(v.ID, "executed", "read_back", "")
	if callCount(s.f.calls, slackPost) != 1 {
		t.Fatal("recovery sent another post")
	}
}

func TestSlackRecoveryRequiresUniqueExactEvidence(t *testing.T) {
	otherOp := approval.Object{"event_type": "beacon_post", "event_payload": approval.Object{"op": "different-operation"}}
	for _, tc := range []struct {
		name string
		echo approval.Object
	}{
		{"text only", approval.Object{"client_msg_id": nil, "metadata": nil}},
		{"other operation", approval.Object{"client_msg_id": nil, "metadata": otherOp}},
		{"conflicting ids", approval.Object{"metadata": otherOp}},
		{"normalized text", approval.Object{"text": strings.Replace(fixtureDraft, "e\u0301", "é", 1)}},
		{"wrong author", approval.Object{"user": "U0OTHER"}},
		{"bot", approval.Object{"bot_id": "B0APP"}},
		{"wrong conversation", approval.Object{"channel": "C0OTHER"}},
		{"wrong thread", approval.Object{"thread_ts": fixtureRoot}},
		{"out of window", approval.Object{"ts": "1800001000.000200"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := slackTest(t)
			v := s.start(s.propose(requestPost(fixtureChannel, nil), ""))
			s.f.mode, s.f.echo = "lost", tc.echo
			s.run(v)
			s.want(v.ID, "outcome_unknown", "", "")
			if callCount(s.f.calls, slackPost) != 1 || s.f.calls[len(s.f.calls)-1].method != string(slackHistory) {
				t.Fatal("did not limit recovery to history for a top-level post")
			}
		})
	}
	t.Run("pagination and restart", func(t *testing.T) {
		s := slackTest(t)
		v := s.start(s.propose(requestPost(fixtureChannel, nil), ""))
		s.f.mode = "lost"
		s.f.pages = func(_ url.Values, posts []approval.Object) approval.Object {
			return approval.Object{"messages": posts, "has_more": true}
		}
		s.run(v)
		s.want(v.ID, "outcome_unknown", "", "")
		s.f.pages = func(q url.Values, posts []approval.Object) approval.Object {
			if q.Get("cursor") == "" {
				return approval.Object{"messages": posts, "response_metadata": approval.Object{"next_cursor": "another"}}
			}
			dedup := maps.Clone(posts[0])
			dedup["ts"] = "1800000000.000201"
			return approval.Object{"messages": []approval.Object{dedup}}
		}
		requireOK(t, s.h.Reconcile(t.Context(), s.j, v.ID))
		s.want(v.ID, "outcome_unknown", "", "")
		s.at = s.at.Add(time.Hour)
		s.restart()
		s.f.pages = nil
		requireOK(t, s.h.Reconcile(t.Context(), s.j, v.ID))
		s.want(v.ID, "executed", "read_back", "")
		last := s.f.calls[len(s.f.calls)-1]
		if s.view(v.ID).SlackOperation != v.SlackOperation || callCount(s.f.calls, slackPost) != 1 ||
			last.query.Get("oldest") != "1799999940.000000" || last.query.Get("latest") != "1800000300.000000" {
			t.Fatal("recovery forgot the original UUID or moved the search window after restart")
		}
	})
}

func TestSlackRecoveryOfDurableStartNeverDispatches(t *testing.T) {
	s := slackTest(t)
	v := s.start(s.propose(requestPost(fixtureChannel, fixtureRoot), ""))
	if !s.j.ClaimDispatch(v.ID) {
		t.Fatal("could not claim fixture dispatch")
	}
	// Model the provider having accepted a write just before Beacon died without a journaled reply.
	write := secret(slackPostToken)
	defer write.Clear()
	d, err := s.h.snapshot(v)
	requireOK(t, err)
	if s.h.api(write, slackCheck, s.h.final).post(t.Context(), d, fixtureUser, v.SlackOperation).ts != fixturePosted {
		t.Fatal("fixture did not accept the post")
	}
	s.restart()
	unknown := s.view(v.ID)
	if unknown.Status != "outcome_unknown" || unknown.SlackOperation != v.SlackOperation || !s.h.Due(unknown) {
		t.Fatal("start was not recoverable at launch")
	}
	if err := s.h.Run(t.Context(), s.j, v, write); !errors.Is(err, approval.ErrTransition) {
		t.Fatal("old start could dispatch after restart", err)
	}
	requireOK(t, s.h.Reconcile(t.Context(), s.j, v.ID))
	s.want(v.ID, "executed", "read_back", "")
	if callCount(s.f.calls, slackPost) != 1 {
		t.Fatal("write was replayed")
	}
}

func TestSlackAbortBeforeClaimDoesNotLookLikeALostPost(t *testing.T) {
	t.Run("cancelled after final reads", func(t *testing.T) {
		s := slackTest(t)
		v := s.start(s.propose(requestPost(fixtureChannel, fixtureRoot), ""))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s.f.hook = func(r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/conversations.replies") && r.URL.Query().Get("oldest") == "" {
				cancel()
			}
		}
		write, err := s.h.Unlock(v.Frozen, make([]byte, 32))
		requireOK(t, err)
		defer write.Clear()
		requireOK(t, s.h.Run(ctx, s.j, v, write))
		s.want(v.ID, "failed", "pre_dispatch", "stopped")
		if len(s.f.posts) != 0 {
			t.Fatal("cancelled start posted")
		}
	})
	t.Run("stopping before reads", func(t *testing.T) {
		s := slackTest(t)
		v := s.start(s.propose(requestPost(fixtureChannel, fixtureRoot), ""))
		requireOK(t, s.j.Shutdown(s.at))
		write, err := s.h.Unlock(v.Frozen, make([]byte, 32))
		requireOK(t, err)
		defer write.Clear()
		requireOK(t, s.h.Run(t.Context(), s.j, v, write))
		s.want(v.ID, "failed", "pre_dispatch", "stopped")
		if len(s.f.posts) != 0 {
			t.Fatal("stopping start posted")
		}
	})
	t.Run("interrupted final read", func(t *testing.T) {
		s := slackTest(t)
		v := s.start(s.propose(requestPost(fixtureChannel, fixtureRoot), ""))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered := make(chan struct{})
		s.f.interrupt = func(r *http.Request) error {
			if r.Header.Get("Authorization") != "Bearer "+slackPostToken || r.Method != http.MethodGet {
				return nil
			}
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-r.Context().Done()
			return r.Context().Err()
		}
		write, err := s.h.Unlock(v.Frozen, make([]byte, 32))
		requireOK(t, err)
		defer write.Clear()
		done := make(chan error, 1)
		go func() { done <- s.h.Run(ctx, s.j, v, write) }()
		select {
		case <-entered:
			cancel()
		case <-time.After(2 * time.Second):
			t.Fatal("final read did not start")
		}
		select {
		case err := <-done:
			requireOK(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("interrupted read did not finish")
		}
		s.want(v.ID, "failed", "pre_dispatch", "stopped")
		if len(s.f.posts) != 0 {
			t.Fatal("interrupted start posted")
		}
	})
	t.Run("unhealthy journal", func(t *testing.T) {
		s := slackTest(t)
		v := s.start(s.propose(requestPost(fixtureChannel, fixtureRoot), ""))
		dir := s.h.dir
		requireOK(t, s.j.Close())
		write, err := s.h.Unlock(v.Frozen, make([]byte, 32))
		requireOK(t, err)
		defer write.Clear()
		if err := s.h.Run(t.Context(), s.j, v, write); !errors.Is(err, approval.ErrTransition) {
			t.Fatal(err)
		}
		j, err := approval.OpenJournal(dir, s.at, "")
		requireOK(t, err)
		defer j.Close()
		got, _, err := j.Get(v.ID, s.at)
		requireOK(t, err)
		if got.Status != "outcome_unknown" || got.Outcome != nil {
			t.Fatal("unhealthy abort recorded a result or lost the start", got.Status)
		}
	})
}
