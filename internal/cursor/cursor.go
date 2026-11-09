// Package cursor stores each source's committed cursor, and which sources have had their first run accepted, in
// the state directory. A cursor moves only after a pass has decided every item (the runner commits it). A dry run
// reads and does not write.
package cursor

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const (
	cursorsFile  = "cursors.json"
	acceptedFile = "accepted.json"
)

// Store is the cursors and the accepted-first set of one state directory.
type Store struct {
	dir      string
	cursors  map[string]string
	accepted map[string]bool
	writable bool
}

// Read loads the store without creating files. Missing files are an empty store.
func Read(dir string) (*Store, error) {
	s := &Store{dir: dir, cursors: map[string]string{}, accepted: map[string]bool{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Open loads the store for writing, creating the directory if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s, err := Read(dir)
	if err != nil {
		return nil, err
	}
	s.writable = true
	return s, nil
}

func (s *Store) load() error {
	if err := readJSON(filepath.Join(s.dir, cursorsFile), &s.cursors); err != nil {
		return err
	}
	if s.cursors == nil {
		s.cursors = map[string]string{}
	}
	var names []string
	if err := readJSON(filepath.Join(s.dir, acceptedFile), &names); err != nil {
		return err
	}
	for _, n := range names {
		s.accepted[n] = true
	}
	return nil
}

func readJSON(path string, dest any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, dest)
}

// Get returns the committed cursor, or false when the source has none.
func (s *Store) Get(name string) ([]byte, bool) {
	enc, ok := s.cursors[name]
	if !ok || enc == "" {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// Commit stores next as the source's cursor. It syncs the file before returning, so a crash after Commit does not
// lose the move. A store from Read refuses to write.
func (s *Store) Commit(name string, next []byte) error {
	if !s.writable {
		return errors.New("cursor: the store is not open for writing")
	}
	if name == "" || len(next) == 0 {
		return errors.New("cursor: a commit needs a source and a cursor")
	}
	s.cursors[name] = base64.StdEncoding.EncodeToString(next)
	return writeAtomic(s.dir, cursorsFile, s.cursors)
}

// Accepted reports whether the source's first run has been accepted.
func (s *Store) Accepted(name string) bool { return s.accepted[name] }

// Accept records that the named sources' first runs were accepted. Sending for them may start on the next pass.
func (s *Store) Accept(names []string) error {
	if !s.writable {
		return errors.New("cursor: the store is not open for writing")
	}
	for _, n := range names {
		if n == "" {
			return errors.New("cursor: a source name is empty")
		}
		s.accepted[n] = true
	}
	out := make([]string, 0, len(s.accepted))
	for n := range s.accepted {
		out = append(out, n)
	}
	sortStrings(out)
	return writeAtomic(s.dir, acceptedFile, out)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
