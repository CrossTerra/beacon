package actionexec

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/cutover"
	"github.com/CrossTerra/beacon/internal/keychain"
)

func TestConnectRotatedSignalKeyRequiresTheArchivedActor(t *testing.T) {
	const appID = "act_01m6a6ft00e40r2081040g2082"
	const person = "act_01m6a6ft00e40r2081040g2081"
	const fixtureKey = "sig_fixture-rotated"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fixtureKey || r.Method != http.MethodGet {
			t.Error("unexpected setup request")
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/api/me":
			fmt.Fprintf(w, `{"actor":{"id":%q,"type":"integration"},"scopes":["write","read"]}`, appID)
		case "/api/actors/" + appID:
			fmt.Fprintf(w, `{"id":%q,"type":"integration","works_for":%q,"deactivated_at":null,"approval_app":true}`, appID, person)
		case "/api/actors/" + person:
			fmt.Fprintf(w, `{"id":%q,"type":"human","handle":"fixture","deactivated_at":null}`, person)
		default:
			t.Error("request to an unexpected setup endpoint")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir, err := os.MkdirTemp("", "beacon-record-setup-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	lock, err := approval.LockInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer approval.CloseInstance(dir, lock)
	key, err := keychain.FromString(fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Clear()
	archived := cutover.SignalActor{Origin: server.URL, Actor: "act_01m6a6ft00e40r2081040g2083", Person: person}
	if err := RegisterSignal(t.Context(), dir, server.URL, key, &archived, nil); err == nil {
		t.Fatal("accepted a different app actor")
	}
	archived.Actor = appID
	if err := RegisterSignal(t.Context(), dir, server.URL, key, &archived, nil); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadSettings(dir)
	if err != nil || settings.Signal == nil || settings.Signal.Person != person || settings.Signal.Actor != appID {
		t.Fatalf("settings: %+v %v", settings, err)
	}
	saved, err := keychain.ReadBound(dir, settings.Signal.binding())
	if err != nil || !saved.Same(key) {
		t.Fatal("key not present in the unattended binding")
	}
}
