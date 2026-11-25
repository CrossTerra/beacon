package actionexec

import (
	"context"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
)

// SignalSettings is explicit local opt-in to sending the journal, not a routing hint from a proposal.
type SignalSettings struct {
	Origin string `json:"origin"`
	Actor  string `json:"actor_id"`
	Person string `json:"person_id"`
	Handle string `json:"handle"`
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
		key, err := keychain.ReadRecordKey(dir, s.Origin, s.Actor)
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
