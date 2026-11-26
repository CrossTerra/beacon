package actionexec

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
)

var (
	slackID     = regexp.MustCompile(`^[A-Z0-9]{1,32}$`)
	slackUserID = regexp.MustCompile(`^[UW][A-Z0-9]{0,31}$`)
	slackTS     = regexp.MustCompile(`^[0-9]{1,16}\.[0-9]{6}$`)
	scopeSyntax = regexp.MustCompile(`^[a-z][a-z0-9_:-]{0,63}$`)
)

const slackEventType = "beacon_post"

type slackMethod string

const (
	slackAuth    slackMethod = "auth.test"
	slackInfo    slackMethod = "conversations.info"
	slackMembers slackMethod = "conversations.members"
	slackUser    slackMethod = "users.info"
	slackHistory slackMethod = "conversations.history"
	slackReplies slackMethod = "conversations.replies"
	slackPost    slackMethod = "chat.postMessage"
)

type slackPurpose uint8

const (
	slackIntake slackPurpose = iota
	slackCheck               // Settings, window, final reads and post
	slackRecover
)

// A handler is for one workspace. An intake or read-back 429 must not stop the window, and no
// 429 sleeps then retries a write. The slots are just the fixed set of methods and purposes above.
type slackRates struct {
	mu    sync.Mutex
	until map[slackRateKey]time.Time
}

type slackRateKey struct {
	method  slackMethod
	purpose slackPurpose
}

func rateRefusal(seconds int64) approval.Refusal {
	return approval.Refusal{Code: "rate_limited", Detail: "Slack is rate limiting requests. Try again in " +
		strconv.FormatInt(seconds, 10) + " seconds."}
}

func (r *slackRates) check(k slackRateKey, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d := r.until[k].Sub(at); d > 0 {
		return rateRefusal(int64((d + time.Second - 1) / time.Second))
	}
	return nil
}

