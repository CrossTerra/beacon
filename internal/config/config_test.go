package config

import (
	"strings"
	"testing"
	"time"
)

const valid = `
signal:
  url: https://signal.example.com
person:
  handle: "@winton"
  identities:
    github: github:user:winton
    gmail: email:winton@example.com
rules: rules.yaml
state: state
mode: dry-run
sources:
  - kind: fixture
    path: fixture.json
`

func TestValid(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.SignalHost() != "signal.example.com:443" || c.KeychainAccount() != "signal:signal.example.com:443" {
		t.Fatalf("%s %s", c.SignalHost(), c.KeychainAccount())
	}
	if c.LeasePeriod() != 5*time.Minute || c.PollInterval() != time.Minute {
		t.Fatal("defaults")
	}
	c.Path = "/home/w/.config/beacon/config.yaml"
	if c.RulesPath() != "/home/w/.config/beacon/rules.yaml" {
		t.Fatal(c.RulesPath())
	}
	out, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(out); err != nil {
		t.Fatalf("marshalled config does not parse: %v\n%s", err, out)
	}
}

func TestRejected(t *testing.T) {
	cases := map[string][2]string{
		"unknown key":      {"mode: dry-run", "mode: dry-run\ntelemetry: true"},
		"http remote":      {"https://signal.example.com", "http://signal.example.com"},
		"path in url":      {"https://signal.example.com", "https://signal.example.com/api"},
		"credentials":      {"https://signal.example.com", "https://u:p@signal.example.com"},
		"mailto identity":  {"email:winton@example.com", "mailto:winton@example.com"},
		"signal identity":  {"github:user:winton", "signal:actor:x"},
		"bad handle":       {`"@winton"`, `"winton"`},
		"bad mode":         {"mode: dry-run", "mode: live"},
		"unknown source":   {"kind: fixture", "kind: slack"},
		"lease too short":  {"mode: dry-run", "mode: dry-run\nlease:\n  period: 30s"},
		"poll too slow":    {"mode: dry-run", "mode: dry-run\npoll_interval: 4m"},
		"secret in config": {"mode: dry-run", "mode: dry-run\ntoken: sig_x"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			y := strings.Replace(valid, c[0], c[1], 1)
			if y == valid {
				t.Fatal("replacement did not apply")
			}
			if _, err := Parse([]byte(y)); err == nil {
				t.Fatalf("parsed:\n%s", y)
			}
		})
	}
}

func TestGitHubAndJiraConfig(t *testing.T) {
	y := strings.Replace(valid, "sources:\n  - kind: fixture\n    path: fixture.json\n", `sources:
  - kind: github
    repos: ["acme/app", "acme/*"]
    projects: ["ENG"]
  - kind: jira
    site: acme
    projects: ["ENG"]
`, 1)
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sources[0].SourceName() != "github" || c.Sources[1].SourceName() != "jira" {
		t.Fatalf("%+v", c.Sources)
	}
	for _, bad := range []string{
		strings.Replace(y, "acme/app", "../secret", 1),
		strings.Replace(y, "site: acme", "site: evil.example", 1),
		strings.Replace(y, "projects: [\"ENG\"]", "projects: [\"eng\"]", 1),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("parsed:\n%s", bad)
		}
	}
}

func TestLoopbackHTTP(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		if _, err := SignalURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestGoogleConfig(t *testing.T) {
	y := strings.Replace(valid, "sources:\n  - kind: fixture\n    path: fixture.json\n", "sources:\n  - kind: gmail\n  - kind: calendar\n  - kind: drive\n", 1)
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Sources[0].IsGoogle() || c.Sources[2].SourceName() != "drive" {
		t.Fatalf("%+v", c.Sources)
	}
	for _, bad := range []string{
		strings.Replace(y, `"@winton"`, "act_abc", 1),
		strings.Replace(y, "  - kind: drive\n", "  - kind: drive\n    site: acme\n", 1),
		strings.Replace(y, "  - kind: drive\n", "  - kind: drive\n    host: evil.example\n", 1),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("parsed:\n%s", bad)
		}
	}
}
