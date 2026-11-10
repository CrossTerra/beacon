package keychain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuth errors. Their text is fixed: it names no token, code or client.
var (
	// ErrGrantRefused means the token endpoint refused the client, the code or the refresh token.
	ErrGrantRefused = errors.New("keychain: the OAuth grant was refused; authorise again")
	// ErrOAuthClient means a client file is not an installed-app OAuth client.
	ErrOAuthClient = errors.New("keychain: the file is not a Desktop app OAuth client (installed client id and secret)")
	// ErrRedirect means the loopback redirect did not carry a code for this flow.
	ErrRedirect = errors.New("keychain: the redirect did not carry an authorisation code for this request")
)

type oauthClient struct {
	ID     string `json:"client_id"`
	Secret string `json:"client_secret"`
}

// OAuthClient reads an OAuth client file as Google Cloud Console downloads it for a Desktop app, and keeps its id and
// secret as one secret. A web client is refused: Beacon's redirect is a loopback address.
func OAuthClient(data []byte) (Secret, error) {
	var f struct {
		Installed *oauthClient `json:"installed"`
	}
	if err := json.Unmarshal(data, &f); err != nil || f.Installed == nil {
		return Secret{}, ErrOAuthClient
	}
	c := oauthClient{ID: strings.TrimSpace(f.Installed.ID), Secret: strings.TrimSpace(f.Installed.Secret)}
	if c.ID == "" || c.Secret == "" || strings.ContainsAny(c.ID, " \t\r\n") {
		return Secret{}, ErrOAuthClient
	}
	v, err := json.Marshal(c)
	if err != nil {
		return Secret{}, ErrOAuthClient
	}
	return Secret{value: v}, nil
}

func (s Secret) oauthClient() (oauthClient, error) {
	var c oauthClient
	if err := json.Unmarshal(s.value, &c); err != nil || c.ID == "" || c.Secret == "" {
		return oauthClient{}, ErrOAuthClient
	}
	return c, nil
}

// OAuthClientID returns the public client id of a secret from OAuthClient. The id is not a credential: it is part of
// the consent URL the browser opens.
func (s Secret) OAuthClientID() (string, error) {
	c, err := s.oauthClient()
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

// PKCE returns a new code verifier, as a secret, and its S256 challenge (RFC 7636). The challenge is public.
func PKCE() (Secret, string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Secret{}, "", err
	}
	v := base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(v))
	return Secret{value: []byte(v)}, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// CodeFromRedirect reads the authorisation code from the loopback redirect. state must match the state the consent
// URL carried. An error from the consent screen, a missing code or another state is ErrRedirect.
func CodeFromRedirect(u *url.URL, state string) (Secret, error) {
	q := u.Query()
	got := q.Get("state")
	if state == "" || subtle.ConstantTimeCompare([]byte(got), []byte(state)) != 1 {
		return Secret{}, ErrRedirect
	}
	code := q.Get("code")
	if q.Get("error") != "" || code == "" || strings.ContainsAny(code, "\r\n") {
		return Secret{}, ErrRedirect
	}
	return Secret{value: []byte(code)}, nil
}

// CodeRequest builds the token request that exchanges an authorisation code (RFC 6749 4.1.3, with the PKCE
// verifier).
func CodeRequest(ctx context.Context, tokenURL string, client, code, verifier Secret, redirectURI string) (*http.Request, error) {
	c, err := client.oauthClient()
	if err != nil {
		return nil, err
	}
	if code.Empty() || verifier.Empty() {
		return nil, ErrRedirect
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {string(code.value)},
		"code_verifier": {string(verifier.value)},
		"redirect_uri":  {redirectURI},
		"client_id":     {c.ID},
		"client_secret": {c.Secret},
	}
	return formRequest(ctx, tokenURL, form)
}

// RefreshRequest builds the token request that exchanges a refresh token for an access token (RFC 6749 6).
func RefreshRequest(ctx context.Context, tokenURL string, client, refresh Secret) (*http.Request, error) {
	c, err := client.oauthClient()
	if err != nil {
		return nil, err
	}
	if refresh.Empty() {
		return nil, ErrGrantRefused
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {string(refresh.value)},
		"client_id":     {c.ID},
		"client_secret": {c.Secret},
	}
	return formRequest(ctx, tokenURL, form)
}

func formRequest(ctx context.Context, tokenURL string, form url.Values) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("keychain: the token endpoint URL is invalid")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "beacon")
	return req, nil
}

// Token is a token endpoint's answer. Refresh is empty when the endpoint did not issue one (a refresh grant
// usually does not). Scopes are the scopes granted; they are not credentials.
type Token struct {
	Access    Secret
	Refresh   Secret
	ExpiresIn time.Duration
	Scopes    []string
}

// ReadToken reads a token endpoint's response. A 400 or 401 is ErrGrantRefused; any other failure has fixed text.
// The body is read here so its tokens never pass through a caller.
func ReadToken(resp *http.Response) (Token, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, errors.New("keychain: the token response could not be read")
	}
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		return Token{}, ErrGrantRefused
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return Token{}, errors.New("keychain: the token endpoint failed")
	}
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.AccessToken == "" || strings.ContainsAny(body.AccessToken, " \r\n") {
		return Token{}, errors.New("keychain: the token response could not be read")
	}
	if body.TokenType != "" && !strings.EqualFold(body.TokenType, "bearer") {
		return Token{}, errors.New("keychain: the token is not a bearer token")
	}
	t := Token{Access: Secret{value: []byte(body.AccessToken)}, ExpiresIn: time.Duration(body.ExpiresIn) * time.Second, Scopes: strings.Fields(body.Scope)}
	if r := strings.TrimSpace(body.RefreshToken); r != "" && !strings.ContainsAny(r, "\r\n") {
		t.Refresh = Secret{value: []byte(r)}
	}
	return t, nil
}
