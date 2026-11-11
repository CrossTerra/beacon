// Package keychain is the only code that reads, holds or writes a credential.
//
// A credential is a Secret. Its value never leaves this package except as an HTTP Authorization header set by
// Authorize. Printing, formatting, logging or marshalling a Secret yields "[redacted]".
package keychain

import (
	"bufio"
	"bytes"
	"context"
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

// ErrAdHocBuild is returned by NewSystem in a build that is not signed with a certificate: go build, go run and test
// binaries are signed ad hoc. Such a build never touches the login keychain.
var ErrAdHocBuild = errors.New("keychain: this beacon is not signed with a certificate (go build, go run and test binaries are ad hoc), so it does not use the login keychain; build with scripts/build-local.sh")

// TestKeychainEnv, set to 1, lets an ad-hoc test binary use the login keychain. Only TestSystem needs it, and only a
// person at the keyboard sets it.
const TestKeychainEnv = "BEACON_TEST_KEYCHAIN"

// Secret holds a credential. The zero value is empty.
type Secret struct {
	value []byte
}

const redacted = "[redacted]"

// Empty reports whether the secret holds nothing.
func (s Secret) Empty() bool { return len(s.value) == 0 }

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

// FromString stores s as a secret. The caller must not log s. This is how `gh auth token` is imported.
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
	email, token, ok := strings.Cut(string(s.value), "\n")
	if !ok || email == "" || token == "" || strings.Contains(token, "\n") {
		return errors.New("keychain: credential is not an email and a token")
	}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(email+":"+token)))
	return nil
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

// Lister is a Store that can list the accounts it holds under Service, without reading their values.
type Lister interface {
	Accounts() ([]string, error)
}

// repairPrefix names the copy Repair keeps of an item while it re-creates it, so an interrupted repair loses nothing.
const repairPrefix = "repair:"

// Repair re-creates every item under Service, so each one has the access a new item gets from this build (Set) and
// none of what earlier builds added to it. It reads each value once, which is the one prompt per item, and writes it
// back. Before it deletes an item it stores a copy under "repair:<account>" and checks the re-created item holds the
// same bytes before it deletes the copy. A repair cut short is finished by the next one: a copy whose item is
// missing is written back first. done is called with each account repaired. No value leaves this package.
func Repair(s Store, done func(account string)) error {
	l, ok := s.(Lister)
	if !ok {
		return errors.New("keychain: this keychain cannot list its items")
	}
	accounts, err := l.Accounts()
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, a := range accounts {
		have[a] = true
	}
	var todo []string
	for _, a := range accounts {
		orig, isCopy := strings.CutPrefix(a, repairPrefix)
		if !isCopy {
			todo = append(todo, a)
			continue
		}
		if !have[orig] {
			v, err := s.Get(a)
			if err != nil {
				return fmt.Errorf("keychain: reading the copy of %s left by an earlier repair: %w", orig, err)
			}
			if err := s.Set(orig, v); err != nil {
				return fmt.Errorf("keychain: restoring %s from the copy left by an earlier repair: %w", orig, err)
			}
			have[orig] = true
			todo = append(todo, orig)
		}
		if err := s.Delete(a); err != nil && !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("keychain: removing the copy of %s left by an earlier repair: %w", orig, err)
		}
	}
	sort.Strings(todo)
	for _, a := range todo {
		if err := repairOne(s, a); err != nil {
			return err
		}
		if done != nil {
			done(a)
		}
	}
	return nil
}

func repairOne(s Store, account string) error {
	v, err := s.Get(account)
	if err != nil {
		return fmt.Errorf("keychain: reading %s: %w", account, err)
	}
	backup := repairPrefix + account
	if err := s.Set(backup, v); err != nil {
		return fmt.Errorf("keychain: keeping a copy of %s: %w", account, err)
	}
	kept := fmt.Sprintf("; its value is kept as %s/%s, and beacon keychain repair restores it", Service, backup)
	if err := s.Delete(account); err != nil {
		return fmt.Errorf("keychain: removing %s to re-create it%s: %w", account, kept, err)
	}
	if err := s.Set(account, v); err != nil {
		return fmt.Errorf("keychain: writing %s back%s: %w", account, kept, err)
	}
	if got, err := s.Get(account); err != nil || !bytes.Equal(got.value, v.value) {
		return fmt.Errorf("keychain: %s did not read back the same%s", account, kept)
	}
	if err := s.Delete(backup); err != nil {
		return fmt.Errorf("keychain: removing the copy of %s: %w", account, err)
	}
	return nil
}

// echoOff turns terminal echo off for r and returns the function that restores it. Tests replace it.
var echoOff = noEcho

// ReadSecret reads one line from r as a secret, trimming surrounding space. When r is a terminal, echo is off while
// it reads, and is restored before ReadSecret returns, also when ctx ends first (an interrupt): then it returns the
// context's error without waiting for the line. An empty line is an error.
func ReadSecret(ctx context.Context, r io.Reader) (Secret, error) {
	restore := echoOff(r)
	defer restore()
	type read struct {
		line string
		err  error
	}
	done := make(chan read, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		done <- read{line, err}
	}()
	var line string
	var err error
	select {
	case <-ctx.Done():
		return Secret{}, ctx.Err()
	case got := <-done:
		line, err = got.line, got.err
	}
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		if errors.Is(err, io.EOF) {
			return Secret{}, errors.New("keychain: no secret given")
		}
		return Secret{}, err
	}
	v := strings.TrimSpace(line)
	if v == "" {
		return Secret{}, errors.New("keychain: no secret given")
	}
	return Secret{value: []byte(v)}, nil
}

// Memory is an in-memory Store for tests.
type Memory struct {
	mu    sync.Mutex
	items map[string][]byte
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
