package actionexec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/cutover"
	"github.com/CrossTerra/beacon/internal/egress"
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
	if old == nil && s.Signal != nil {
		old = &cutover.SignalActor{Origin: s.Signal.Origin, Actor: s.Signal.Actor, Person: s.Signal.Person}
	}
	if old != nil && old.Origin != origin {
		return errors.New("the code is for a different Signal origin")
	}
	if transport == nil {
		transport = egress.Transport
	}
	allowed := []string{host.Address}
	c := &http.Client{Transport: egress.Guard(allowed, transport(allowed)), Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer c.CloseIdleConnections()
	get := func(path string, out any) error {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+path, nil)
		if err != nil {
			return errors.New("invalid Signal setup origin")
		}
		key.Authorize(r.Header)
		resp, err := c.Do(r)
		if err != nil {
			return errors.New("could not reach Signal to verify the approval-app key")
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil || resp.StatusCode != http.StatusOK || len(b) > 1<<20 || json.Unmarshal(b, out) != nil {
			return errors.New("could not verify the approval-app key with Signal")
		}
		return nil
	}
	type actor struct {
		ID, Type, Handle string
		Deactivated      json.RawMessage `json:"deactivated_at"`
		ApprovalApp      bool            `json:"approval_app"`
		WorksFor         string          `json:"works_for"`
	}
	var me struct {
		Actor  actor
		Scopes []string
	}
	if err := get("/api/me", &me); err != nil {
		return err
	}
	slices.Sort(me.Scopes)
	if me.Actor.Type != "integration" || !approval.ValidID(me.Actor.ID, "act") || !slices.Equal(me.Scopes, []string{"read", "write"}) {
		return errors.New("expected an integration approval-app key with exactly read and write scopes")
	}
	if old != nil && old.Actor != me.Actor.ID {
		return errors.New("rotate the key on the same Signal app actor; this code names another one")
	}
	var app, person actor
	if err := get("/api/actors/"+me.Actor.ID, &app); err != nil {
		return err
	}
	if app.ID != me.Actor.ID || app.Type != "integration" || !app.ApprovalApp || string(app.Deactivated) != "null" ||
		!approval.ValidID(app.WorksFor, "act") || app.WorksFor == app.ID || old != nil && old.Person != app.WorksFor {
		return errors.New("the approval app does not work for the required active person")
	}
	if err := get("/api/actors/"+app.WorksFor, &person); err != nil {
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
