package vault

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/tobischo/gokeepasslib/v3"
)

// SignalPair names the two slots after an offline caller has verified the actors and their common
// person. CollectorAccount is the slot from the collector config, not the collector's actor ID.
type SignalPair struct {
	Origin, Person, ApprovalActor, CollectorActor, CollectorAccount string
	Activation                                                      [32]byte // digest of the private, nonsecret preparation; zero for the storage-only primitive
}

const (
	pairPerson = "beacon.signal.person"
	pairActor  = "beacon.signal.actor"
	pairCommit = "beacon.signal.commit"
)

var errSignalPair = errors.New("invalid Signal credential pair")

// Match Signal's KEY_TOKEN in src/credentials.ts, not the legacy v1 setup reader's grammar.
var signalKeyFormat = regexp.MustCompile(`^sig_[0-9a-hjkmnp-tv-z]{26}_[A-Za-z0-9_-]{43}$`)

func SignalKeyFormat(key []byte) bool { return signalKeyFormat.Match(key) }

// SignalRecordBinding is used only for approval journal delivery.
func SignalRecordBinding(origin, actor string) Binding {
	return Binding{Service: "signal", Origin: origin, Account: actor, Use: "records"}
}

// SignalCollectorBinding is used only for the collector's Signal credential.
func SignalCollectorBinding(origin, account string) Binding {
	return Binding{Service: "signal-collector", Origin: origin, Account: account, Use: "signal"}
}

func (p SignalPair) bindings() ([2]Binding, error) {
	if _, err := approval.RecordEgress(p.Origin); err != nil || !strings.HasPrefix(p.Origin, "https://") ||
		!approval.ValidID(p.Person, "act") || !approval.ValidID(p.ApprovalActor, "act") || !approval.ValidID(p.CollectorActor, "act") ||
		p.ApprovalActor == p.CollectorActor || p.Person == p.ApprovalActor || p.Person == p.CollectorActor ||
		p.CollectorAccount == "" || len(p.CollectorAccount) > 200 || strings.ContainsFunc(p.CollectorAccount, unicode.IsSpace) ||
		strings.ContainsFunc(p.CollectorAccount, unicode.IsControl) {
		return [2]Binding{}, errSignalPair
	}
	return [2]Binding{SignalRecordBinding(p.Origin, p.ApprovalActor), SignalCollectorBinding(p.Origin, p.CollectorAccount)}, nil
}

// PutSignalPair installs both protected entries in one replacement of an existing unattended
// database. It takes the offline instance lock itself and never creates a database or a key.
// This is a storage primitive, not authorization to attach these credentials to a configuration
// or journal. An exact retry does not rewrite the vault; a partial or different pair is a conflict.
func PutSignalPair(ctx context.Context, dir string, pair SignalPair, appKey, collectorKey []byte) error {
	if pair.Activation != [32]byte{} {
		return errSignalPair
	}
	lock, err := approval.LockInstance(dir)
	if err != nil {
		return err
	}
	defer approval.CloseInstance(dir, lock)
	u, err := openSignalPair(dir)
	if err != nil {
		return err
	}
	defer u.Close()
	_, err = u.commit(ctx, pair, appKey, collectorKey, SignalPairCommit{})
	return err
}

// SignalPairUpdate holds the pre-verification generation. It never rebases onto an edit.
type SignalPairUpdate struct {
	dir  string
	lock *approval.OfflineLock
	key  []byte
	info os.FileInfo
	base [32]byte
	db   *gokeepasslib.Database
}

func BeginSignalPair(lock *approval.OfflineLock) (*SignalPairUpdate, error) {
	if err := lock.Check(); err != nil {
		return nil, err
	}
	u, err := openSignalPair(lock.Dir())
	if err == nil {
		u.lock = lock
	}
	return u, err
}

func openSignalPair(dir string) (*SignalPairUpdate, error) {
	if _, err := approval.CheckPrivate(dir, os.ModeDir|0o700); err != nil {
		return nil, err
	}
	if err := existingVault(dir); !errors.Is(err, ErrExists) {
		if err != nil {
			return nil, err
		}
		return nil, ErrKeyMissing
	}
	key, err := readKeyFile(dir)
	if err != nil {
		return nil, err
	}
	body, info, err := readDB(dir)
	if err != nil {
		clear(key)
		return nil, err
	}
	db, err := decode(body, key)
	if err != nil || db.UnlockProtectedEntries() != nil {
		clear(key)
		return nil, ErrUnreadable
	}
	return &SignalPairUpdate{dir: dir, key: key, info: info, base: sha256.Sum256(body), db: db}, nil
}

func (u *SignalPairUpdate) Close() {
	clear(u.key)
	u.db = nil
}

func (u *SignalPairUpdate) Base() [32]byte { return u.base }

