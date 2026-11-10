package google

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
)

// Flow is the installed-app authorisation: PKCE and a loopback redirect (RFC 8252). Beacon prints the consent URL;
// the person's browser, not Beacon, contacts the consent page. Beacon listens on 127.0.0.1 for one redirect and
// exchanges the code at the token endpoint through HTTP, which the caller guards with the egress list.
type Flow struct {
	// AuthURL is the consent page. Empty is Google's.
	AuthURL string
	// TokenURL is the token endpoint. Empty is Google's.
	TokenURL string
	HTTP     *http.Client
	Scopes   []string
	// Out is where the consent URL is printed.
	Out io.Writer
	// Open opens the consent URL in a browser. Nil prints it only.
	Open func(string) error
	// Timeout is how long to wait for the redirect. Zero is five minutes.
	Timeout time.Duration
	// LoginHint is the Google account the consent screen should offer. Empty offers any.
	LoginHint string
}

// ErrNotGranted means the person did not grant every scope asked for. The text names the scopes, not a token.
type ErrNotGranted struct{ Missing []string }

func (e *ErrNotGranted) Error() string {
	return "google: these scopes were not granted: " + strings.Join(e.Missing, " ") + "; run the command again and allow them"
}

// Run asks the person to consent and returns the token the code was exchanged for. The token holds a refresh token.
func (f Flow) Run(ctx context.Context, client keychain.Secret) (keychain.Token, error) {
	if f.HTTP == nil || len(f.Scopes) == 0 {
		return keychain.Token{}, errors.New("google: the flow has no client or scopes")
	}
	authURL, tokenURL := f.AuthURL, f.TokenURL
	if authURL == "" {
		authURL = AuthURL
	}
	if tokenURL == "" {
		tokenURL = TokenURL
	}
	clientID, err := client.OAuthClientID()
	if err != nil {
		return keychain.Token{}, err
	}
	verifier, challenge, err := keychain.PKCE()
	if err != nil {
		return keychain.Token{}, err
	}
	state, err := randomState()
	if err != nil {
		return keychain.Token{}, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return keychain.Token{}, errors.New("google: could not listen on 127.0.0.1 for the redirect")
	}
	redirect := "http://" + ln.Addr().String() + "/"

	type result struct {
		code keychain.Secret
		err  error
	}
	done := make(chan result, 1)
	var once sync.Once
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if r.Method != http.MethodGet || r.URL.Path != "/" || (q.Get("code") == "" && q.Get("error") == "") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			// A request without this flow's state is not the consent screen's answer. It is refused and the flow
			// keeps waiting, so another local process cannot end it.
			if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, "This is not the redirect Beacon is waiting for.\n")
				return
			}
			code, err := keychain.CodeFromRedirect(r.URL, state)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, "Beacon did not receive an authorisation. Return to the terminal.\n")
			} else {
				io.WriteString(w, "Beacon received the authorisation. You can close this tab.\n")
			}
			once.Do(func() { done <- result{code, err} })
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	params := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"scope":                 {strings.Join(f.Scopes, " ")},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
	}
	if f.LoginHint != "" {
		params.Set("login_hint", f.LoginHint)
	}
	consent := authURL + "?" + params.Encode()
	if f.Out != nil {
		fmt.Fprintf(f.Out, "Open this URL in your browser and allow read-only access:\n\n  %s\n\nWaiting for the redirect to %s\n", consent, redirect)
	}
	if f.Open != nil {
		if err := f.Open(consent); err != nil && f.Out != nil {
			fmt.Fprintln(f.Out, "Could not open a browser; open the URL above.")
		}
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var got result
	select {
	case <-ctx.Done():
		return keychain.Token{}, ctx.Err()
	case <-timer.C:
		return keychain.Token{}, errors.New("google: no redirect arrived in time")
	case got = <-done:
	}
	if got.err != nil {
		return keychain.Token{}, got.err
	}
	req, err := keychain.CodeRequest(ctx, tokenURL, client, got.code, verifier, redirect)
	if err != nil {
		return keychain.Token{}, err
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return keychain.Token{}, err
	}
	defer resp.Body.Close()
	tok, err := keychain.ReadToken(resp)
	if err != nil {
		return keychain.Token{}, err
	}
	if tok.Refresh.Empty() {
		return keychain.Token{}, errors.New("google: the token endpoint issued no refresh token")
	}
	granted := map[string]bool{}
	for _, s := range tok.Scopes {
		granted[s] = true
	}
	var missing []string
	for _, s := range f.Scopes {
		if !granted[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return keychain.Token{}, &ErrNotGranted{Missing: missing}
	}
	return tok, nil
}

func randomState() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
