package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/google/googletest"
	"github.com/CrossTerra/beacon/internal/keychain"
)

// browser plays the person's browser: it reads the consent URL and follows the redirect with a code.
func browser(t *testing.T, srv *googletest.Server, state func(string) string, challenge ...string) func(string) error {
	return func(consent string) error {
		u, err := url.Parse(consent)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("client_id") != googletest.ClientID || q.Get("code_challenge_method") != "S256" || q.Get("access_type") != "offline" {
			t.Errorf("consent query %v", q)
		}
		if strings.Contains(consent, googletest.ClientSecret) {
			t.Error("the consent URL carries the client secret")
		}
		redirect, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || redirect.Hostname() != "127.0.0.1" || redirect.Scheme != "http" {
			t.Errorf("redirect %q", q.Get("redirect_uri"))
		}
		srv.Challenge = q.Get("code_challenge")
		if len(challenge) > 0 {
			srv.Challenge = challenge[0]
		}
		go func() {
			resp, err := http.Get(fmt.Sprintf("%s?code=%s&state=%s&scope=x", q.Get("redirect_uri"), srv.Code, url.QueryEscape(state(q.Get("state")))))
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
}

func client(t *testing.T) keychain.Secret {
	c, err := keychain.OAuthClient([]byte(googletest.ClientFile))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFlow(t *testing.T) {
	srv := googletest.New(t)
	var out strings.Builder
	f := Flow{
		AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: srv.URL + "/token", HTTP: srv.Client(),
		Scopes: Scopes([]string{"gmail", "calendar", "drive"}, false), Out: &out, Timeout: 5 * time.Second,
		Open: browser(t, srv, func(s string) string { return s }),
	}
	tok, err := f.Run(context.Background(), client(t))
	if err != nil {
		t.Fatal(err)
	}
	if tok.Refresh.Empty() || strings.Join(tok.Scopes, " ") != googletest.AllScopes {
		t.Fatalf("token %v", tok.Scopes)
	}
	if srv.Challenge == "" {
		t.Fatal("no PKCE challenge")
	}
	for _, secret := range []string{googletest.Refresh, googletest.Access, googletest.ClientSecret, srv.Code} {
		if strings.Contains(out.String(), secret) || strings.Contains(fmt.Sprintf("%v %+v", tok, tok), secret) {
			t.Fatalf("a credential was printed: %s", out.String())
		}
	}
}

func TestFlowRefusesAnotherState(t *testing.T) {
	srv := googletest.New(t)
	f := Flow{
		TokenURL: srv.URL + "/token", HTTP: srv.Client(), Scopes: []string{ScopeGmailMetadata}, Timeout: 500 * time.Millisecond,
		Open: browser(t, srv, func(string) string { return "forged" }),
	}
	if _, err := f.Run(context.Background(), client(t)); err == nil || !strings.Contains(err.Error(), "no redirect arrived") {
		t.Fatalf("%v", err)
	}
	if srv.Called("/token") {
		t.Fatal("a forged redirect reached the token endpoint")
	}
}

func TestFlowNeedsEveryScope(t *testing.T) {
	srv := googletest.New(t)
	srv.Scope = ScopeGmailMetadata
	f := Flow{
		TokenURL: srv.URL + "/token", HTTP: srv.Client(), Scopes: Scopes([]string{"gmail", "drive"}, false), Timeout: 5 * time.Second,
		Open: browser(t, srv, func(s string) string { return s }),
	}
	_, err := f.Run(context.Background(), client(t))
	var ng *ErrNotGranted
	if !errors.As(err, &ng) || len(ng.Missing) != 2 {
		t.Fatalf("%v", err)
	}
}

func TestFlowWrongVerifier(t *testing.T) {
	srv := googletest.New(t)
	f := Flow{
		TokenURL: srv.URL + "/token", HTTP: srv.Client(), Scopes: []string{ScopeGmailMetadata}, Timeout: 5 * time.Second,
		Open: browser(t, srv, func(s string) string { return s }, "not-the-challenge"),
	}
	if _, err := f.Run(context.Background(), client(t)); !errors.Is(err, keychain.ErrGrantRefused) {
		t.Fatalf("%v", err)
	}
}

func TestFlowConsentRefused(t *testing.T) {
	srv := googletest.New(t)
	f := Flow{
		TokenURL: srv.URL + "/token", HTTP: srv.Client(), Scopes: []string{ScopeGmailMetadata}, Timeout: 5 * time.Second,
		Open: func(consent string) error {
			u, _ := url.Parse(consent)
			q := u.Query()
			go func() {
				resp, err := http.Get(q.Get("redirect_uri") + "?error=access_denied&state=" + url.QueryEscape(q.Get("state")))
				if err == nil {
					resp.Body.Close()
				}
			}()
			return nil
		},
	}
	if _, err := f.Run(context.Background(), client(t)); !errors.Is(err, keychain.ErrRedirect) {
		t.Fatalf("%v", err)
	}
}
