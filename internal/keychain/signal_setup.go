package keychain

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/localstate"
	"github.com/CrossTerra/beacon/internal/vault"
)

// SignalAPIKey checks the wire format only. The API must still verify the key and its authority.
func SignalAPIKey(s Secret) bool { return vault.SignalKeyFormat(s.value) }

// StoreSignalPair hands both secrets to the offline vault transaction without exposing their bytes.
// The caller is responsible for verifying the actor/person bindings and for any configuration changes.
func StoreSignalPair(ctx context.Context, dir string, pair vault.SignalPair, app, collector Secret) error {
	if !SignalAPIKey(app) || !SignalAPIKey(collector) || app.Same(collector) {
		return errors.New("invalid Signal credential pair")
	}
	return vault.PutSignalPair(ctx, dir, pair, app.value, collector.value)
}

// CommitSignalPair applies a pair under the already-owned offline lock without exposing secret bytes.
func CommitSignalPair(ctx context.Context, update *vault.SignalPairUpdate, pair vault.SignalPair, app, collector Secret, c vault.SignalPairCommit) (bool, error) {
	return update.Commit(ctx, pair, app.value, collector.value, c)
}

// ReadPreparedSignal opens only the requested slot after both slots and the current private
// preparation, origin and journal attachment have been checked. It never falls back to a single slot.
func ReadPreparedSignal(dir string, pair vault.SignalPair, slot vault.Binding) (Secret, error) {
	p, err := localstate.LoadSignalPreparation(dir)
	if err != nil || p == nil || p.Pair() != pair || p.CheckInstance(dir) != nil {
		return Secret{}, vault.ErrConflict
	}
	value, err := vault.ReadSignalPair(dir, pair, slot)
	if errors.Is(err, os.ErrNotExist) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, err
	}
	return Secret{value: []byte(value)}, nil
}

// ReadSignalSetup takes the /beacon setup code from hidden input. Its bytes never reach a caller.
func ReadSignalSetup(ctx context.Context, r io.Reader) (string, Secret, error) {
	code, err := ReadSecret(ctx, r)
	if err != nil {
		return "", Secret{}, err
	}
	defer code.Clear()
	return parseSignalSetup(code)
}

// ReadSignalPair prompts for the two credentials in order, refusing a code for another origin
// before reading the collector key. Real CLI input must be a terminal with echo disabled;
// a non-file reader is the fixture seam. One reader owns both lines.
func ReadSignalPair(ctx context.Context, r io.Reader, prompts io.Writer, expectedOrigin string) (Secret, Secret, error) {
	if r == nil {
		return Secret{}, Secret{}, errors.New("enter Signal credentials in a terminal")
	}
	if f, ok := r.(*os.File); ok {
		restore, err := hideEcho(f)
		if err != nil {
			return Secret{}, Secret{}, errors.New("enter Signal credentials directly in a terminal; hidden input is required")
		}
		defer restore()
	}
	in := bufio.NewReader(r)
	fmt.Fprint(prompts, "Approval-app setup code from Signal /beacon (input hidden): ")
	code, err := readSecretLine(ctx, in)
	fmt.Fprintln(prompts)
	if err != nil {
		return Secret{}, Secret{}, errors.New("no Signal approval-app setup code read")
	}
	defer code.Clear()
	origin, app, err := parseSignalSetup(code)
	if err != nil {
		return Secret{}, Secret{}, err
	}
	if origin != expectedOrigin {
		app.Clear()
		return Secret{}, Secret{}, errors.New("the approval-app code names a different Signal origin; no credentials were sent")
	}
	fmt.Fprint(prompts, "Separate Signal collector key (input hidden): ")
	collector, err := readSecretLine(ctx, in)
	fmt.Fprintln(prompts)
	if err != nil {
		app.Clear()
		return Secret{}, Secret{}, errors.New("no Signal collector key read")
	}
	return app, collector, nil
}

func parseSignalSetup(code Secret) (string, Secret, error) {
	const prefix = "beacon-setup:v1:"
	bad := errors.New("expected the new setup code from Signal's /beacon page")
	b, ok := bytes.CutPrefix(code.value, []byte(prefix))
	if !ok || len(b) > 8192 {
		return "", Secret{}, bad
	}
	decoded := make([]byte, base64.RawURLEncoding.DecodedLen(len(b)))
	defer clear(decoded)
	n, err := base64.RawURLEncoding.Strict().Decode(decoded, bytes.TrimRight(b, "="))
	if err != nil {
		return "", Secret{}, bad
	}
	if fields, err := approval.ParseObject(decoded[:n]); err != nil || len(fields) != 2 {
		return "", Secret{}, bad
	}
	var v struct{ Origin, Key string }
	if json.Unmarshal(decoded[:n], &v) != nil || v.Origin == "" {
		return "", Secret{}, bad
	}
	secret := Secret{value: []byte(v.Key)}
	if !ActionToken(secret) || !bytes.HasPrefix(secret.value, []byte("sig_")) || len(secret.value) == 4 {
		secret.Clear()
		return "", Secret{}, bad
	}
	return v.Origin, secret, nil
}
