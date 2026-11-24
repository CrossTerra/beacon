// Package vault is the unattended KeePass vault. There is no master password.
package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/tobischo/gokeepasslib/v3"
	"github.com/tobischo/gokeepasslib/v3/wrappers"
	"golang.org/x/sys/unix"
)

const (
	dbName  = "unattended.kdbx"
	keyName = "unattended.keyx"
	keyRef  = "unattended.secret-service"
	// 64 MiB, stored as KeePass does: bytes. The library passes Memory/1024 to Argon2.
	kdfMemory     = 64 << 20
	kdfIterations = 2
)

var (
	ErrConflict   = errors.New("vault_conflict")
	ErrAmbiguous  = errors.New("vault_ambiguous")
	ErrUnreadable = errors.New("vault_unreadable")
	ErrExists     = errors.New("vault already exists")
	ErrLocked     = errors.New("vault_locked")
	ErrKeyMissing = errors.New("vault_key_missing")
)

// Binding identifies one credential. A secret is never taken from a proposal.
type Binding struct {
	Service   string
	Origin    string
	Account   string
	Workspace string
	Use       string
}

// Create writes a new KDBX 4 database and a 0600 key file. It never replaces an existing key file or
// database: both are published with link, which fails if the name is already there.
func Create(dir string) error {
	return create(context.Background(), dir, nil)
}

type saveSessionKey func(context.Context, string, []byte) (bool, error)

// Setup chooses a key store once, before creating the database. Existing stores are never moved.
func Setup(ctx context.Context, dir string) error { return create(ctx, dir, saveKeyToSession) }

func KeyStore(dir string) string {
	if _, err := approval.CheckPrivate(filepath.Join(dir, keyRef), 0o600); err == nil {
		return "Secret Service"
	}
	return filepath.Join(dir, keyName)
}

