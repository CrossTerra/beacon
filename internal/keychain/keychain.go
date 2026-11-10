// Package keychain is the only code that reads, holds or writes a credential.
//
// A credential is a Secret. Its value never leaves this package except as an HTTP Authorization header set by
// Authorize. Printing, formatting, logging or marshalling a Secret yields "[redacted]".
package keychain

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

// Authorize sets the bearer Authorization header on h.
func (s Secret) Authorize(h http.Header) {
	h.Set("Authorization", "Bearer "+string(s.value))
}

// SourceAccount is the keychain account for one source's credential, under Service.
func SourceAccount(name string) string { return "source:" + name }

// The keychain accounts of the Google grant the Gmail, Calendar and Drive sources share, under Service: the
// person's OAuth client (id and secret) and the refresh token.
const (
	GoogleClientAccount  = "google:client"
	GoogleRefreshAccount = "google:refresh"
)

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
