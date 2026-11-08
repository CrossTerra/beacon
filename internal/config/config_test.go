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
		"unknown source":   {"kind: fixture", "kind: github"},
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

func TestLoopbackHTTP(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		if _, err := SignalURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}
