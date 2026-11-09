package keychain

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const value = "sig_supersecretvalue"

func secret() Secret { return Secret{value: []byte(value)} }

func TestSecretNeverPrints(t *testing.T) {
	s := secret()
	type wrapper struct {
		Key Secret
	}
	outs := []string{
		fmt.Sprint(s), fmt.Sprintf("%v %s %q %x %#v %+v", s, s, s, s, s, s), fmt.Sprintf("%v %+v %#v", wrapper{s}, wrapper{s}, &wrapper{s}),
	}
	j, _ := json.Marshal(wrapper{s})
	outs = append(outs, string(j))
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "key", s)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "key", s)
	outs = append(outs, buf.String())
	for _, o := range outs {
		if strings.Contains(o, value) || strings.Contains(o, fmt.Sprintf("%x", value)) {
			t.Fatalf("secret printed: %s", o)
		}
	}
}

func TestAuthorize(t *testing.T) {
	h := http.Header{}
	secret().Authorize(h)
	if h.Get("Authorization") != "Bearer "+value {
		t.Fatalf("header %q", h.Get("Authorization"))
	}
}

func TestAuthorizeBasic(t *testing.T) {
	s, err := Pair("winton@example.com", value)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	if err := s.AuthorizeBasic(h); err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("winton@example.com:"+value))
	if h.Get("Authorization") != want {
		t.Fatal("basic header")
	}
	if strings.Contains(s.String(), value) || strings.Contains(fmt.Sprint(s), "winton@example.com") {
		t.Fatal("pair printed")
	}
	if _, err := Pair("", value); err == nil {
		t.Fatal("empty email")
	}
	if err := secret().AuthorizeBasic(http.Header{}); err == nil {
		t.Fatal("bearer secret used as basic")
	}
}

func TestReadSecret(t *testing.T) {
	s, err := ReadSecret(context.Background(), strings.NewReader("  "+value+"  \nmore"))
	if err != nil || string(s.value) != value {
		t.Fatalf("%v", err)
	}
	if _, err := ReadSecret(context.Background(), strings.NewReader(value)); err != nil {
		t.Fatalf("no newline: %v", err)
	}
	for _, in := range []string{"", "\n", "   \n"} {
		if _, err := ReadSecret(context.Background(), strings.NewReader(in)); err == nil {
			t.Fatalf("read %q as a secret", in)
		}
	}
}

// An interrupt while the hidden prompt waits ends the read at once and restores echo first, so the terminal is
// never left with echo off.
func TestReadSecretInterrupted(t *testing.T) {
	restored := make(chan struct{}, 1)
	echoOff = func(io.Reader) func() { return func() { restored <- struct{}{} } }
	defer func() { echoOff = noEcho }()

	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := ReadSecret(ctx, r)
		got <- err
	}()
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadSecret kept waiting for input after the interrupt")
	}
	select {
	case <-restored:
	default:
		t.Fatal("echo was not restored")
	}
	// A line read normally restores echo too.
	if _, err := ReadSecret(context.Background(), strings.NewReader(value+"\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restored:
	default:
		t.Fatal("echo was not restored after a normal read")
	}
}

func TestMemory(t *testing.T) {
	m := NewMemory()
	if _, err := m.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := m.Set("a", secret()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get("a")
	if string(got.value) != value {
		t.Fatal("round trip")
	}
	if err := m.Set("a", Secret{}); err == nil {
		t.Fatal("stored an empty secret")
	}
	if err := m.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// TestSystem writes, reads and deletes an item in the login keychain. It runs only with BEACON_TEST_KEYCHAIN=1,
// because the keychain may ask for permission.
func TestSystem(t *testing.T) {
	if os.Getenv("BEACON_TEST_KEYCHAIN") != "1" {
		t.Skip("set BEACON_TEST_KEYCHAIN=1 to use the login keychain")
	}
	store, err := NewSystem()
	if err != nil {
		t.Fatal(err)
	}
	account := "beacon-test-" + strings.ReplaceAll(t.Name(), "/", "-")
	_ = store.Delete(account)
	if _, err := store.Get(account); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before: %v", err)
	}
	if err := store.Set(account, secret()); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(account, Secret{value: []byte(value + "2")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.Get(account)
	if err != nil || string(got.value) != value+"2" {
		t.Fatalf("get: %v", err)
	}
	if err := store.Delete(account); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(account); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after: %v", err)
	}
}
