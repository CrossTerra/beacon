package actionexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
)

// SignalCheckTarget comes from the operator and local configuration, never from a credential.
type SignalCheckTarget struct {
	Origin, PersonID, Handle, ApprovalActor, CollectorActor string
	RelayIdentities                                         []string // configured own-activity identities the collector must be allowed to relay
}

// PinPerson adds an independent selector. An ID and a handle can coexist; Signal must confirm both.
func (t *SignalCheckTarget) PinPerson(selector string) error {
	var field *string
	if handle, ok := strings.CutPrefix(selector, "@"); ok && approval.ValidSignalHandle(handle) {
		field, selector = &t.Handle, handle
	} else if approval.ValidID(selector, "act") {
		field = &t.PersonID
	} else {
		return errors.New("specify the intended Signal person as @handle or an act_ actor ID")
	}
	if *field != "" && *field != selector {
		return errors.New("the intended Signal person conflicts with an existing pin")
	}
	*field = selector
	return nil
}

// SignalVerificationEgress is shared by the guided check and beacon egress. In this flow
// localhost is the review origin, not an exception to TLS for the Signal credentials.
func SignalVerificationEgress(origin string) (egress.Host, error) {
	host, err := approval.RecordEgress(origin)
	if err != nil || !strings.HasPrefix(origin, "https://") {
		return egress.Host{}, errors.New("specify a trusted Signal HTTPS origin with no path, query, or credentials")
	}
	host.Purpose = "Signal onboarding verification: GET /api/me, /api/actors/{id}, and /api/api-keys"
	return host, nil
}

// VerifySignalPair uses only documented reads at the independently pinned origin. It does not open
// local credentials or save anything, and does not use the collector key to test a write.
func VerifySignalPair(ctx context.Context, t SignalCheckTarget, app, collector keychain.Secret, transport func([]string) http.RoundTripper) error {
	_, err := CheckSignalPair(ctx, t, app, collector, transport)
	return err
}

// VerifiedSignal contains only the verified nonsecret identity bindings.
type VerifiedSignal struct {
	Person, Handle, ApprovalActor, CollectorActor string
}

func CheckSignalPair(ctx context.Context, t SignalCheckTarget, app, collector keychain.Secret, transport func([]string) http.RoundTripper) (VerifiedSignal, error) {
	host, err := SignalVerificationEgress(t.Origin)
	if err != nil {
		return VerifiedSignal{}, err
	}
	if t.PersonID == "" && t.Handle == "" {
		return VerifiedSignal{}, errors.New("the intended Signal person must be pinned before verification")
	}
	if !keychain.SignalAPIKey(app) || !keychain.SignalAPIKey(collector) || app.Same(collector) {
		return VerifiedSignal{}, errors.New("provide two distinct, well-formed Signal API keys; no credentials were sent")
	}
	v := newSignalCheck(t.Origin, host, transport)
	defer v.client.CloseIdleConnections()
	approvalApp, err := v.integration(ctx, app, t.PersonID, t.ApprovalActor, true)
	if err != nil {
		return VerifiedSignal{}, fmt.Errorf("approval app: %w", err)
	}
	var person signalCheckActor
	if err := v.get(ctx, app, "/api/actors/"+approvalApp.Actor.WorksFor, &person); err != nil {
		return VerifiedSignal{}, fmt.Errorf("intended person: %w", err)
	}
	if person.ID != approvalApp.Actor.WorksFor || person.Type != "human" || string(person.Deactivated) != "null" ||
		!approval.ValidSignalHandle(person.Handle) || t.Handle != "" && person.Handle != t.Handle {
		return VerifiedSignal{}, errors.New("the approval app does not work for the intended active human")
	}
	relay, err := v.integration(ctx, collector, person.ID, t.CollectorActor, false)
	if err != nil {
		return VerifiedSignal{}, fmt.Errorf("collector: %w", err)
	}
	if relay.Actor.ID == approvalApp.Actor.ID {
		return VerifiedSignal{}, errors.New("the approval app and collector must be separate Signal integrations")
	}
	if err := v.relayGrant(ctx, collector, relay, person.Identities, t.RelayIdentities); err != nil {
		return VerifiedSignal{}, err
	}
	return VerifiedSignal{person.ID, person.Handle, approvalApp.Actor.ID, relay.Actor.ID}, nil
}

type signalCheck struct {
	origin string
	client *http.Client
}

