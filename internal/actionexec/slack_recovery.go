package actionexec

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

const slackSearchWindow = 5 * time.Minute

type slackRetries struct {
	mu       sync.Mutex
	attempts map[string]slackAttempt
}

type slackAttempt struct {
	last  time.Time
	count int
}

func (r *slackRetries) mark(id string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attempts == nil {
		r.attempts = make(map[string]slackAttempt)
	}
	r.attempts[id] = slackAttempt{at, min(r.attempts[id].count+1, 4)}
}

func (r *slackRetries) forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.attempts, id)
}

func (h *postHandler) Due(v approval.View) bool {
	if v.Status != "outcome_unknown" {
		return false
	}
	if _, err := h.snapshot(v); err != nil {
		return false
	}
	h.retries.mu.Lock()
	a, ok := h.retries.attempts[v.ID]
	h.retries.mu.Unlock()
	if !ok {
		return true // one read-back per launch even if the search window has long passed
	}
	at := h.now()
	return at.Sub(v.StartedAt) <= slackSearchWindow+time.Minute && at.Sub(a.last) >= 15*time.Second<<(a.count-1)
}

// Reconcile can only read with the bound unattended token. A negative or incomplete search, even
// long after start, never becomes "not posted" and never permits another write.
func (h *postHandler) Reconcile(ctx context.Context, j *approval.Journal, id string) error {
	v, done, ok := h.readView(ctx, j, id, approval.Post)
	if !ok {
		return nil
	}
	defer done()
	d, err := h.snapshot(v)
	if err != nil {
		return nil
	}
	h.retries.mark(id, h.now())
	leave, err := h.recovery.enter()
	if err != nil {
		return nil
	}
	defer leave()
	ctx, cancel := context.WithTimeout(ctx, readDeadline)
	defer cancel()
	read, err := h.reader()
	if err != nil {
		return nil
	}
	defer read.Clear()
	a := h.api(read, slackRecover, h.recovery)
	if _, err := a.identify(ctx, h.user, false); err != nil {
		return nil
	}
	ch, err := a.conversation(ctx, d.channel)
	if err != nil || ch.visibility() != v.Slack.Visibility || a.requireMember(ctx, ch, h.user) != nil {
		return nil
	}
	ts := a.find(ctx, d, h.user, v.SlackOperation, v.StartedAt)
	if ts == "" {
		return nil
	}
	h.retries.forget(id)
	return h.posted(j, v.ID, d.channel, ts, "read_back")
}

func (a slackAPI) find(ctx context.Context, d slackDraft, user, op string, start time.Time) string {
	// Search around the original start, not around the time of the restart or button press. Leave
	// room for both clock skew and final reads. Slack accepts seconds plus six decimal places.
	oldest := start.Add(-time.Minute).Truncate(time.Second)
	latest := a.now().Add(time.Minute)
	if end := start.Add(slackSearchWindow); end.Before(latest) {
		latest = end
	}
	if floor := latest.Truncate(time.Second); !latest.Equal(floor) {
		latest = floor.Add(time.Second)
	}
	if latest.Before(oldest) {
		return ""
	}
	q := url.Values{"limit": {"15"}, "inclusive": {"true"}, "include_all_metadata": {"true"},
		"oldest": {strconv.FormatInt(oldest.Unix(), 10) + ".000000"}, "latest": {strconv.FormatInt(latest.Unix(), 10) + ".000000"}}
	seen := make(map[string]bool)
	found := ""
	for range 4 {
		p, err := a.messages(ctx, d, q)
		if err != nil || p.Messages == nil {
			return ""
		}
		for _, m := range p.Messages {
			marked, conflict := m.markedBy(op)
			if conflict || marked && (!m.matches(d, user, oldest, latest) || found != "" && found != m.TS) {
				return ""
			}
			if marked {
				found = m.TS
			}
		}
		next, err := p.Cursor.next(seen)
		if err != nil {
			return ""
		}
		if next == "" {
			if p.More {
				return ""
			}
			return found
		}
		setCursor(q, next)
	}
	return ""
}

func (m slackMessage) markedBy(op string) (match, conflict bool) {
	// Slack can assign its own client_msg_id. Prefer a Beacon metadata ID if readable;
	// an event_type without its payload does not contradict the client_msg_id.
	var meta struct {
		Type    string          `json:"event_type"`
		Payload json.RawMessage `json:"event_payload"`
	}
	if json.Unmarshal(m.Metadata, &meta) == nil && meta.Type == slackEventType && len(meta.Payload) != 0 {
		var payload struct {
			Op string `json:"op"`
		}
		if err := json.Unmarshal(meta.Payload, &payload); err != nil {
			return false, m.ClientID == op
		}
		if payload.Op != "" {
			return payload.Op == op, m.ClientID == op && payload.Op != op
		}
	}
	return m.ClientID == op, false
}

// Slack can escape bare ampersands in the returned mrkdwn. Compare that representation without
// decoding existing entities or changing links, mentions, Unicode, or whitespace.
var slackAmpersands = strings.NewReplacer("&amp;", "&amp;", "&lt;", "&lt;", "&gt;", "&gt;", "&", "&amp;")

func (m slackMessage) matches(d slackDraft, user string, oldest, latest time.Time) bool {
	// An app attribution is not a bot author when Slack attests to the approved user
	// on an ordinary message. Auth still requires a user token, not a bot token.
	if m.Type != "message" || m.Subtype != nil || m.User == nil || *m.User != user ||
		m.TS == d.thread || !slackTS.MatchString(m.TS) || !absentOr(m.Channel, d.channel) || m.Text == nil {
		return false
	}
	if *m.Text != d.text && *m.Text != slackAmpersands.Replace(d.text) {
		return false
	}
	if d.thread != "" && m.Thread == nil || !absentOr(m.Thread, threadOrSelf(d.thread, m.TS)) {
		return false
	}
	sec, frac, _ := strings.Cut(m.TS, ".")
	s, _ := strconv.ParseInt(sec, 10, 64)
	us, _ := strconv.ParseInt(frac, 10, 64)
	at := time.Unix(s, us*1000)
	return !at.Before(oldest) && !at.After(latest)
}
