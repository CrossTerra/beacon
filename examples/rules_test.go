package examples

import (
	"testing"

	"github.com/CrossTerra/beacon/internal/github"
	"github.com/CrossTerra/beacon/internal/google"
	"github.com/CrossTerra/beacon/internal/jira"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/slack"
)

func TestExampleChecks(t *testing.T) {
	f, err := rules.Load("rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]rules.Source{
		"github":   {Fields: github.Fields, StateFields: github.StateFields, Identity: true},
		"jira":     {Fields: jira.Fields, StateFields: jira.StateFields, Identity: true},
		"gmail":    {Fields: google.GmailFields, StateFields: google.GmailStateFields, Identity: true},
		"calendar": {Fields: google.CalendarFields, StateFields: google.CalendarStateFields, Identity: true},
		"drive":    {Fields: google.DriveFields, StateFields: google.DriveStateFields, Identity: true},
		"slack":    {Fields: slack.Fields, StateFields: slack.StateFields, Identity: true},
	}
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

func TestExampleListsNoBody(t *testing.T) {
	f, err := rules.Load("rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"github", "jira", "gmail", "calendar", "drive", "slack"} {
		if f.Lists(name, "body") {
			t.Errorf("a starter rule lists body for %s", name)
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
