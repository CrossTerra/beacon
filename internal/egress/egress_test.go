package egress

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/CrossTerra/beacon/internal/source"
)

type fakeSource struct {
	name  string
	hosts []string
}

func (f fakeSource) Name() string                                   { return f.name }
func (f fakeSource) Fields() []string                               { return nil }
func (f fakeSource) StateFields() []string                          { return nil }
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

// The production transport takes no proxy from the environment: a proxy would be a host not on the list.
func TestTransportIgnoresProxyEnvironment(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	rt := Transport([]string{"beacon-egress-test.invalid:80"})
	inner, ok := rt.(*guard).next.(*http.Transport)
	if !ok || inner.Proxy != nil {
		t.Fatalf("transport proxy func set: %#v", inner)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://beacon-egress-test.invalid/", nil)
	resp, err := rt.RoundTrip(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("reached a host that does not exist: through the proxy")
	}
	if proxied.Load() != 0 {
		t.Fatal("the proxy from the environment received the request")
	}
}

// A transport never follows a redirect; the next request would go through the guard again in any case.
func TestTransportDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Add(1)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	rt := Transport([]string{Address(u)})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/events", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || followed.Load() != 0 {
		t.Fatalf("status %d, followed %d", resp.StatusCode, followed.Load())
	}
}
