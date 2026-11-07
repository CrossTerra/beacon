// Package keychain is the only code that reads, holds or writes a credential.
//
// A credential is a Secret. Its value never leaves this package except as an HTTP Authorization header set by
// Authorize. Printing, formatting, logging or marshalling a Secret yields "[redacted]".
package keychain

import (
	"bufio"
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

// ReadSecret reads one line from r as a secret, trimming surrounding space. When r is a terminal, echo is off while
// it reads. An empty line is an error.
func ReadSecret(r io.Reader) (Secret, error) {
	restore := noEcho(r)
	line, err := bufio.NewReader(r).ReadString('\n')
	restore()
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
