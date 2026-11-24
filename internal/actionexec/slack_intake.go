package actionexec

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
)

const (
	slackIntakeDeadline = 4 * time.Second
	slackNameLife       = 10 * time.Minute
)

type namedAt struct {
	name string
	at   time.Time
}

type peerBudget struct {
	budget *budget
	usedAt time.Time
}

type slackLookups struct {
	mu          sync.Mutex
	peers       map[string]peerBudget
	identity    slackIdentity
	identityAt  time.Time
	identityKey [32]byte
	names       map[string]namedAt
}

func (h *postHandler) peerMeter(peer string) (*budget, error) {
	c, at := &h.lookup, h.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.peers {
		if at.Sub(entry.usedAt) >= time.Hour {
			delete(c.peers, key)
		}
	}
	entry, ok := c.peers[peer]
	if !ok {
		// A peer is a trusted run ID (or all unattributed peers), not a caller-chosen name.
		if len(c.peers) == 128 {
			return nil, refusal("busy")
		}
		entry.budget = newBudget(h.intakePerPeer, 2)
	}
	if c.peers == nil {
		c.peers = make(map[string]peerBudget)
	}
	entry.usedAt = at
	c.peers[peer] = entry
	return entry.budget, nil
}

func (h *postHandler) place(parent context.Context, req approval.Request, peer string) (approval.SlackPlacement, error) {
	if req.Type != approval.Post {
		return approval.SlackPlacement{}, refusal("not_configured")
	}
	d, err := h.draft(req.Params)
	if err != nil {
		return approval.SlackPlacement{}, err
	}
	leave, err := h.intake.enter()
	if err != nil {
		return approval.SlackPlacement{}, err
	}
	defer leave()
	meter, err := h.peerMeter(peer)
	if err != nil {
		return approval.SlackPlacement{}, err
	}
	ctx, cancel := context.WithTimeout(parent, slackIntakeDeadline)
	defer cancel()
	read, err := h.reader()
	if err != nil {
		return approval.SlackPlacement{}, err
	}
	defer read.Clear()
	a := h.api(read, slackIntake, h.intake)
	a.peer = meter
	id, err := h.intakeIdentity(ctx, a)
	if err != nil {
		return approval.SlackPlacement{}, err
	}
	ch, err := a.conversation(ctx, d.channel)
	if err != nil {
		return approval.SlackPlacement{}, err
	}
	vis := ch.visibility()
	if vis == "" {
		return approval.SlackPlacement{}, refusal("visibility_unknown")
	}
	label := h.conversationName(ctx, a, ch, vis)
	if parent.Err() != nil {
		return approval.SlackPlacement{}, refusal("slack_unavailable")
	}
	return approval.SlackPlacement{Account: h.binding.Account, Visibility: vis, Conversation: label, WorkspaceLabel: workspaceName(id)}, nil
}

func (h *postHandler) intakeIdentity(ctx context.Context, a slackAPI) (slackIdentity, error) {
	c, at := &h.lookup, h.now()
	key := keychain.ActionTokenFingerprint(a.token)
	c.mu.Lock()
	id, hit := c.identity, c.identityKey == key && fresh(c.identityAt, at)
	c.mu.Unlock()
	if hit {
		return id, nil
	}
	id, err := a.identify(ctx, h.user, false)
	if err != nil {
		return id, err
	}
	c.mu.Lock()
	c.identity, c.identityKey, c.identityAt = id, key, at
	c.mu.Unlock()
	return id, nil
}

func fresh(then, now time.Time) bool { return !now.Before(then) && now.Sub(then) < slackNameLife }

func workspaceName(id slackIdentity) string {
	if name := oneLine(id.Team, 80); name != "" {
		return name
	}
	return "unknown workspace"
}

func (h *postHandler) conversationName(ctx context.Context, a slackAPI, ch slackConversation, vis approval.SlackVisibility) string {
	switch vis {
	case approval.SlackPublic, approval.SlackPrivate:
		if name := oneLine(ch.Name, 80); name != "" {
			return "#" + name
		}
	case approval.SlackDM:
		if ch.User == h.user {
			return "DM with yourself"
		}
		if name := h.displayUser(ctx, a, ch.User); name != "" {
			return "DM with " + name
		}
	case approval.SlackGroupDM:
		if name := h.groupName(ctx, a, ch.ID); name != "" {
			return name
		}
	}
	return "unknown channel " + ch.ID
}

func (h *postHandler) groupName(ctx context.Context, a slackAPI, channel string) string {
	key := "group:" + channel
	if name := h.lookup.name(key, h.now()); name != "" {
		return name
	}
	p, err := a.memberPage(ctx, channel, "")
	if err != nil {
		return ""
	}
	ids := slices.DeleteFunc(p.Members, func(id string) bool { return id == h.user || !slackUserID.MatchString(id) })
	slices.Sort(ids)
	ids = slices.Compact(ids)
	var names []string
	for _, id := range ids[:min(len(ids), 8)] {
		if name := h.displayUser(ctx, a, id); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	phrase := "Group DM with " + strings.Join(names, ", ")
	if rest := len(ids) - len(names); rest > 0 {
		phrase += " +" + strconv.Itoa(rest)
	}
	// Don't cache an incomplete name if a lookup failed, or imply the first page is everyone.
	if p.Cursor.Next != "" {
		phrase += " (more members)"
	}
	if len(names) == min(len(ids), 8) {
		h.lookup.remember(key, phrase, h.now())
	}
	return phrase
}

func (h *postHandler) displayUser(ctx context.Context, a slackAPI, id string) string {
	if !slackUserID.MatchString(id) {
		return ""
	}
	key := "user:" + id
	if name := h.lookup.name(key, h.now()); name != "" {
		return name
	}
	name, err := a.userName(ctx, id)
	if err != nil || name == "" {
		return ""
	}
	h.lookup.remember(key, name, h.now())
	return name
}

func (c *slackLookups) name(key string, at time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.names[key]; ok && fresh(v.at, at) {
		return v.name
	}
	return ""
}

func (c *slackLookups) remember(key, name string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.names {
		if !fresh(v.at, at) {
			delete(c.names, k)
		}
	}
	if c.names == nil {
		c.names = make(map[string]namedAt)
	}
	if len(c.names) < 512 {
		c.names[key] = namedAt{name, at}
	}
}
