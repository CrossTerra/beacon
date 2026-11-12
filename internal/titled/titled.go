// Package titled records the subjects whose Signal threads a send pass checked for a retitle and left alone, so a
// later pass does not read them from Signal again. It holds subjects and fixed outcome codes: no title, no name.
package titled

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

// FileName is the record inside the state directory.
const FileName = "retitled.json"

// Outcomes a thread is left alone with. Fixed codes.
const (
	// NotOpened is a thread Beacon's first event on the subject did not open: someone else's.
	NotOpened = "not_opened"
	// NotFound is an event or thread Signal no longer shows the key.
	NotFound = "not_found"
	// Moved is a thread merged into another, or a subject that left its thread.
	Moved = "moved"
	// Renamed is a thread whose title is no longer the one Beacon opened it with: someone renamed it.
	Renamed = "renamed"
	// Same is a thread that already has the title Beacon would give it.
	Same = "same"
	// Unreadable is a conversation the source cannot read any more.
	Unreadable = "unreadable"
)

var (
	outcomes = map[string]bool{NotOpened: true, NotFound: true, Moved: true, Renamed: true, Same: true, Unreadable: true}
	subject  = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)
)

// Store is the record of one state directory.
type Store struct {
	dir     string
	checked map[string]string
}

type file struct {
	Checked map[string]string `json:"checked"`
}

// Read loads the record in dir. A missing file is an empty record.
func Read(dir string) (*Store, error) {
	s := &Store{dir: dir, checked: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, errors.New("titled: the record could not be read")
	}
	for k, v := range f.Checked {
		if !subject.MatchString(k) || !outcomes[v] {
			return nil, errors.New("titled: the record holds an entry it cannot read")
		}
		s.checked[k] = v
	}
	return s, nil
}

// Checked reports whether subject's thread was checked and left alone.
func (s *Store) Checked(subj string) bool {
	_, ok := s.checked[subj]
	return ok
}

// Mark records that subject's thread was left alone, and why, and writes the record atomically.
func (s *Store) Mark(subj, outcome string) error {
	if !subject.MatchString(subj) || !outcomes[outcome] {
		return errors.New("titled: not a subject and outcome it records")
	}
	s.checked[subj] = outcome
	return s.write()
}

func (s *Store) write() error {
	data, err := json.Marshal(file{Checked: s.checked})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, FileName+".tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
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
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(s.dir, FileName)); err != nil {
		return err
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
