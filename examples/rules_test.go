package examples

import (
	"testing"

	"github.com/CrossTerra/beacon/internal/github"
	"github.com/CrossTerra/beacon/internal/jira"
	"github.com/CrossTerra/beacon/internal/rules"
)

func TestExampleChecks(t *testing.T) {
	f, err := rules.Load("rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	both := map[string]rules.Source{
		"github": {Fields: github.Fields, StateFields: github.StateFields, Identity: true},
		"jira":   {Fields: jira.Fields, StateFields: jira.StateFields, Identity: true},
	}
	for name, sources := range map[string]map[string]rules.Source{
		"both":   both,
		"github": {"github": both["github"]},
		"jira":   {"jira": both["jira"]},
	} {
		for _, p := range f.Check(sources) {
			if p.Error() {
				t.Errorf("%s: %s", name, p)
			}
		}
	}
}