func newSignalCheck(origin string, host egress.Host, transport func([]string) http.RoundTripper) signalCheck {
	if transport == nil {
		transport = egress.Transport
	}
	allowed := []string{host.Address}
	return signalCheck{origin, &http.Client{Transport: egress.Guard(allowed, transport(allowed)), Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type signalCheckActor struct {
	ID, Type, Handle string
	WorksFor         string          `json:"works_for"`
	ApprovalApp      *bool           `json:"approval_app"`
	Deactivated      json.RawMessage `json:"deactivated_at"`
	Directory        *bool
	Executes         []string
	Reconciles       []string
	Identities       []string
}

type signalCaller struct {
	Actor   signalCheckActor
	Scopes  []string
	Curator *bool
	Key     struct{ ID string }
}

func (v signalCheck) integration(ctx context.Context, secret keychain.Secret, person, actor string, isApp bool) (signalCaller, error) {
	var me signalCaller
	if err := v.get(ctx, secret, "/api/me", &me); err != nil {
		return me, err
	}
	want := []string{"read", "write"}
	if !isApp {
		want = append(want, "relay")
	}
	m := me.Actor
	if m.Type != "integration" || !approval.ValidID(m.ID, "act") || !approval.ValidID(m.WorksFor, "act") || m.ID == m.WorksFor ||
		m.ApprovalApp == nil || *m.ApprovalApp != isApp || me.Curator == nil || *me.Curator || !exactScopes(me.Scopes, want) {
		return me, errors.New("the Signal response did not confirm the required integration role and exact scopes")
	}
	if person != "" && person != m.WorksFor || actor != "" && actor != m.ID {
		return me, errors.New("the integration does not match the intended person or existing actor pin")
	}
	var detail signalCheckActor
	if err := v.get(ctx, secret, "/api/actors/"+m.ID, &detail); err != nil {
		return me, err
	}
	if detail.ID != m.ID || detail.Type != m.Type || detail.WorksFor != m.WorksFor || string(detail.Deactivated) != "null" ||
		detail.ApprovalApp == nil || *detail.ApprovalApp != isApp || detail.Directory == nil || *detail.Directory ||
		detail.Executes == nil || len(detail.Executes) != 0 || detail.Reconciles == nil || len(detail.Reconciles) != 0 {
		return me, errors.New("the Signal actor details did not confirm an active integration without other capabilities")
	}
	return me, nil
}

func exactScopes(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, scope := range want {
		if !slices.Contains(got, scope) {
			return false
		}
	}
	return true
}

func (v signalCheck) relayGrant(ctx context.Context, key keychain.Secret, me signalCaller, attached, required []string) error {
	if !approval.ValidID(me.Key.ID, "key") {
		return errors.New("the Signal response did not identify the collector key for grant verification")
	}
	var page struct {
		Data []struct {
			ID               string
			ActorID          string `json:"actor_id"`
			Scopes           []string
			IdentityPrefixes []string        `json:"identity_prefixes"`
			Revoked          json.RawMessage `json:"revoked_at"`
			SignedIn         *bool           `json:"signed_in"`
		}
	}
	if err := v.get(ctx, key, "/api/api-keys?limit=100", &page); err != nil {
		return fmt.Errorf("collector grant: %w", err)
	}
	found := false
	for _, k := range page.Data {
		if k.ID != me.Key.ID {
			continue
		}
		if found || k.ActorID != me.Actor.ID || !exactScopes(k.Scopes, me.Scopes) || string(k.Revoked) != "null" || k.SignedIn == nil || *k.SignedIn {
			return errors.New("the Signal key list did not confirm the collector key's specific active grant")
		}
		if !narrowRelay(k.IdentityPrefixes, attached) {
			return errors.New("the collector relay must name only exact supported identities attached to the intended person; no wildcards")
		}
		for _, identity := range required {
			// Signal lowercases email addresses before matching. Provider account addresses
			// are ASCII; do not guess at JavaScript's Unicode case conversion otherwise.
			if strings.HasPrefix(identity, "email:") && !strings.ContainsFunc(identity, func(r rune) bool { return r > unicode.MaxASCII }) {
				identity = strings.ToLower(identity)
			}
			if !slices.Contains(k.IdentityPrefixes, identity) {
				return errors.New("the collector relay grant does not cover all configured own-activity identities")
			}
		}
		found = true
	}
	if !found {
		return errors.New("the collector key's grant was not found in the first 100 Signal keys for its actor")
	}
	return nil
}

func narrowRelay(grant, identities []string) bool {
	if len(grant) == 0 || len(grant) > 20 {
		return false
	}
	for i, identity := range grant {
		if len(identity) > 200 || strings.ContainsRune(identity, '*') || strings.ContainsFunc(identity, unicode.IsSpace) ||
			strings.ContainsFunc(identity, unicode.IsControl) || !slices.Contains(identities, identity) || slices.Contains(grant[:i], identity) {
			return false
		}
		supported := false
		for _, stem := range []string{"github:user:", "jira:account:", "slack:user:", "email:"} {
			if name, ok := strings.CutPrefix(identity, stem); ok && name != "" {
				supported = true
				break
			}
		}
		if !supported {
			return false
		}
	}
	return true
}

func (v signalCheck) get(ctx context.Context, secret keychain.Secret, path string, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, v.origin+path, nil)
	if err != nil {
		return errors.New("invalid Signal verification request")
	}
	r.Header.Set("Accept", "application/json")
	secret.Authorize(r.Header)
	res, err := v.client.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("could not reach the pinned Signal origin")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("verification read refused by Signal (HTTP %d)", res.StatusCode)
	}
	const limit = 1 << 20
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	defer clear(b)
	if err != nil || len(b) > limit || json.Unmarshal(b, out) != nil {
		return errors.New("invalid verification response from Signal")
	}
	return nil
}
