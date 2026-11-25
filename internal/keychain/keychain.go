// Package keychain provides redacting credentials and their vault bindings. Its name is historical;
// only the explicit one-time collector import reads legacy Keychain items.
//
// A credential is a Secret. Its value never leaves this package except as an HTTP Authorization header set by
// Authorize. Printing, formatting, logging or marshalling a Secret yields "[redacted]".
package keychain

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Service is the keychain service name every Beacon item is stored under.
const Service = "beacon"

// ErrNotFound is returned when no item exists for the account.
var ErrNotFound = errors.New("keychain: item not found")

// Secret holds a credential. The zero value is empty.
type Secret struct {
	value []byte
}

const redacted = "[redacted]"

// Empty reports whether the secret holds nothing.
func (s Secret) Empty() bool { return len(s.value) == 0 }

// Same reports whether two nonempty credentials have the same bytes without exposing them.
func (s Secret) Same(other Secret) bool {
	return !s.Empty() && subtle.ConstantTimeCompare(s.value, other.value) == 1
}

// Authorize sets the bearer Authorization header on h.
func (s Secret) Authorize(h http.Header) {
	h.Set("Authorization", "Bearer "+string(s.value))
}

// SourceAccount is the keychain account for one source's credential, under Service.
func SourceAccount(name string) string { return "source:" + name }

// GoogleClientAccount is the keychain account, under Service, of the OAuth client (id and secret) the Google sources
// for one Google account use.
func GoogleClientAccount(email string) string { return "google:" + email + ":client" }

// GoogleRefreshAccount is the keychain account, under Service, of the refresh token for one Google account.
func GoogleRefreshAccount(email string) string { return "google:" + email + ":refresh" }

// SlackAccount is the keychain account, under Service, of the Slack user token for one user in one workspace.
func SlackAccount(workspace, user string) string { return "slack:" + workspace + ":" + user }

// SlackUserToken checks that s is a Slack user token (xoxp-). A bot or app token cannot search, and is refused
// before it is stored.
func SlackUserToken(s Secret) error {
	if !strings.HasPrefix(string(s.value), "xoxp-") || strings.ContainsAny(string(s.value), " \t\r\n") {
		return errors.New("keychain: that is not a Slack user token (xoxp-…); copy the User OAuth Token from the app's OAuth & Permissions page")
	}
	return nil
}

// FromString wraps a supplied value as a Secret. Setup commands use the credential readers.
func FromString(s string) (Secret, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return Secret{}, errors.New("keychain: no secret given")
	}
	return Secret{value: []byte(s)}, nil
}

// PairWith stores an email and a token secret as one basic-auth secret. The token's bytes are read only here.
func PairWith(email string, token Secret) (Secret, error) {
	return Pair(email, string(token.value))
}

// Pair stores an email and a token as one secret for HTTP basic auth. Neither value leaves this package except as
// the Authorization header AuthorizeBasic sets.
func Pair(email, token string) (Secret, error) {
	email = strings.TrimSpace(email)
	token = strings.TrimSpace(token)
	if email == "" || token == "" || strings.ContainsAny(email, "\r\n") || strings.ContainsAny(token, "\r\n") {
		return Secret{}, errors.New("keychain: email and token are required")
	}
	return Secret{value: []byte(email + "\n" + token)}, nil
}

// AuthorizeBasic sets a basic Authorization header from a secret stored by Pair.
func (s Secret) AuthorizeBasic(h http.Header) error {
	email, token, err := basicParts(string(s.value))
	if err != nil {
		return err
	}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(email+":"+token)))
	return nil
}

func basicParts(s string) (email, token string, err error) {
	email, token, ok := strings.Cut(s, "\n")
	if !ok || strings.TrimSpace(email) == "" || strings.TrimSpace(token) == "" ||
		strings.ContainsAny(email, ":\r\x00") || strings.ContainsAny(token, "\r\n\x00") {
		return "", "", errors.New("keychain: credential is not an email and a token")
	}
	return email, token, nil
}

// String implements fmt.Stringer without revealing the value.
func (s Secret) String() string { return redacted }

// GoString implements fmt.GoStringer without revealing the value.
func (s Secret) GoString() string { return redacted }

// Format implements fmt.Formatter, so every verb prints the placeholder.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON implements json.Marshaler without revealing the value.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler without revealing the value.
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Store keeps secrets by account under Service.
type Store interface {
	Get(account string) (Secret, error)
	Set(account string, s Secret) error
	Delete(account string) error
}

// Memory is an in-memory Store for tests.
type Memory struct {
	mu    sync.Mutex
	items map[string][]byte
	reads int
}

// Reads is how many values Get has read, so a test can show that listing reads none.
func (m *Memory) Reads() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reads
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{items: map[string][]byte{}} }

// Put stores value under account. It is how a test gives the store a credential.
func (m *Memory) Put(account, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[account] = []byte(value)
}

// Contains reports whether account holds value. Tests use it to check a secret was stored without printing it.
func (m *Memory) Contains(account, value string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return string(m.items[account]) == value
}

// Accounts implements Lister.
func (m *Memory) Accounts() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.items))
	for a := range m.items {
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

// Get implements Store.
func (m *Memory) Get(account string) (Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	v, ok := m.items[account]
	if !ok {
		return Secret{}, ErrNotFound
	}
	return Secret{value: append([]byte(nil), v...)}, nil
}

// Set implements Store.
func (m *Memory) Set(account string, s Secret) error {
	if s.Empty() {
		return errors.New("keychain: empty secret")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[account] = append([]byte(nil), s.value...)
	return nil
}

// Delete implements Store.
func (m *Memory) Delete(account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[account]; !ok {
		return ErrNotFound
	}
	delete(m.items, account)
	return nil
}
