package egress

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/CrossTerra/beacon/internal/source"
)

type fakeSource struct {
	name  string
	hosts []string
}

func (f fakeSource) Name() string                                   { return f.name }
func (f fakeSource) Fields() []string                               { return nil }
func (f fakeSource) Hosts() []string                                { return f.hosts }
func (f fakeSource) Collect(context.Context) ([]source.Item, error) { return nil, nil }

func TestList(t *testing.T) {
	u, _ := url.Parse("https://Signal.Example.com")
	got := List(u, []source.Source{
		fakeSource{"github", []string{"api.github.com:443"}},
		fakeSource{"jira", []string{"acme.atlassian.net:443", "api.github.com:443"}},
	})
	want := []string{"acme.atlassian.net:443", "api.github.com:443", "signal.example.com:443"}
	if a := Addresses(got); len(a) != 3 || a[0] != want[0] || a[1] != want[1] || a[2] != want[2] {
		t.Fatalf("%v", a)
	}
	if got[1].Purpose != "source github; source jira" {
		t.Fatalf("%q", got[1].Purpose)
	}
}

type mustNotCall struct{ t *testing.T }

func (m mustNotCall) RoundTrip(*http.Request) (*http.Response, error) {
	m.t.Fatal("a denied request reached the transport")
	return nil, nil
}

func TestGuard(t *testing.T) {
	g := Guard([]string{"signal.example.com:443", "127.0.0.1:8080"}, mustNotCall{t})
	for _, raw := range []string{"https://signal.example.com:8443/", "http://signal.example.com/", "https://evil.example/", "http://127.0.0.1:8081/"} {
		req, _ := http.NewRequest(http.MethodPost, raw, nil)
		_, err := g.RoundTrip(req)
		var d *DeniedError
		if !errors.As(err, &d) {
			t.Errorf("%s: %v", raw, err)
		}
	}
}
