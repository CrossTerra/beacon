package keychain

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestReadSignalSetupRetainsLegacyV1Compatibility(t *testing.T) {
	const origin = "https://signal.fixture.test"
	const legacy = "sig_fixture-legacy"
	code := "beacon-setup:v1:" + base64.RawURLEncoding.EncodeToString([]byte(`{"origin":"`+origin+`","key":"`+legacy+`"}`))
	gotOrigin, key, err := ReadSignalSetup(t.Context(), strings.NewReader(code))
	defer key.Clear()
	if err != nil || gotOrigin != origin || string(key.value) != legacy || SignalAPIKey(key) {
		t.Fatal("legacy setup reader did not preserve its v1 compatibility")
	}
}