func (r *slackRates) deferMethod(k slackRateKey, at time.Time, header string) error {
	seconds, err := strconv.ParseInt(strings.TrimSpace(header), 10, 32)
	if err != nil || seconds < 0 {
		seconds = 60
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.until == nil {
		r.until = make(map[slackRateKey]time.Time)
	}
	if until := at.Add(time.Duration(seconds) * time.Second); until.After(r.until[k]) {
		r.until[k] = until
	}
	return rateRefusal(seconds)
}

type slackAPI struct {
	*provider
	rates   *slackRates
	token   keychain.Secret
	reads   *budget
	peer    *budget // non-nil only at socket intake
	purpose slackPurpose
}

func (a slackAPI) read(ctx context.Context, method slackMethod, q url.Values) (answer, error) {
	switch method {
	case slackAuth, slackInfo, slackMembers, slackUser, slackHistory, slackReplies:
	default:
		return answer{}, refusal("slack_unavailable")
	}
	k := slackRateKey{method, a.purpose}
	if err := a.rates.check(k, a.now()); err != nil {
		return answer{}, err
	}
	if a.peer != nil {
		if err := a.peer.take(a.now()); err != nil {
			return answer{}, err
		}
	}
	res, err := a.call(ctx, a.token, a.reads, http.MethodGet, "/api/"+string(method), q, nil)
	if res.status == http.StatusTooManyRequests {
		return res, a.rates.deferMethod(k, a.now(), res.header.Get("Retry-After"))
	}
	if err == refusal("unavailable") {
		return res, refusal("slack_unavailable")
	}
	return res, err
}

func (a slackAPI) canPost() error {
	return a.rates.check(slackRateKey{slackPost, slackCheck}, a.now())
}

type slackEnvelope struct {
	OK     *bool  `json:"ok"`
	Error  string `json:"error"`
	Needed string `json:"needed"`
}

func (a slackAPI) get(ctx context.Context, method slackMethod, q url.Values, out any) (http.Header, error) {
	res, err := a.read(ctx, method, q)
	if err != nil {
		return nil, err
	}
	if res.status == 401 || res.status == 403 {
		return nil, refusal("slack_refused")
	}
	var env slackEnvelope
	if res.decode(&env) != nil || env.OK == nil {
		return nil, refusal("slack_unavailable")
	}
	if !*env.OK {
		if err := slackRejection(env); err != nil {
			return nil, err
		}
		return nil, refusal("slack_unavailable")
	}
	if err := res.decode(out); err != nil {
		return nil, refusal("slack_unavailable")
	}
	return res.header, nil
}

// Slack may return these before a write. New/unknown error strings, internal_error, fatal_error,
// and request_timeout are not evidence that it did nothing.
func slackRejection(env slackEnvelope) error {
	switch env.Error {
	case "is_archived", "channel_is_archived":
		return refusal("channel_archived")
	case "channel_not_found":
		return refusal("channel_not_found")
	case "thread_not_found", "cannot_reply_to_message":
		return refusal("thread_not_found")
	case "not_in_channel":
		return refusal("not_in_channel")
	case "missing_scope":
		var scopes []string
		for _, scope := range strings.Split(env.Needed, ",") {
			scope = strings.TrimSpace(scope)
			if !slices.Contains(slackReadScopes, scope) && scope != "chat:write" && scope != slackMetadataRead {
				return missingSlackScopes(nil)
			}
			if !slices.Contains(scopes, scope) {
				scopes = append(scopes, scope)
			}
		}
		return missingSlackScopes(scopes)
	case "msg_too_long", "msg_blocks_too_long":
		return refusal("message_too_long")
	case "metadata_must_be_sent_from_app", "invalid_metadata_format", "invalid_metadata_schema", "metadata_too_large":
		return approval.Refusal{Code: "metadata_refused", Detail: "Slack refused the operation ID attached as metadata. Nothing was posted."}
	case "invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired", "not_allowed_token_type",
		"access_denied", "no_permission", "ekm_access_denied", "team_access_not_granted", "enterprise_is_restricted",
		"two_factor_setup_required", "org_login_required", "restricted_action", "restricted_action_read_only_channel",
		"restricted_action_non_threadable_channel", "restricted_action_thread_locked", "restricted_action_thread_only_channel",
		"posting_to_channel_denied", "message_limit_exceeded", "no_text", "invalid_blocks", "invalid_blocks_format",
		"too_many_attachments", "invalid_attachments", "too_many_contact_cards", "as_user_not_supported",
		"duplicate_channel_not_found", "duplicate_message_not_found", "invalid_arguments", "invalid_arg_name",
		"invalid_array_arg", "invalid_charset", "invalid_form_data", "invalid_post_type", "missing_post_type",
		"invalid_json", "json_not_object", "deprecated_endpoint", "method_deprecated", "upgrade_required":
		return refusal("slack_refused")
	default:
		return nil
	}
}

var slackReadScopes = []string{"channels:read", "groups:read", "im:read", "mpim:read", "users:read",
	"channels:history", "groups:history", "im:history", "mpim:history"}

const slackMetadataRead = "metadata.message:read"

func missingSlackScopes(scopes []string) error {
	detail := "The token lacks a Slack scope. Check the scopes listed in the setup guide."
	if len(scopes) == 1 {
		detail = "The token lacks the Slack scope " + scopes[0] + "."
	} else if len(scopes) > 1 {
		detail = "The token lacks Slack scopes: " + strings.Join(scopes, ", ") + "."
	}
	return approval.Refusal{Code: "missing_scope", Detail: detail}
}

func checkSlackScopes(header http.Header, write bool) error {
	have := make(map[string]bool)
	for _, line := range header.Values("X-OAuth-Scopes") {
		for _, scope := range strings.Split(line, ",") {
			scope = strings.TrimSpace(scope)
			if !scopeSyntax.MatchString(scope) {
				return missingSlackScopes(nil)
			}
			have[scope] = true
		}
	}
	var missing []string
	if write && !have["chat:write"] {
		missing = append(missing, "chat:write")
	}
	for _, scope := range slackReadScopes {
		if !have[scope] {
			missing = append(missing, scope)
		}
	}
	if len(missing) != 0 {
		return missingSlackScopes(missing)
	}
	if !write {
		for scope := range have {
			// Slack adds identify to every user token; it reads only the user's own identity.
			if scope != slackMetadataRead && scope != "identify" && !slices.Contains(slackReadScopes, scope) {
				return approval.Refusal{Code: "read_scope_not_allowed", Detail: "Use only the listed read scopes for the unattended Slack token."}
			}
		}
	}
	return nil
}

type slackIdentity struct {
	Workspace string  `json:"team_id"`
	UserID    string  `json:"user_id"`
	Team      string  `json:"team"`
	User      string  `json:"user"`
	BotID     *string `json:"bot_id"`
}

func (a slackAPI) identify(ctx context.Context, user string, write bool) (slackIdentity, error) {
	var id slackIdentity
	if !keychain.ActionSlackUserToken(a.token) {
		return id, refusal("not_a_user")
	}
	headers, err := a.get(ctx, slackAuth, nil, &id)
	if err != nil {
		return id, err
	}
	if id.BotID != nil || !slackUserID.MatchString(id.UserID) {
		return id, refusal("not_a_user")
	}
	if id.UserID != user {
		return id, refusal("account_mismatch")
	}
	if id.Workspace != a.binding.Workspace {
		return id, refusal("workspace_mismatch")
	}
	if id.Team == "" || id.User == "" {
		return id, refusal("slack_unavailable")
	}
	return id, checkSlackScopes(headers, write)
}

type slackConversation struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	User     string  `json:"user"`
	Team     *string `json:"context_team_id"`
	Channel  *bool   `json:"is_channel"`
	Group    bool    `json:"is_group"`
	Private  *bool   `json:"is_private"`
	DM       bool    `json:"is_im"`
	GroupDM  bool    `json:"is_mpim"`
	Archived bool    `json:"is_archived"`
	Frozen   bool    `json:"is_frozen"`
	Member   *bool   `json:"is_member"`
}

