package google

import (
	"os"
	"strings"
	"testing"

	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/source"
)

// Gmail files a service's billing problem under Updates. The starter rules send it, asking, before the bulk rule drops
// the rest of Updates, and still drop the same service's receipts.
func TestStarterRulesSendServiceAlerts(t *testing.T) {
	data, err := os.ReadFile("../../examples/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f, err := rules.Parse([]byte(strings.ReplaceAll(string(data), "YOUR_LOGIN", "winton")))
	if err != nil {
		t.Fatal(err)
	}
	mail := func(from, subject string) source.Item {
		return source.Item{Source: "gmail", Subject: "gmail:winton:thread:t1", Key: "gmail:t1:m1", Type: TypeReceived,
			OccurredAt: testNow, Actor: "email:" + from,
			Fields: map[string]string{"from": from, "to": "winton@example.com", "subject": subject, "bulk": "true",
				"labels": "CATEGORY_UPDATES, UNREAD", "date": testNow.Format("2006-01-02T15:04:05Z07:00"),
				"link": "https://mail.google.com/mail/u/0/#all/t1"}}
	}
	for _, tc := range []struct {
		from, subject, rule string
		tier                rules.Tier
	}{
		{"noreply@github.com", "[GitHub] Action needed: a billing problem on the acme account", "gmail-service-alerts", rules.Send},
		{"noreply@github.com", "[GitHub] Your payment failed", "gmail-service-alerts", rules.Send},
		{"noreply@github.com", "[GitHub] Payment receipt for acme", "gmail-bulk", rules.Never},
		{"noreply@notgithub.com", "There is a problem with your billing", "gmail-bulk", rules.Never},
	} {
		d := f.Decide(mail(tc.from, tc.subject))
		if d.Tier != tc.tier || d.Rule == nil || d.Rule.Name != tc.rule {
			t.Errorf("%s %q: %s by %v", tc.from, tc.subject, d.Tier, d.Rule)
			continue
		}
		if tc.tier == rules.Send && d.Rule.Ask == "" {
			t.Errorf("%q: sent without an ask", tc.subject)
		}
	}
}
