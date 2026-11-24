package keychain

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"

	"github.com/CrossTerra/beacon/internal/approval"
)

// ReadSignalSetup takes the /beacon setup code from hidden input. Its bytes never reach a caller.
func ReadSignalSetup(ctx context.Context, r io.Reader) (string, Secret, error) {
	const prefix = "beacon-setup:v1:"
	bad := errors.New("expected the new setup code from Signal's /beacon page")
	code, err := ReadSecret(ctx, r)
	if err != nil {
		return "", Secret{}, err
	}
	defer code.Clear()
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
