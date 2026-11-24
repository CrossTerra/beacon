package actionexec

import (
	"context"
	"errors"
	"maps"
	"strings"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
)

type postHandler struct {
	*provider
	user          string
	rates         slackRates
	intake        *budget
	intakePerPeer int
	lookup        slackLookups
	retries       slackRetries
}

type slackDraft struct{ workspace, channel, thread, text string }

func (h *postHandler) draft(params approval.Object) (slackDraft, error) {
	if err := approval.Post.CheckParams(params); err != nil {
		return slackDraft{}, err
	}
	thread, _ := params["thread_ts"].(string)
	d := slackDraft{params["workspace"].(string), params["channel"].(string), thread, params["text"].(string)}
	if d.workspace != h.binding.Workspace {
		return slackDraft{}, refusal("workspace_mismatch")
	}
	return d, nil
}

func (d slackDraft) destination() approval.Object {
	o := approval.Object{"workspace": d.workspace, "channel": d.channel, "thread_ts": nil}
	if d.thread != "" {
		o["thread_ts"] = d.thread
	}
	return o
}

func (d slackDraft) checkLength() error {
	// Slack can split long texts into multiple messages. The approval is for one exact post.
	n := 0
	for _, r := range d.text {
		n++
		if r > 0xffff {
			n++
		}
		if n > 4000 {
			return approval.Refusal{Code: "message_too_long", Detail: "Use at most 4,000 UTF-16 code units."}
		}
	}
	return nil
}

func (h *postHandler) api(token keychain.Secret, purpose slackPurpose, meter *budget) slackAPI {
	return slackAPI{provider: h.provider, rates: &h.rates, token: token, purpose: purpose, reads: meter}
}

func (h *postHandler) save(ctx context.Context, read, write keychain.Secret, key []byte) error {
	if !keychain.ActionSlackUserToken(read) || !keychain.ActionSlackUserToken(write) {
		return refusal("not_a_user")
	}
	leave, err := h.preflight.enter()
	if err != nil {
		return err
	}
	defer leave()
	ctx, cancel := context.WithTimeout(ctx, readDeadline)
	defer cancel()
	if _, err := h.api(read, slackCheck, h.preflight).identify(ctx, h.user, false); err != nil {
		return err
	}
	if _, err := h.api(write, slackCheck, h.preflight).identify(ctx, h.user, true); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return refusal("slack_unavailable")
	}
	return h.store(read, write, key)
}

func (h *postHandler) Unlock(f *approval.Frozen, key []byte) (keychain.Secret, error) {
	token, err := h.provider.Unlock(f, key)
	if err != nil {
		return keychain.Secret{}, err
	}
	if !keychain.ActionSlackUserToken(token) {
		token.Clear()
		return keychain.Secret{}, refusal("not_a_user")
	}
	return token, nil
}

func (h *postHandler) placed(p *approval.SlackPlacement) bool {
	return p != nil && p.Account == h.binding.Account && p.Visibility != ""
}

func (h *postHandler) Resolve(ctx context.Context, p approval.Proposal) (approval.Resolved, error) {
	d, err := h.draft(p.Params)
	if err != nil {
		return approval.Resolved{}, err
	}
	if err := d.checkLength(); err != nil {
		return approval.Resolved{}, err
	}
	if p.Type != approval.Post || !h.placed(p.Slack) {
		return approval.Resolved{}, approval.Refusal{Code: "destination_changed", Detail: "Submit a new proposal so Beacon can verify the destination at intake."}
	}
	leave, err := h.preflight.enter()
	if err != nil {
		return approval.Resolved{}, err
	}
	defer leave()
	ctx, cancel := context.WithTimeout(ctx, readDeadline)
	defer cancel()
	read, err := h.reader()
	if err != nil {
		return approval.Resolved{}, err
	}
	defer read.Clear()
	a := h.api(read, slackCheck, h.preflight)
	id, ch, root, err := h.check(ctx, a, d, p.Slack.Visibility, false)
	if err != nil {
		return approval.Resolved{}, err
	}
	label := h.conversationName(ctx, a, ch, p.Slack.Visibility)
	facts := approval.Object{"Workspace": workspaceName(id), "Visibility": p.Slack.Visibility.Label()}
	if d.thread != "" {
		first := ""
		if root.Text != nil {
			first = *root.Text
			if i := strings.IndexAny(first, "\r\n\v\f\u0085\u2028\u2029"); i >= 0 {
				first = first[:i]
			}
		}
		if first = oneLine(first, 80); first == "" {
			first = "(no text on first line)"
		}
		facts["Thread root preview"] = first
	} else {
		facts["Thread"] = "New top-level message"
	}
	return approval.Resolved{Binding: approval.Binding{Origin: h.binding.Origin, Account: h.binding.Account, Destination: d.destination()},
		AccountLabel: oneLine(id.User, 80), DestinationLabel: label, Facts: facts}, nil
}

