package keychain

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const clientFile = `{"installed":{"client_id":"id-1.apps.googleusercontent.com","client_secret":"GOCSPX-secret","redirect_uris":["http://localhost"]}}`

func TestOAuthClient(t *testing.T) {
	c, err := OAuthClient([]byte(clientFile))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := c.OAuthClientID(); err != nil || id != "id-1.apps.googleusercontent.com" {
		t.Fatalf("%q %v", id, err)
	}
	for _, printed := range []string{fmt.Sprint(c), fmt.Sprintf("%+v %#v %s", c, c, c)} {
		if strings.Contains(printed, "GOCSPX") {
			t.Fatal("the client secret printed")
		}
	}
	data, _ := json.Marshal(c)
	if strings.Contains(string(data), "GOCSPX") {
		t.Fatal("the client secret marshalled")
	}
	for _, bad := range []string{
		`{"web":{"client_id":"a","client_secret":"b"}}`,
		`{"installed":{"client_id":"a"}}`,
		`not json`,
	} {
		if _, err := OAuthClient([]byte(bad)); !errors.Is(err, ErrOAuthClient) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func TestPKCE(t *testing.T) {
	v, challenge, err := PKCE()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(v.value)
	if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || len(v.value) < 43 {
		t.Fatal("challenge is not S256 of the verifier")
	}
	v2, _, _ := PKCE()
	if string(v2.value) == string(v.value) {
		t.Fatal("verifiers repeat")
	}
}

func TestCodeFromRedirect(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:5000/?code=abc&state=s1&scope=x")
	code, err := CodeFromRedirect(u, "s1")
	if err != nil || string(code.value) != "abc" {
		t.Fatal(err)
	}
	for _, raw := range []string{"/?code=abc&state=s2", "/?error=access_denied&state=s1", "/?state=s1", "/?code=abc"} {
		u, _ := url.Parse(raw)
		if _, err := CodeFromRedirect(u, "s1"); !errors.Is(err, ErrRedirect) {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestTokenRequests(t *testing.T) {
	client, _ := OAuthClient([]byte(clientFile))
	refresh, _ := FromString("1//refresh")
	req, err := RefreshRequest(context.Background(), "https://oauth2.googleapis.com/token", client, refresh)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(body))
	if req.Method != http.MethodPost || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "1//refresh" || form.Get("client_secret") != "GOCSPX-secret" {
		t.Fatalf("%s %v", req.Method, form)
	}
	if req.URL.RawQuery != "" {
		t.Fatal("credentials in the URL")
	}
	code, _ := FromString("c0de")
	verifier, _, _ := PKCE()
	req, err = CodeRequest(context.Background(), "https://oauth2.googleapis.com/token", client, code, verifier, "http://127.0.0.1:1/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(req.Body)
	form, _ = url.ParseQuery(string(body))
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "c0de" || form.Get("code_verifier") != string(verifier.value) || form.Get("redirect_uri") != "http://127.0.0.1:1/" {
		t.Fatalf("%v", form)
	}
}

func TestReadToken(t *testing.T) {
	resp := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
	}
	tok, err := ReadToken(resp(200, `{"access_token":"ya29.a","refresh_token":"1//r","expires_in":3599,"scope":"s1 s2","token_type":"Bearer"}`))
	if err != nil || string(tok.Access.value) != "ya29.a" || string(tok.Refresh.value) != "1//r" || len(tok.Scopes) != 2 || tok.ExpiresIn.Seconds() != 3599 {
		t.Fatalf("%v %v", tok, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", tok), "ya29") {
		t.Fatal("token printed")
	}
	if _, err := ReadToken(resp(400, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)); !errors.Is(err, ErrGrantRefused) {
		t.Fatal(err)
	}
	if _, err := ReadToken(resp(500, `boom 1//r`)); err == nil || strings.Contains(err.Error(), "1//r") {
		t.Fatal(err)
	}
	if _, err := ReadToken(resp(200, `{"access_token":"x","token_type":"mac"}`)); err == nil {
		t.Fatal("a non-bearer token was accepted")
	}
}
