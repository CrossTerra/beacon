package approval

import (
	"crypto/rand"
	"fmt"
	"time"
)

type SlackVisibility string

const (
	SlackPublic  SlackVisibility = "public"
	SlackPrivate SlackVisibility = "private"
	SlackDM      SlackVisibility = "dm"
	SlackGroupDM SlackVisibility = "group_dm"
)

func (v SlackVisibility) Label() string {
	switch v {
	case SlackDM:
		return "DM"
	case SlackGroupDM:
		return "group DM"
	default:
		return string(v)
	}
}

// SlackPlacement is verified by the read-only intake lookup, never supplied by the proposer.
// The account and visibility are rechecked, but the labels are display only. PublicDay is set
// by the journal so a top-level post keeps the same public subject across dates and restarts.
type SlackPlacement struct {
	Account        string          `json:"account"`
	Visibility     SlackVisibility `json:"visibility"`
	Conversation   string          `json:"conversation"`
	WorkspaceLabel string          `json:"workspace_label"`
	PublicDay      string          `json:"public_day,omitempty"`
}

func (p SlackPlacement) valid(r Request) bool {
	if r.Type != Post || !label.matches(p.Account) || !(field{max: 800}).matches(p.Conversation) ||
		!(field{max: 80}).matches(p.WorkspaceLabel) {
		return false
	}
	switch p.Visibility {
	case SlackPublic:
		if r.Params["thread_ts"] == nil {
			_, err := time.Parse(time.DateOnly, p.PublicDay)
			return err == nil
		}
		return p.PublicDay == ""
	case SlackPrivate, SlackDM, SlackGroupDM:
		return p.PublicDay == ""
	default:
		return false
	}
}

func (p SlackPlacement) publicSubject(params Object) string {
	if p.Visibility != SlackPublic {
		return ""
	}
	channel := text(params, "channel")
	if !paramsSchemas[Post]["channel"].matches(channel) {
		return ""
	}
	if ts := text(params, "thread_ts"); ts != "" && threadTS.matches(ts) {
		return "slack:" + channel + ":" + ts
	}
	if _, err := time.Parse(time.DateOnly, p.PublicDay); err == nil && params["thread_ts"] == nil {
		return "slack:" + channel + ":" + p.PublicDay
	}
	return ""
}

var slackOperation = pattern(`[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`)

func newSlackOperation() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&15 | 0x40
	b[8] = b[8]&63 | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
