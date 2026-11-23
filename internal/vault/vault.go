// Package vault is the unattended KeePass vault. The key file is 0600. There is no master password.
// Linux Secret Service is not wired in this slice; both platforms use the key file.
package vault

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/tobischo/gokeepasslib/v3"
	"golang.org/x/sys/unix"
)

const (
	dbName  = "unattended.kdbx"
	keyName = "unattended.keyx"
)

var (
	ErrConflict   = errors.New("vault_conflict")
	ErrAmbiguous  = errors.New("vault_ambiguous")
	ErrUnreadable = errors.New("vault_unreadable")
)

// Binding identifies one credential. A secret is never taken from a proposal.
type Binding struct {
	Service   string
	Origin    string
	Account   string
	Workspace string
	Use       string
}

// Create writes a new KDBX 4 database and a 0600 key file. It refuses if either file exists.
func Create(dir string) error {
	if err := approval.PrivateDir(dir); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(dir, dbName)); err == nil {
		return errors.New("vault already exists")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	xmlKey, err := keyFile(key)
	if err != nil {
		return err
	}
	if err := writePrivate(dir, keyName, xmlKey); err != nil {
		return err
	}
	db, err := newDB(xmlKey)
	if err != nil {
		return err
	}
	return save(dir, db, nil)
}

// Put stores one secret. If the file changed and the same binding changed, it writes nothing.
func Put(dir string, b Binding, secret string) error {
	key, err := os.ReadFile(filepath.Join(dir, keyName))
	if err != nil {
		return err
	}
	body, info, err := readDB(dir)
	if err != nil {
		return err
	}
	base := sha256.Sum256(body)
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
		setPassword(matches[0], secret)
	} else {
		entry := gokeepasslib.NewEntry()
		entry.Values = append(entry.Values,
			gokeepasslib.ValueData{Key: "Title", Value: gokeepasslib.V{Content: b.Service}},
			gokeepasslib.ValueData{Key: "Password", Value: gokeepasslib.V{Content: secret}},
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
		return ErrConflict
	}
	return save(dir, db, &base)
}

// Get returns the secret for one binding. Two entries with the same binding are unusable.
func Get(dir string, b Binding) (string, error) {
	key, err := os.ReadFile(filepath.Join(dir, keyName))
	if err != nil {
		return "", err
	}
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

func setPassword(e *gokeepasslib.Entry, secret string) {
	if i := e.GetPasswordIndex(); i >= 0 {
		e.Values[i].Value.Content = secret
		return
	}
	e.Values = append(e.Values, gokeepasslib.ValueData{Key: "Password", Value: gokeepasslib.V{Content: secret}})
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
	db := gokeepasslib.NewDatabase(gokeepasslib.WithDatabaseKDBXVersion40())
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
	if err := gokeepasslib.NewDecoder(bytes.NewReader(body)).Decode(db); err != nil {
		return nil, err
	}
	return db, nil
}

// beforeSave lets a test change the file after the base hash was taken.
var beforeSave func()

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
	path := filepath.Join(dir, dbName)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	body, err := os.ReadFile(path)
	return body, info, err
}

func keyFile(raw []byte) ([]byte, error) {
	// 32 raw bytes is a key file the library accepts without a password.
	return append([]byte(nil), raw...), nil
}

func writePrivate(dir, name string, body []byte) error {
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
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
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return unix.Fsync(int(d.Fd()))
}