func (c slackConversation) visibility() approval.SlackVisibility {
	switch {
	case c.DM && c.GroupDM:
		return ""
	case c.DM:
		return approval.SlackDM
	case c.GroupDM:
		return approval.SlackGroupDM
	case c.Group || c.Private != nil && *c.Private:
		return approval.SlackPrivate
	case c.Channel != nil && *c.Channel && c.Private != nil && !*c.Private:
		return approval.SlackPublic
	default:
		return ""
	}
}

func (a slackAPI) conversation(ctx context.Context, channel string) (slackConversation, error) {
	var v struct {
		Channel slackConversation `json:"channel"`
	}
	_, err := a.get(ctx, slackInfo, url.Values{"channel": {channel}}, &v)
	if err != nil {
		return v.Channel, err
	}
	if v.Channel.ID != channel {
		return v.Channel, refusal("channel_not_found")
	}
	if v.Channel.Team != nil && *v.Channel.Team != a.binding.Workspace {
		return v.Channel, refusal("workspace_mismatch")
	}
	return v.Channel, nil
}

type slackCursor struct {
	Next string `json:"next_cursor"`
}

func (c slackCursor) next(seen map[string]bool) (string, error) {
	if len(c.Next) > 2048 || seen[c.Next] {
		return "", refusal("slack_unavailable")
	}
	if c.Next != "" {
		seen[c.Next] = true
	}
	return c.Next, nil
}

func setCursor(q url.Values, cursor string) {
	if cursor != "" {
		q.Set("cursor", cursor)
	}
}

type slackMemberPage struct {
	Members []string    `json:"members"`
	Cursor  slackCursor `json:"response_metadata"`
}

func (a slackAPI) memberPage(ctx context.Context, channel, cursor string) (slackMemberPage, error) {
	var p slackMemberPage
	q := url.Values{"channel": {channel}, "limit": {"200"}}
	setCursor(q, cursor)
	_, err := a.get(ctx, slackMembers, q, &p)
	return p, err
}

func (a slackAPI) requireMember(ctx context.Context, c slackConversation, user string) error {
	if c.Member != nil {
		if *c.Member {
			return nil
		}
		return refusal("not_in_channel")
	}
	var cursor string
	seen := make(map[string]bool)
	for range 3 {
		p, err := a.memberPage(ctx, c.ID, cursor)
		if err != nil {
			return err
		}
		if slices.Contains(p.Members, user) {
			return nil
		}
		cursor, err = p.Cursor.next(seen)
		if err != nil {
			return err
		}
		if cursor == "" {
			return refusal("not_in_channel")
		}
	}
	return refusal("slack_unavailable")
}

func (a slackAPI) userName(ctx context.Context, id string) (string, error) {
	var v struct {
		User struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			RealName string `json:"real_name"`
		} `json:"user"`
	}
	_, err := a.get(ctx, slackUser, url.Values{"user": {id}}, &v)
	if err != nil || v.User.ID != id || v.User.Name == "" {
		return "", refusal("slack_unavailable")
	}
	if name := oneLine(v.User.RealName, 80); name != "" {
		return name, nil
	}
	return oneLine(v.User.Name, 80), nil
}

type slackMessage struct {
	Type     string          `json:"type"`
	Subtype  *string         `json:"subtype"`
	Channel  *string         `json:"channel"`
	User     *string         `json:"user"`
	TS       string          `json:"ts"`
	Thread   *string         `json:"thread_ts"`
	Text     *string         `json:"text"`
	ClientID string          `json:"client_msg_id"`
	Metadata json.RawMessage `json:"metadata"`
}