// Existing refuses all Signal slots if no preparation is expected. With a preparation it
// accepts only no slots or the complete pair committed for that preparation.
func (u *SignalPairUpdate) Existing(p *SignalPair) (bool, error) {
	bindings := [2]Binding{SignalRecordBinding("", ""), SignalCollectorBinding("", "")}
	if p != nil {
		var err error
		bindings, err = p.bindings()
		if err != nil {
			return false, err
		}
	}
	_, entries, err := pairSlots(u.db, bindings, [2][]byte{})
	if err != nil || entries == [2]*gokeepasslib.Entry{} {
		return false, err
	}
	if p == nil || !p.sameEntries(entries) {
		return false, ErrConflict
	}
	return true, nil
}

type SignalPairCommit struct {
	// Prepare publishes only inert, nonsecret state after known vault conflicts have been ruled out.
	Prepare func(complete bool) error
	// Check revalidates the preparation and the local state immediately before publication.
	Check func() error
	// Renamed is a fixture seam at the only credential publication boundary.
	Renamed func() error
}

func (u *SignalPairUpdate) Commit(ctx context.Context, pair SignalPair, appKey, collectorKey []byte, c SignalPairCommit) (bool, error) {
	if err := u.lock.Check(); err != nil {
		return false, err
	}
	if pair.Activation == [32]byte{} || c.Prepare == nil || c.Check == nil {
		return false, errSignalPair
	}
	return u.commit(ctx, pair, appKey, collectorKey, c)
}

func (u *SignalPairUpdate) commit(ctx context.Context, pair SignalPair, appKey, collectorKey []byte, c SignalPairCommit) (bool, error) {
	bindings, err := pair.bindings()
	if err != nil || len(appKey) == 0 || len(collectorKey) == 0 || subtle.ConstantTimeCompare(appKey, collectorKey) == 1 ||
		pair.Activation != [32]byte{} && (!SignalKeyFormat(appKey) || !SignalKeyFormat(collectorKey)) {
		return false, errSignalPair
	}
	check := func() error {
		if u.lock != nil {
			if err := u.lock.Check(); err != nil {
				return err
			}
		}
		return checkPairKey(ctx, u.dir, u.key)
	}
	if u.db == nil {
		return false, ErrConflict
	}
	if err := check(); err != nil {
		return false, err
	}
	if checkDatabaseBase(u.dir, u.base, u.info) != nil {
		return false, ErrConflict
	}
	secrets := [2][]byte{appKey, collectorKey}
	group, present, err := pairSlots(u.db, bindings, secrets)
	if err != nil {
		return false, err
	}
	complete := present[0] != nil || present[1] != nil
	if complete {
		if !pair.sameEntries(present) {
			return false, ErrConflict
		}
		for i, e := range present {
			if subtle.ConstantTimeCompare([]byte(e.GetPassword()), secrets[i]) != 1 {
				return false, ErrConflict
			}
		}
	}
	if c.Prepare != nil {
		if err := c.Prepare(complete); err != nil {
			return false, err
		}
	}
	recheck := func() error {
		if c.Check != nil {
			if err := c.Check(); err != nil {
				return err
			}
		}
		return check()
	}
	if complete {
		return true, u.syncRetry(recheck)
	}
	if group == nil {
		group = beaconGroup(u.db)
	}
	actors := [2]string{pair.ApprovalActor, pair.CollectorActor}
	commit := pair.commitment(appKey, collectorKey)
	for i, binding := range bindings {
		e := newBoundEntry(binding, string(secrets[i]))
		e.Values = append(e.Values, entryValue(pairPerson, pair.Person), entryValue(pairActor, actors[i]))
		if commit != "" {
			e.Values = append(e.Values, entryValue(pairCommit, commit))
		}
		group.Entries = append(group.Entries, e)
	}
	if u.db.LockProtectedEntries() != nil {
		return false, ErrUnreadable
	}
	err = saveChecked(u.dir, u.db, u.base, u.info, recheck, c.Renamed)
	if errors.Is(err, errBaseChanged) {
		return false, ErrConflict // never retry a paired install against a manual edit
	}
	return false, err
}

func (p SignalPair) commitment(app, collector []byte) string {
	if p.Activation == [32]byte{} {
		return ""
	}
	h := sha256.New()
	h.Write(p.Activation[:])
	h.Write(app) // both are fixed-length Signal keys
	h.Write(collector)
	return hex.EncodeToString(h.Sum(nil))
}

