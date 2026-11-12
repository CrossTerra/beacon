package seal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// FloorFile is the subject floor in the state directory: a level per subject hash, no content (DEC-34).
const FloorFile = "seal-floor.json"

// Floor is the highest level Beacon has sent on each subject. A later item on that subject is sealed at least that
// high. Subjects are kept as hashes. A floor from ReadFloor raises in memory only, so a dry run is consistent within
// its pass and writes nothing.
type Floor struct {
	mu       sync.Mutex
	dir      string
	writable bool
	levels   map[string]Level
	// local holds raises this process made without persisting them: a dry run's, or an item not recorded.
	local map[string]Level
}

// ReadFloor loads the floor in dir without writing. A missing file is an empty floor.
func ReadFloor(dir string) (*Floor, error) {
	f := &Floor{dir: dir, levels: map[string]Level{}, local: map[string]Level{}}
	data, err := os.ReadFile(filepath.Join(dir, FloorFile))
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &f.levels); err != nil {
			return nil, err
		}
	}
	for k, l := range f.levels {
		if !Valid(string(l)) {
			delete(f.levels, k)
		}
	}
	return f, nil
}

// OpenFloor loads the floor in dir for writing. The caller holds the outbox lock on dir.
func OpenFloor(dir string) (*Floor, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := ReadFloor(dir)
	if err != nil {
		return nil, err
	}
	f.writable = true
	return f, nil
}

// NewFloor is an empty floor in memory, for tests and explain.
func NewFloor() *Floor { return &Floor{levels: map[string]Level{}, local: map[string]Level{}} }

// HashSubject is how the floor names a subject.
func HashSubject(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:16])
}

// Get is the subject's floor, None when Beacon has sent nothing sealed there.
func (f *Floor) Get(subject string) Level {
	if f == nil {
		return None
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h := HashSubject(subject)
	return Max(f.levels[h], f.local[h])
}

// Raise records that level was sent on subject. It never lowers the floor. With persist, a writable floor is synced
// to disk before Raise returns; without it, or on a floor from ReadFloor, the raise holds for this process only.
func (f *Floor) Raise(subject string, level Level, persist bool) error {
	if f == nil || level == None {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h := HashSubject(subject)
	if !f.writable || !persist {
		f.local[h] = Max(f.local[h], level)
		return nil
	}
	if f.levels[h].Rank() >= level.Rank() {
		return nil
	}
	f.levels[h] = level
	return writeAtomic(f.dir, FloorFile, f.levels)
}

func writeAtomic(dir, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, name+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return err
	}
	cleanup = false
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
