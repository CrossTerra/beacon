package actionexec

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/cutover"
	"github.com/CrossTerra/beacon/internal/keychain"
)

// RegisterSignal verifies the new /beacon key and pins a cutover to the same app actor and person.
// Caller holds the instance lock. Saving only changes the unattended credential and settings.
func RegisterSignal(ctx context.Context, dir, origin string, key keychain.Secret, old *cutover.SignalActor,
	transport func([]string) http.RoundTripper) error {
	if !approval.HoldsInstance(dir) {
		return approval.ErrLocked
	}
	host, err := approval.RecordEgress(origin)
	if err != nil || !keychain.ActionToken(key) {
		return errors.New("invalid Signal setup code")
	}
	s, err := LoadSettings(dir)
	if err != nil {
		return err
	}
	if s.Signal.Paired() {
		return errors.New("paired Signal setup does not support single-key enrollment or rotation")
	}
	if old == nil && s.Signal != nil {
		old = &cutover.SignalActor{Origin: s.Signal.Origin, Actor: s.Signal.Actor, Person: s.Signal.Person}
	}
	if old != nil && old.Origin != origin {
		return errors.New("the code is for a different Signal origin")
	}
	v := newSignalCheck(origin, host, transport)
	defer v.client.CloseIdleConnections()
	var me signalCaller
	if err := v.get(ctx, key, "/api/me", &me); err != nil {
		return err
	}
	slices.Sort(me.Scopes)
	if me.Actor.Type != "integration" || !approval.ValidID(me.Actor.ID, "act") || !slices.Equal(me.Scopes, []string{"read", "write"}) {
		return errors.New("expected an integration approval-app key with exactly read and write scopes")
	}
	if old != nil && old.Actor != me.Actor.ID {
		return errors.New("rotate the key on the same Signal app actor; this code names another one")
	}
	var app, person signalCheckActor
	if err := v.get(ctx, key, "/api/actors/"+me.Actor.ID, &app); err != nil {
		return err
	}
	if app.ID != me.Actor.ID || app.Type != "integration" || app.ApprovalApp == nil || !*app.ApprovalApp || string(app.Deactivated) != "null" ||
		!approval.ValidID(app.WorksFor, "act") || app.WorksFor == app.ID || old != nil && old.Person != app.WorksFor {
		return errors.New("the approval app does not work for the required active person")
	}
	if err := v.get(ctx, key, "/api/actors/"+app.WorksFor, &person); err != nil {
		return err
	}
	if person.ID != app.WorksFor || person.Type != "human" || string(person.Deactivated) != "null" {
		return errors.New("the approval app does not work for an active person")
	}
	next := SignalSettings{Origin: origin, Actor: app.ID, Person: person.ID, Handle: strings.TrimPrefix(person.Handle, "@")}
	if _, err := next.identity(); err != nil {
		return err
	}
	s.Signal = &next
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := keychain.StoreBound(dir, next.binding(), key); err != nil {
		return err
	}
	return approval.WritePrivate(filepath.Join(dir, settingsName), b)
}