type slackPage struct {
	Messages []slackMessage `json:"messages"`
	More     bool           `json:"has_more"`
	Cursor   slackCursor    `json:"response_metadata"`
}

func (a slackAPI) messages(ctx context.Context, d slackDraft, q url.Values) (slackPage, error) {
	var p slackPage
	q.Set("channel", d.channel)
	method := slackHistory
	if d.thread != "" {
		method = slackReplies
		q.Set("ts", d.thread)
	}
	_, err := a.get(ctx, method, q, &p)
	return p, err
}

func (a slackAPI) root(ctx context.Context, d slackDraft) (slackMessage, error) {
	p, err := a.messages(ctx, d, url.Values{"limit": {"1"}})
	if err != nil {
		return slackMessage{}, err
	}
	if len(p.Messages) != 0 {
		m := p.Messages[0]
		if m.Type == "message" && m.TS == d.thread && absentOr(m.Thread, d.thread) && absentOr(m.Channel, d.channel) &&
			(m.Subtype == nil || *m.Subtype != "tombstone" && *m.Subtype != "message_deleted") {
			return m, nil
		}
	}
	return slackMessage{}, refusal("thread_not_found")
}

func absentOr(p *string, want string) bool { return p == nil || *p == want }

type slackPostBody struct {
	Channel     string        `json:"channel"`
	Text        string        `json:"text"`
	Thread      string        `json:"thread_ts,omitempty"`
	ClientID    string        `json:"client_msg_id"`
	Metadata    slackMetadata `json:"metadata"`
	Mrkdwn      bool          `json:"mrkdwn"`
	Parse       string        `json:"parse"`
	UnfurlLinks bool          `json:"unfurl_links"`
	UnfurlMedia bool          `json:"unfurl_media"`
}

type slackMetadata struct {
	Type    string            `json:"event_type"`
	Payload map[string]string `json:"event_payload"`
}

type postReply struct {
	ts, ambiguous string
	refused       error
}

func (a slackAPI) post(ctx context.Context, d slackDraft, user, op string) postReply {
	body := slackPostBody{Channel: d.channel, Text: d.text, Thread: d.thread, ClientID: op, Mrkdwn: true, Parse: "none",
		Metadata: slackMetadata{Type: slackEventType, Payload: map[string]string{"op": op}}}
	res, err := a.call(ctx, a.token, nil, http.MethodPost, "/api/"+string(slackPost), nil, body)
	switch {
	case res.status == http.StatusTooManyRequests:
		return postReply{refused: a.rates.deferMethod(slackRateKey{slackPost, slackCheck}, a.now(), res.header.Get("Retry-After"))}
	case res.status == 401 || res.status == 403 || res.status == 404 || res.status == 413:
		return postReply{refused: refusal("slack_refused")}
	case res.status >= 500:
		return postReply{ambiguous: "server_error"}
	case err != nil:
		return postReply{ambiguous: "reply_lost"}
	case res.status < 200 || res.status >= 300 && res.status < 400:
		return postReply{ambiguous: "reply_unclear"}
	}
	var posted struct {
		slackEnvelope
		Channel string          `json:"channel"`
		TS      string          `json:"ts"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(res.body, &posted) == nil && posted.OK != nil {
		if !*posted.OK {
			if err := slackRejection(posted.slackEnvelope); err != nil {
				return postReply{refused: err}
			}
		} else if res.status < 300 && (posted.Channel == "" || posted.Channel == d.channel) && slackTS.MatchString(posted.TS) && posted.TS != d.thread {
			// The receipt acknowledges our one-shot request. The message echo need not have
			// a particular shape, but explicit contradictions still require read-back.
			var echo struct {
				TS      string  `json:"ts"`
				Channel *string `json:"channel"`
				User    *string `json:"user"`
				Thread  *string `json:"thread_ts"`
			}
			_ = json.Unmarshal(posted.Message, &echo)
			if (echo.TS == "" || echo.TS == posted.TS) && absentOr(echo.Channel, d.channel) && absentOr(echo.User, user) &&
				absentOr(echo.Thread, threadOrSelf(d.thread, posted.TS)) {
				return postReply{ts: posted.TS}
			}
		}
	}
	return postReply{ambiguous: "reply_unclear"}
}

func threadOrSelf(thread, ts string) string {
	if thread != "" {
		return thread
	}
	return ts
}