func (p SignalPair) sameEntries(entries [2]*gokeepasslib.Entry) bool {
	actors := [2]string{p.ApprovalActor, p.CollectorActor}
	var secrets [2]string
	for i, e := range entries {
		if e == nil || e.GetContent(pairPerson) != p.Person || e.GetContent(pairActor) != actors[i] {
			return false
		}
		password := e.Get("Password")
		if password == nil || !password.Value.Protected.Bool || password.Value.Content == "" {
			return false
		}
		secrets[i] = password.Value.Content
	}
	app, collector := []byte(secrets[0]), []byte(secrets[1])
	if subtle.ConstantTimeCompare(app, collector) == 1 {
		return false
	}
	if p.Activation != [32]byte{} && (!SignalKeyFormat(app) || !SignalKeyFormat(collector)) {
		return false
	}
	commit := p.commitment(app, collector)
	return entries[0].GetContent(pairCommit) == commit && entries[1].GetContent(pairCommit) == commit
}

// ReadSignalPair returns only the requested slot, and only when both verified slots are
// intact in the same database generation. Missing either one never yields the other.
func ReadSignalPair(dir string, pair SignalPair, slot Binding) (string, error) {
	bindings, err := pair.bindings()
	if err != nil || pair.Activation == [32]byte{} || slot != bindings[0] && slot != bindings[1] {
		return "", errSignalPair
	}
	u, err := openSignalPair(dir)
	if err != nil {
		return "", err
	}
	defer u.Close()
	_, entries, err := pairSlots(u.db, bindings, [2][]byte{})
	if err != nil {
		return "", err
	}
	if entries == [2]*gokeepasslib.Entry{} {
		return "", os.ErrNotExist
	}
	if !pair.sameEntries(entries) {
		return "", ErrConflict
	}
	if slot == bindings[0] {
		return entries[0].GetPassword(), nil
	}
	return entries[1].GetPassword(), nil
}

func checkPairKey(ctx context.Context, dir string, key []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := approval.CheckPrivate(dir, os.ModeDir|0o700); err != nil || !errors.Is(existingVault(dir), ErrExists) {
		return ErrConflict
	}
	current, err := readKeyFile(dir)
	defer clear(current)
	if err != nil || subtle.ConstantTimeCompare(current, key) != 1 {
		return ErrConflict
	}
	return ctx.Err()
}

// A previous process may have stopped just after rename. Flush that same generation, not a new
// candidate, before confirming the retry. The lock excludes concurrent Beacon publications.
func (u *SignalPairUpdate) syncRetry(check func() error) error {
	lock, err := lockDatabase(u.dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := check(); err != nil {
		return err
	}
	if checkDatabaseBase(u.dir, u.base, u.info) != nil {
		return ErrConflict
	}
	return approval.FlushDirectory(u.dir)
}

// Do not mistake a hidden or misbound Signal entry for an empty install. Only the live entries
// directly in the first root's Beacon group may be used. Histories are not live credentials.
func pairSlots(db *gokeepasslib.Database, bindings [2]Binding, secrets [2][]byte) (*gokeepasslib.Group, [2]*gokeepasslib.Entry, error) {
	var slots [2]*gokeepasslib.Entry
	var group *gokeepasslib.Group
	roots := db.Content.Root.Groups
	for i := range roots[0].Groups {
		g := &roots[0].Groups[i]
		if g.Name == "Beacon" {
			if group != nil {
				return nil, slots, ErrAmbiguous
			}
			group = g
		}
	}
	var scan func([]gokeepasslib.Group) error
	scan = func(groups []gokeepasslib.Group) error {
		for i := range groups {
			g := &groups[i]
			for j := range g.Entries {
				e := &g.Entries[j]
				slot := -1
				for _, v := range e.Values {
					if v.Key == "beacon.service" {
						switch v.Value.Content {
						case bindings[0].Service:
							slot = 0
						case bindings[1].Service:
							slot = 1
						}
					}
				}
				if slot < 0 {
					if e.Get(pairPerson) != nil || e.Get(pairActor) != nil || e.Get(pairCommit) != nil ||
						len(secrets[0]) != 0 && subtle.ConstantTimeCompare([]byte(e.GetPassword()), secrets[0]) == 1 ||
						len(secrets[1]) != 0 && subtle.ConstantTimeCompare([]byte(e.GetPassword()), secrets[1]) == 1 {
						return ErrConflict
					}
					continue
				}
				if slots[slot] != nil || duplicateEntryFields(e) {
					return ErrAmbiguous
				}
				if g != group || !matchesBinding(e, bindings[slot]) {
					return ErrConflict
				}
				slots[slot] = e
			}
			if err := scan(g.Groups); err != nil {
				return err
			}
		}
		return nil
	}
	err := scan(roots)
	return group, slots, err
}

func duplicateEntryFields(e *gokeepasslib.Entry) bool {
	fields := make(map[string]bool, len(e.Values))
	for _, v := range e.Values {
		if fields[v.Key] {
			return true
		}
		fields[v.Key] = true
	}
	return false
}