func (h *postHandler) check(ctx context.Context, a slackAPI, d slackDraft, want approval.SlackVisibility, write bool) (slackIdentity, slackConversation, slackMessage, error) {
	var ch slackConversation
	var root slackMessage
	if err := a.canPost(); err != nil {
		return slackIdentity{}, ch, root, err
	}
	id, err := a.identify(ctx, h.user, write)
	if err != nil {
		return id, ch, root, err
	}
	ch, err = a.conversation(ctx, d.channel)
	if err != nil {
		return id, ch, root, err
	}
	if ch.visibility() != want {
		return id, ch, root, refusal("destination_changed")
	}
	if ch.Archived || ch.Frozen {
		return id, ch, root, refusal("channel_archived")
	}
	if err := a.requireMember(ctx, ch, h.user); err != nil {
		return id, ch, root, err
	}
	if d.thread != "" {
		root, err = a.root(ctx, d)
	}
	return id, ch, root, err
}

func (h *postHandler) snapshot(v approval.View) (slackDraft, error) {
	if v.Type != approval.Post || !h.bound(v.Frozen) || !h.placed(v.Slack) || v.SlackOperation == "" || v.StartedAt.IsZero() {
		return slackDraft{}, refusal("destination_changed")
	}
	d, err := h.draft(v.Frozen.Params)
	if err != nil {
		return d, err
	}
	if !maps.Equal(v.Frozen.Destination, d.destination()) {
		return d, refusal("destination_changed")
	}
	return d, d.checkLength()
}

func (h *postHandler) finalCheck(ctx context.Context, a slackAPI, d slackDraft, want approval.SlackVisibility) error {
	leave, err := h.final.enter()
	if err != nil {
		return err
	}
	defer leave()
	ctx, cancel := context.WithTimeout(ctx, readDeadline)
	defer cancel()
	_, _, _, err = h.check(ctx, a, d, want, true)
	return err
}

func (h *postHandler) Run(ctx context.Context, j *approval.Journal, v approval.View, write keychain.Secret) error {
	if !j.MayDispatch(v.ID) {
		return stopBeforeDispatch(j, v.ID, h.now())
	}
	d, err := h.snapshot(v)
	a := h.api(write, slackCheck, h.final)
	if err == nil {
		err = h.finalCheck(ctx, a, d, v.Slack.Visibility)
	}
	if err == nil {
		err = a.canPost()
	}
	if err != nil {
		return h.failPost(j, v.ID, "pre_dispatch", err)
	}
	if ctx.Err() != nil || !j.ClaimDispatch(v.ID) {
		return stopBeforeDispatch(j, v.ID, h.now())
	}
	reply := a.post(ctx, d, h.user, v.SlackOperation)
	switch {
	case reply.ts != "":
		return h.posted(j, v.ID, d.channel, reply.ts, "reply")
	case reply.refused != nil:
		return h.failPost(j, v.ID, "reply", reply.refused)
	default:
		if err := j.Unknown(v.ID, reply.ambiguous, h.now()); err != nil {
			return err
		}
		return h.Reconcile(ctx, j, v.ID)
	}
}

func (h *postHandler) failPost(j *approval.Journal, id, by string, err error) error {
	r := approval.Refusal{Code: "slack_unavailable"}
	_ = errors.As(err, &r)
	return j.SetResult(id, approval.Result{ConfirmedBy: by, Error: r.Code, Text: r.Detail}, h.now())
}

func (h *postHandler) posted(j *approval.Journal, id, channel, ts, by string) error {
	return j.SetResult(id, approval.Result{ConfirmedBy: by, Native: "slack:message:" + channel + ":" + ts, Text: "Posted to Slack."}, h.now())
}
