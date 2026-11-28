package vault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tobischo/gokeepasslib/v3"
)

func TestSignalPairSealsBothBindingsToOneGeneration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := Create(dir); err != nil {
		t.Fatal(err)
	}
	key, err := readKeyFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	load := func() *gokeepasslib.Database {
		t.Helper()
		body, _, err := readDB(dir)
		if err != nil {
			t.Fatal(err)
		}
		db, err := decode(body, key)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	before := load().Header.FileHeaders.EncryptionIV
	pair := SignalPair{Origin: "https://signal.fixture.test", Person: "act_01m6a6ft00e40r2081040g2081",
		ApprovalActor: "act_01m6a6ft00e40r2081040g2082", CollectorActor: "act_01m6a6ft00e40r2081040g2083",
		CollectorAccount: "signal:signal.fixture.test:443"}
	app, collector := []byte("synthetic-app-password"), []byte("synthetic-collector-password")
	if err := PutSignalPair(t.Context(), dir, pair, app, collector); err != nil {
		t.Fatal(err)
	}
	db := load()
	if bytes.Equal(before, db.Header.FileHeaders.EncryptionIV) {
		t.Fatal("candidate reused the previous generation's outer IV")
	}
	bindings, err := pair.bindings()
	if err != nil {
		t.Fatal(err)
	}
	actors := []string{pair.ApprovalActor, pair.CollectorActor}
	for i, raw := range [][]byte{app, collector} {
		entries := find(beaconGroup(db), bindings[i])
		if len(entries) != 1 || entries[0].GetContent(pairPerson) != pair.Person || entries[0].GetContent(pairActor) != actors[i] {
			t.Fatal("pair metadata does not name the same person and distinct actors")
		}
		p := entries[0].Get("Password")
		if p == nil || !p.Value.Protected.Bool || p.Value.Content == string(raw) {
			t.Fatal("the candidate stored an unprotected password field")
		}
	}
	if info, err := os.Stat(filepath.Join(dir, dbName)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("the published generation is not private")
	}
}

func TestSignalPairRefusesDuplicateOrHiddenSlots(t *testing.T) {
	bindings := [2]Binding{SignalRecordBinding("https://signal.fixture.test", "app"),
		SignalCollectorBinding("https://signal.fixture.test", "collector")}
	for _, tc := range []struct {
		name string
		edit func(*gokeepasslib.Group, *gokeepasslib.Group)
		want error
	}{
		{"duplicate group", func(root, _ *gokeepasslib.Group) {
			root.Groups = append(root.Groups, gokeepasslib.Group{Name: "Beacon"})
		}, ErrAmbiguous},
		{"duplicate slot", func(_, g *gokeepasslib.Group) {
			g.Entries = append(g.Entries, g.Entries[0].Clone())
		}, ErrAmbiguous},
		{"duplicate binding field", func(_, g *gokeepasslib.Group) {
			g.Entries[0].Values = append(g.Entries[0].Values, entryValue("beacon.origin", "https://other.fixture.test"))
		}, ErrAmbiguous},
		{"hidden slot", func(root, g *gokeepasslib.Group) {
			root.Entries, g.Entries = g.Entries, nil
		}, ErrConflict},
		{"wrong service", func(_, g *gokeepasslib.Group) {
			g.Entries[0].Get("beacon.service").Value.Content = "different"
		}, ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := gokeepasslib.NewDatabase()
			root := &db.Content.Root.Groups[0]
			g := beaconGroup(db)
			g.Entries = []gokeepasslib.Entry{newBoundEntry(bindings[0], "fixture-app")}
			tc.edit(root, g)
			if _, _, err := pairSlots(db, bindings, [2][]byte{[]byte("fixture-app"), []byte("fixture-collector")}); !errors.Is(err, tc.want) {
				t.Fatal("ambiguous or misbound slot was accepted")
			}
		})
	}
}
