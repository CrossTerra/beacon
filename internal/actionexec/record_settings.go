package actionexec

import (
	"context"
	"errors"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/localstate"
	"github.com/CrossTerra/beacon/internal/vault"
)

// SignalSettings is explicit local opt-in to sending the journal, not a routing hint from a proposal.
type SignalSettings struct {
	Origin   string `json:"origin"`
	Actor    string `json:"actor_id"`
	Person   string `json:"person_id"`
	Handle   string `json:"handle"`
	prepared *localstate.SignalPreparation
}

func (s SignalSettings) identity() (approval.SignalIdentity, error) {
	who := approval.SignalIdentity{Person: s.Person, Handle: s.Handle}
	if !who.Valid() || !approval.ValidID(s.Actor, "act") || s.Actor == s.Person {
		return approval.SignalIdentity{}, errSettings
	}
	if _, err := approval.RecordEgress(s.Origin); err != nil {
		return approval.SignalIdentity{}, errSettings
	}
	return who, nil
}

func (s SignalSettings) binding() vault.Binding {
	return vault.SignalRecordBinding(s.Origin, s.Actor)
}

func (s SignalSettings) CollectorActor() string {
	if s.prepared == nil {
		return ""
	}
	return s.prepared.CollectorActor
}

func (s *SignalSettings) Paired() bool { return s != nil && s.prepared != nil }

// CheckActivation runs before service replay so interrupted preparation cannot start accepting
// unowned actions. Legacy settings retain their existing delivery behavior.
func (s *SignalSettings) CheckActivation(dir string) error {
	if !s.Paired() {
		return nil
	}
	key, err := s.readKey(dir)
	key.Clear()
	if err != nil {
		return errors.New("paired Signal activation is unavailable; keep the service stopped and check offline setup")
	}
	return nil
}

func (s *SignalSettings) CheckJournal(id string) error {
	if s != nil && s.prepared != nil && s.prepared.Journal != id {
		return errors.New("paired Signal activation belongs to a different journal")
	}
	return nil
}

func (s SignalSettings) readKey(dir string) (keychain.Secret, error) {
	if s.prepared != nil {
		if s.Origin != s.prepared.Origin || s.Actor != s.prepared.ApprovalActor || s.Person != s.prepared.Person || s.Handle != s.prepared.Handle {
			return keychain.Secret{}, errSettings
		}
		return keychain.ReadPreparedSignal(dir, s.prepared.Pair(), s.binding())
	}
	return keychain.ReadRecordKey(dir, s.Origin, s.Actor)
}

// RecordConnection reads the bound app key anew for each outbox drain. Nothing from the run or
// the approval's PRF key is involved. Signal authorizes who may write records for the person.
func (s SignalSettings) RecordConnection(dir string) approval.ConnectRecords {
	return func(ctx context.Context) (approval.SignalIdentity, approval.RecordTransport, error) {
		who, err := s.identity()
		if err != nil {
			return who, nil, err
		}
		if err := ctx.Err(); err != nil {
			return who, nil, err
		}
		key, err := s.readKey(dir)
		if err != nil {
			return who, nil, err
		}
		records, err := approval.NewSignalRecords(s.Origin, key.Authorize)
		if err != nil {
			key.Clear()
			return who, nil, err
		}
		return who, &recordConnection{records, key}, nil
	}
}

type recordConnection struct {
	*approval.SignalRecords
	key keychain.Secret
}

func (c *recordConnection) Close() {
	c.SignalRecords.Close()
	c.key.Clear()
}