func create(ctx context.Context, dir string, saveKey saveSessionKey) error {
	if err := approval.PrivateDir(dir); err != nil {
		return err
	}
	lock, err := approval.OpenPrivate(filepath.Join(dir, "unattended.init.lock"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	for _, name := range []string{dbName, keyName, keyRef} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return ErrExists
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	key := make([]byte, 32)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	xmlKey, err := keyFile(key)
	if err != nil {
		return err
	}
	defer clear(xmlKey)
	name := keyName
	var id [16]byte
	if saveKey != nil {
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		stored, err := saveKey(ctx, hex.EncodeToString(id[:]), xmlKey)
		if err != nil {
			return err
		}
		if stored {
			name = keyRef
		}
	}
	if name == keyRef {
		if err := publishExclusive(dir, name, []byte(hex.EncodeToString(id[:]))); err != nil {
			return err
		}
	} else if err := publishKeyFile(dir, xmlKey); err != nil {
		return err
	}
	db, err := newDB(xmlKey)
	if err != nil {
		os.Remove(filepath.Join(dir, name))
		return err
	}
	var buf bytes.Buffer
	if err := gokeepasslib.NewEncoder(&buf).Encode(db); err != nil {
		os.Remove(filepath.Join(dir, name))
		return err
	}
	if err := publishExclusive(dir, dbName, buf.Bytes()); err != nil {
		os.Remove(filepath.Join(dir, name))
		return err
	}
	return nil
}

// Put stores one secret. If the file changed and the target entry is unchanged, it re-reads and retries
// once. A changed target entry, or a second conflict, writes nothing.
func Put(dir string, b Binding, secret string) error {
	return put(dir, b, secret, false, false)
}

// PutMissing is for cutover. Do not overwrite an entry concurrently saved by KeePassXC.
func PutMissing(dir string, b Binding, secret string) error {
	return put(dir, b, secret, false, true)
}

func put(dir string, b Binding, secret string, retried, onlyMissing bool) error {
	key, err := readKeyFile(dir)
	if err != nil {
		return err
	}
	defer clear(key)
	body, info, err := readDB(dir)
	if err != nil {
		return err
	}
	base := sha256.Sum256(body)
	before, err := entrySecret(body, key, b)
	if err != nil {
		return err
	}
	db, err := decode(body, key)
	if err != nil {
		return ErrUnreadable
	}
	if err := db.UnlockProtectedEntries(); err != nil {
		return err
	}
	group := beaconGroup(db)
	matches := find(group, b)
	if len(matches) > 1 {
		return ErrAmbiguous
	}
	if len(matches) == 1 {
		if onlyMissing {
			return ErrConflict
		}
		setPassword(matches[0], secret)
	} else {
		entry := gokeepasslib.NewEntry()
		entry.Values = append(entry.Values,
			gokeepasslib.ValueData{Key: "Title", Value: gokeepasslib.V{Content: b.Service}},
			protectedPassword(secret),
			gokeepasslib.ValueData{Key: "beacon.service", Value: gokeepasslib.V{Content: b.Service}},
			gokeepasslib.ValueData{Key: "beacon.origin", Value: gokeepasslib.V{Content: b.Origin}},
			gokeepasslib.ValueData{Key: "beacon.account", Value: gokeepasslib.V{Content: b.Account}},
			gokeepasslib.ValueData{Key: "beacon.use", Value: gokeepasslib.V{Content: b.Use}},
		)
		if b.Workspace != "" {
			entry.Values = append(entry.Values, gokeepasslib.ValueData{Key: "beacon.workspace", Value: gokeepasslib.V{Content: b.Workspace}})
		}
		group.Entries = append(group.Entries, entry)
	}
	if err := db.LockProtectedEntries(); err != nil {
		return err
	}
	current, info2, err := readDB(dir)
	if err != nil {
		return err
	}
	if sha256.Sum256(current) != base || !os.SameFile(info, info2) {
		return retryPut(dir, b, secret, key, before, retried, onlyMissing)
	}
	if err := save(dir, db, &base); err != nil {
		if !errors.Is(err, ErrConflict) {
			return err
		}
		return retryPut(dir, b, secret, key, before, retried, onlyMissing)
	}
	return nil
}

func retryPut(dir string, b Binding, secret string, key []byte, before string, retried, onlyMissing bool) error {
	if retried {
		return ErrConflict
	}
	current, _, err := readDB(dir)
	if err != nil {
		return ErrConflict
	}
	again, err := entrySecret(current, key, b)
	if err != nil || again != before {
		return ErrConflict
	}
	return put(dir, b, secret, true, onlyMissing)
}

// ExportKey writes a private, exclusive copy for KeePassXC. A key is never printed.
func ExportKey(dir, dest string) error {
	key, err := readKeyFile(dir)
	if err != nil {
		return err
	}
	defer clear(key)
	return publishExclusive(filepath.Dir(dest), filepath.Base(dest), key)
}

func entrySecret(body, key []byte, b Binding) (string, error) {
	db, err := decode(body, key)
	if err != nil {
		return "", ErrUnreadable
	}
	if err := db.UnlockProtectedEntries(); err != nil {
		return "", err
	}
	matches := find(beaconGroup(db), b)
	if len(matches) > 1 {
		return "", ErrAmbiguous
	}
	if len(matches) == 0 {
		return "", nil
	}
	return matches[0].GetPassword(), nil
}

// Get returns the secret for one binding. Two entries with the same binding are unusable.
func Get(dir string, b Binding) (string, error) {
	key, err := readKeyFile(dir)
	if err != nil {
		return "", err
	}
	defer clear(key)
	body, _, err := readDB(dir)
	if err != nil {
		return "", err
	}
	db, err := decode(body, key)
	if err != nil {
		return "", ErrUnreadable
	}
	if err := db.UnlockProtectedEntries(); err != nil {
		return "", err
	}
	matches := find(beaconGroup(db), b)
	if len(matches) == 0 {
		return "", os.ErrNotExist
	}
	if len(matches) > 1 {
		return "", ErrAmbiguous
	}
	return matches[0].GetPassword(), nil
}

// Contains searches the unattended vault before accepting a write token, even when a collector entry or
// an edited KeePassXC entry has a different binding. An unreadable vault is not evidence of absence.
func Contains(dir string, secret []byte) (bool, error) {
	body, _, err := readDB(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	key, err := readKeyFile(dir)
	if err != nil {
		return false, err
	}
	defer clear(key)
	db, err := decode(body, key)
	if err != nil {
		return false, ErrUnreadable
	}
	if err := db.UnlockProtectedEntries(); err != nil {
		return false, err
	}
	var scan func([]gokeepasslib.Group) bool
	scan = func(groups []gokeepasslib.Group) bool {
		for _, g := range groups {
			for _, e := range g.Entries {
				if subtle.ConstantTimeCompare(secret, []byte(e.GetPassword())) == 1 {
					return true
				}
			}
			if scan(g.Groups) {
				return true
			}
		}
		return false
	}
	return scan(db.Content.Root.Groups), nil
}

func setPassword(e *gokeepasslib.Entry, secret string) {
	if i := e.GetPasswordIndex(); i >= 0 {
		e.Values[i].Value.Content = secret
		e.Values[i].Value.Protected = wrappers.NewBoolWrapper(true)
		return
	}
	e.Values = append(e.Values, protectedPassword(secret))
}

func protectedPassword(secret string) gokeepasslib.ValueData {
	return gokeepasslib.ValueData{Key: "Password", Value: gokeepasslib.V{Content: secret, Protected: wrappers.NewBoolWrapper(true)}}
}

func beaconGroup(db *gokeepasslib.Database) *gokeepasslib.Group {
	root := &db.Content.Root.Groups[0]
	for i := range root.Groups {
		if root.Groups[i].Name == "Beacon" {
			return &root.Groups[i]
		}
	}
	g := gokeepasslib.NewGroup()
	g.Name = "Beacon"
	root.Groups = append(root.Groups, g)
	return &root.Groups[len(root.Groups)-1]
}

func find(g *gokeepasslib.Group, b Binding) []*gokeepasslib.Entry {
	var out []*gokeepasslib.Entry
	for i := range g.Entries {
		e := &g.Entries[i]
		if e.GetContent("beacon.service") == b.Service && e.GetContent("beacon.origin") == b.Origin &&
			e.GetContent("beacon.account") == b.Account && e.GetContent("beacon.workspace") == b.Workspace &&
			e.GetContent("beacon.use") == b.Use {
			out = append(out, e)
		}
	}
	return out
}

func newDB(key []byte) (*gokeepasslib.Database, error) {
	db := gokeepasslib.NewDatabase(gokeepasslib.WithDatabaseKDBXVersion4())
	db.Header.FileHeaders.CipherID = append([]byte(nil), gokeepasslib.CipherChaCha20...)
	kdf := db.Header.FileHeaders.KdfParameters
	kdf.UUID = append([]byte(nil), gokeepasslib.KdfArgon2id...)
	kdf.Memory = kdfMemory
	kdf.Iterations = kdfIterations
	kdf.Parallelism = 2
	kdf.Version = 0x13
	cred, err := gokeepasslib.NewKeyDataCredentials(key)
	if err != nil {
		return nil, err
	}
	db.Credentials = cred
	if err := db.LockProtectedEntries(); err != nil && db.Content != nil {
		return nil, err
	}
	return db, nil
}

func decode(body, key []byte) (*gokeepasslib.Database, error) {
	db := gokeepasslib.NewDatabase()
	cred, err := gokeepasslib.NewKeyDataCredentials(key)
	if err != nil {
		return nil, err
	}
	db.Credentials = cred
	if err := gokeepasslib.NewDecoder(bytes.NewReader(body)).Decode(db); err != nil || db.Content == nil || len(db.Content.Root.Groups) == 0 {
		return nil, ErrUnreadable
	}
	return db, nil
}

// beforeSave lets a test change the file after the base hash was taken.
var beforeSave func()

// afterRename lets a test change the file after it is published and before the read-back.
var afterRename func(path string)

func save(dir string, db *gokeepasslib.Database, base *[32]byte) error {
	if beforeSave != nil {
		beforeSave()
	}
	var buf bytes.Buffer
	if err := gokeepasslib.NewEncoder(&buf).Encode(db); err != nil {
		return err
	}
	if base != nil {
		current, _, err := readDB(dir)
		if err != nil {
			return err
		}
		if sha256.Sum256(current) != *base {
			return ErrConflict
		}
	}
	return writePrivate(dir, dbName, buf.Bytes())
}

func readDB(dir string) ([]byte, os.FileInfo, error) {
	f, err := approval.OpenPrivate(filepath.Join(dir, dbName), os.O_RDONLY)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	body, err := io.ReadAll(f)
	return body, info, err
}

func readKeyFile(dir string) ([]byte, error) {
	ref, err := readSmallKey(filepath.Join(dir, keyRef))
	if err == nil {
		if id, err := hex.DecodeString(string(ref)); err != nil || len(id) != 16 {
			return nil, ErrUnreadable
		}
		return loadSessionKey(string(ref))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := readSmallKey(filepath.Join(dir, keyName))
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(filepath.Join(dir, dbName)); !errors.Is(statErr, os.ErrNotExist) {
			return nil, ErrKeyMissing
		}
	}
	return key, err
}

func readSmallKey(path string) ([]byte, error) {
	const limit = 4096
	f, err := approval.OpenPrivate(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(b) > limit {
		clear(b)
		return nil, ErrUnreadable
	}
	return b, nil
}

func publishKeyFile(dir string, body []byte) error {
	return publish(dir, keyName, body, excludeFromBackup)
}

func keyFile(raw []byte) ([]byte, error) {
	sum := sha256.Sum256(raw)
	type data struct {
		XMLName xml.Name `xml:"KeyFile"`
		Meta    struct {
			Version string `xml:"Version"`
		} `xml:"Meta"`
		Key struct {
			Data struct {
				Hash  string `xml:"Hash,attr"`
				Value string `xml:",chardata"`
			} `xml:"Data"`
		} `xml:"Key"`
	}
	var doc data
	doc.Meta.Version = "2.0"
	doc.Key.Data.Hash = fmt.Sprintf("%X", sum[:4])
	doc.Key.Data.Value = hex.EncodeToString(raw)
	body, err := xml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}

// publishExclusive links the file into place. Link fails if the name exists, including as a symlink,
// so Create cannot replace a key file or database.
func publishExclusive(dir, name string, body []byte) error {
	return publish(dir, name, body, nil)
}

func publish(dir, name string, body []byte, prepare func(string) error) error {
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if prepare != nil {
		if err := prepare(tmp.Name()); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := writeTemp(tmp, body); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrExists
		}
		return err
	}
	if afterRename != nil {
		afterRename(path)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(written, body) {
		return ErrConflict
	}
	return syncDir(dir)
}

func writeTemp(tmp *os.File, body []byte) error {
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	return tmp.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return unix.Fsync(int(d.Fd()))
}

func writePrivate(dir, name string, body []byte) error {
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := writeTemp(tmp, body); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if afterRename != nil {
		afterRename(path)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(written, body) {
		return ErrConflict
	}
	return syncDir(dir)
}
