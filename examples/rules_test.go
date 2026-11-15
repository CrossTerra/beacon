package examples

import (
	"testing"

	"github.com/CrossTerra/beacon/internal/github"
	"github.com/CrossTerra/beacon/internal/google"
	"github.com/CrossTerra/beacon/internal/jira"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/slack"
	"github.com/CrossTerra/beacon/internal/source"
)

func TestExampleChecks(t *testing.T) {
	f, err := rules.Load("rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// As beacon rules check sees them, with which items are personal.
	all := sealSources()
	sets := map[string]map[string]rules.Source{"all": all}
	for name, s := range all {
		sets[name] = map[string]rules.Source{name: s}
	}
	for name, sources := range sets {
		for _, p := range f.Check(sources) {
			if p.Error() {
				t.Errorf("%s: %s", name, p)
			}
		}
	}
}

// The starter rules list body only for Slack, and only on the channel message rules: plain in public channels, sealed
// in private ones.
func TestExampleListsBodyOnlyForChannels(t *testing.T) {
	f, err := rules.Load("rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"github", "jira", "gmail", "calendar", "drive"} {
		if f.Lists(name, "body") {
			t.Errorf("a starter rule lists body for %s", name)
		}
	}
	for _, r := range f.Rules {
		if !r.ListsBody() {
			continue
		}
		switch r.Name {
		case "slack-channel-messages":
			if r.Seal != "" {
				t.Errorf("%s is sealed", r.Name)
			}
		case "slack-channel-messages-private":
			if r.Seal != "owner+agents" || len(r.Plain) != 0 {
				t.Errorf("%s: seal %q plain %v", r.Name, r.Seal, r.Plain)
			}
		default:
			t.Errorf("%s lists body", r.Name)
		}
	}
}

func channelItem(typ, kind, author string) source.Item {
	return source.Item{Source: "slack", Type: typ, Fields: map[string]string{"channel": "C0ENG", "channel_type": kind, "author": author}}
}

// Both example files decide Slack channel items the same way: a public channel's sent plain with its text, a private
// channel's sealed for the person and their agents with its text, an app's dropped, and none asks.
func TestExampleChannelMessages(t *testing.T) {
	for _, file := range []string{"rules.yaml", "rules-sealed.yaml"} {
		f, err := rules.Load(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			it   source.Item
			tier rules.Tier
			seal string
		}{
			{channelItem(slack.TypeChannel, "channel", "U0BOB"), rules.Send, ""},
			{channelItem(slack.TypeChannel, "private", "U0BOB"), rules.Send, "owner+agents"},
			{channelItem(slack.TypeReviewRequest, "channel", "U0ERIN"), rules.Send, ""},
			{channelItem(slack.TypeReviewRequest, "private", "U0ERIN"), rules.Send, "owner+agents"},
			{channelItem(slack.TypeChannel, "channel", "B0CI"), rules.Never, ""},
			{channelItem(slack.TypeChannel, "private", "B0CI"), rules.Never, ""},
		} {
			d := f.Decide(tc.it)
			if d.Tier != tc.tier {
				t.Errorf("%s: %s %s by %s: %s", file, tc.it.Type, tc.it.Fields["channel_type"], tc.it.Fields["author"], d.Tier)
				continue
			}
			if d.Tier == rules.Never {
				continue
			}
			if d.Rule.Seal != tc.seal || d.Rule.Ask != "" || !d.Rule.ListsBody() {
				t.Errorf("%s: %s %s: rule %s seal %q ask %q", file, tc.it.Type, tc.it.Fields["channel_type"], d.Rule.Name, d.Rule.Seal, d.Rule.Ask)
			}
		}
	}
}

// rules check refuses a channel message rule that sends the text plain and can match a private channel (DEC-32), and
// accepts one limited to public channels.
func TestChannelBodyCheck(t *testing.T) {
	for rule, wantErr := range map[string]bool{
		"{name: c, match: {source: slack, type: channel_message}, tier: send, fields: [body]}":                                                     true,
		"{name: c, match: {source: slack, type: channel_message, fields: {channel_type: {regex: 'channel|private'}}}, tier: send, fields: [body]}": true,
		"{name: c, match: {source: slack, type: channel_message, fields: {channel_type: channel}}, tier: send, fields: [body]}":                    false,
		"{name: c, match: {source: slack, type: channel_message, fields: {channel_type: private}}, tier: send, seal: owner, fields: [body]}":       false,
	} {
		f, err := rules.Parse([]byte("rules:\n  - " + rule + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		got := false
		for _, p := range f.Check(sealSources()) {
			got = got || p.Error()
		}
		if got != wantErr {
			t.Errorf("%s: error %v", rule, got)
		}
	}
}

// sealSources are every source's fields, and which of their items are personal, as `beacon rules check` sees them.
func sealSources() map[string]rules.Source {
	return map[string]rules.Source{
		"github":   {Fields: github.Fields, StateFields: github.StateFields, Identity: true},
		"jira":     {Fields: jira.Fields, StateFields: jira.StateFields, Identity: true},
		"gmail":    {Fields: google.GmailFields, StateFields: google.GmailStateFields, Identity: true, Personal: rules.PersonalAll},
		"calendar": {Fields: google.CalendarFields, StateFields: google.CalendarStateFields, Identity: true, Personal: rules.PersonalAll},
		"drive":    {Fields: google.DriveFields, StateFields: google.DriveStateFields, Identity: true, Personal: rules.PersonalAll},
		"slack": {Fields: slack.Fields, StateFields: slack.StateFields, Identity: true, Personal: rules.PersonalSome,
			PersonalField: "channel_type", PersonalValues: slack.PersonalKinds},
	}
}

// The sealed example checks clean, warnings included, against every source's fields.
func TestSealedExampleChecksClean(t *testing.T) {
	f, err := rules.Load("rules-sealed.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range f.Check(sealSources()) {
		t.Errorf("%s", p)
	}
	for _, name := range []string{"gmail", "drive", "slack"} {
		if !f.ListsSealed(name, "body") {
			t.Errorf("no sealed rule lists body for %s", name)
		}
	}
}

// The starter rules have no DEC-32 error with the personal sources marked.
func TestStarterRulesHaveNoSealErrors(t *testing.T) {
	f, err := rules.Load("rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range f.Check(sealSources()) {
		if p.Error() {
			t.Errorf("%s", p)
		}
	}
}
