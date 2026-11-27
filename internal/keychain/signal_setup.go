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
	"regexp"

	"github.com/CrossTerra/beacon/internal/approval"
)

// Match Signal's KEY_TOKEN in src/credentials.ts, without changing the legacy v1 setup reader.
var signalAPIKey = regexp.MustCompile(`^sig_[0-9a-hjkmnp-tv-z]{26}_[A-Za-z0-9_-]{43}$`)

// SignalAPIKey checks the wire format only. The API must still verify the key and its authority.
func SignalAPIKey(s Secret) bool { return signalAPIKey.Match(s.value) }

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
