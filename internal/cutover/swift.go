// Package cutover verifies and archives a drained Swift installation. No record from it is submitted to Go.
package cutover

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"golang.org/x/sys/unix"
)

const archiveName = "swift-archive"

type Archive struct {
	Journal        string       `json:"journal"`
	SnapshotSHA256 string       `json:"snapshot_sha256"`
	Signal         *SignalActor `json:"signal,omitempty"`
}

type SignalActor struct {
	Origin string `json:"origin"`
	Actor  string `json:"actor"`
	Person string `json:"person"`
}

var (
	ErrDrain   = errors.New("the Swift journal has pending work, a start without a result, an unclosed run, or an unacknowledged record; finish draining it before cutover")
	ErrChanged = errors.New("the Swift journal or archive changed after the snapshot; leave both in place and do not cut over")

	errTetherUnconfirmed = errors.New("the Swift tether cleanup is unconfirmed; reopen Swift to finish its sweep before cutting over")
)

// Load returns the ordering floor. Without an archive, refuse an unmigrated Swift journal or unconfirmed tether.
func Load(dir string) (Archive, error) {
	path := filepath.Join(dir, archiveName)
	if _, err := approval.CheckPrivate(path, os.ModeDir|0o700); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(filepath.Join(dir, "tether.session")); err == nil {
			return Archive{}, errTetherUnconfirmed
		} else if !errors.Is(err, os.ErrNotExist) {
			return Archive{}, err
		}
		if _, err := os.Lstat(filepath.Join(dir, "app", "journal.sqlite")); err == nil {
			return Archive{}, errors.New("legacy Swift state is present; drain and quit Beacon.app and use install.sh --migrate-swift")
		} else if !errors.Is(err, os.ErrNotExist) {
			return Archive{}, err
		}
		// act.json held client trust settings, not journal data. Go never uses it.
		return Archive{}, nil
	} else if err != nil {
		return Archive{}, err
	}
	var m Archive
	if err := readJSON(filepath.Join(path, "migration.json"), &m); err != nil || !approval.ValidID(m.Journal, "jrn") ||
		!digest(m.SnapshotSHA256) || !validActor(m.Signal) {
		return Archive{}, errors.New("invalid Swift archive metadata; leave the service stopped")
	}
	snapshot := filepath.Join(path, "journal.sqlite")
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(snapshot + suffix); !errors.Is(err, os.ErrNotExist) {
			return Archive{}, ErrChanged // a standalone archive must not have mutable SQLite sidecars
		}
	}
	if sum, err := snapshotHash(snapshot); err != nil {
		return Archive{}, err
	} else if sum != m.SnapshotSHA256 {
		return Archive{}, ErrChanged
	}
	return m, nil
}

func validActor(s *SignalActor) bool {
	return s == nil || approval.ValidID(s.Actor, "act") && approval.ValidID(s.Person, "act") && s.Actor != s.Person && s.Origin != ""
}

// Migrate is called offline with the Go instance lock held. The app and retired collector must already be stopped.
// sqlite is the system CLI supplied by the macOS migration command; tests use the same CLI with private fixtures.
func Migrate(ctx context.Context, dir, sqlite string) (Archive, error) {
	if !approval.HoldsInstance(dir) {
		return Archive{}, approval.ErrLocked
	}
	lock, err := stopped(ctx, dir)
	if err != nil {
		return Archive{}, err
	}
	defer lock.Close()
	target := filepath.Join(dir, archiveName)
	_, err = os.Lstat(target)
	retry := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Archive{}, err
	}
	if _, err := os.Lstat(filepath.Join(dir, "journal.log")); !retry && !errors.Is(err, os.ErrNotExist) {
		return Archive{}, errors.New("a Go journal already exists without a Swift archive; nothing was changed")
	}
	app := filepath.Join(dir, "app")
	if _, err := approval.CheckPrivate(app, os.ModeDir|0o700); err != nil {
		return Archive{}, err
	}
	src := filepath.Join(app, "journal.sqlite")
	var settings struct{ Signal *SignalActor }
	if err := readJSON(filepath.Join(app, "settings.json"), &settings); err != nil && !errors.Is(err, os.ErrNotExist) || !validActor(settings.Signal) {
		return Archive{}, errors.New("could not read the old Signal actor binding")
	}
	stage, err := os.MkdirTemp(dir, ".swift-archive-")
	if err != nil {
		return Archive{}, err
	}
	defer os.RemoveAll(stage)
	snapshot := filepath.Join(stage, "journal.sqlite")
	if err := snapshotSwift(ctx, sqlite, src, snapshot); err != nil {
		return Archive{}, err
	}
	if retry {
		m, err := Load(dir)
		if err != nil {
			return Archive{}, err
		}
		if !sameActor(m.Signal, settings.Signal) {
			return Archive{}, ErrChanged
		}
		id, err := query(ctx, sqlite, snapshot, "SELECT value FROM meta WHERE name='journal_id';")
		if err != nil {
			return Archive{}, err
		}
		if id != m.Journal {
			return Archive{}, ErrChanged
		}
		if err := matchSource(ctx, sqlite, snapshot, filepath.Join(target, "journal.sqlite")); err != nil {
			return Archive{}, err
		}
		if err := socketFree(ctx, dir); err != nil {
			return Archive{}, err
		}
		return Load(dir) // check the archive again; never refresh it or import an old action
	}
	id, hasRecords, err := verify(ctx, sqlite, snapshot)
	if err != nil {
		return Archive{}, err
	}
	if hasRecords && settings.Signal == nil {
		return Archive{}, errors.New("the drained Swift journal has records but no saved Signal actor; reconnect the same actor before cutover")
	}
	f, err := approval.OpenPrivate(snapshot, os.O_RDWR)
	if err != nil {
		return Archive{}, err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return Archive{}, err
	}
	if err := socketFree(ctx, dir); err != nil {
		return Archive{}, err
	}
	sum, err := snapshotHash(snapshot)
	if err != nil {
		return Archive{}, err
	}
	m := Archive{Journal: id, SnapshotSHA256: sum, Signal: settings.Signal}
	b, err := json.Marshal(m)
	if err != nil {
		return Archive{}, err
	}
	if err := approval.WritePrivate(filepath.Join(stage, "migration.json"), b); err != nil {
		return Archive{}, err
	}
	if err := os.Rename(stage, target); err != nil {
		return Archive{}, err
	}
	return m, approval.FlushDirectory(dir)
}

// Some SQLite builds cannot open a read-only WAL-mode DB once its sidecars have been removed. Copy
// the closed source and any committed WAL into a private subdirectory, then let SQLite create its
// WAL index and checkpoint only the copy. Do not copy the old transient -shm index.
func snapshotSwift(ctx context.Context, sqlite, src, dest string) error {
	work, err := os.MkdirTemp(filepath.Dir(dest), ".source-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	copyDB := filepath.Join(work, "journal.sqlite")
	files := make(map[string]os.FileInfo)
	for _, suffix := range []string{"", "-wal", "-journal"} {
		name := src + suffix
		info, err := copyPrivate(name, copyDB+suffix)
		if suffix != "" && errors.Is(err, os.ErrNotExist) {
			files[name] = nil
			continue
		}
		if err != nil {
			return err
		}
		files[name] = info
	}
	if _, err := sqliteQuery(ctx, sqlite, copyDB, "VACUUM INTO '"+strings.ReplaceAll(dest, "'", "''")+"';", false); err != nil {
		return err
	}
	for name, old := range files {
		current, err := approval.CheckPrivate(name, 0o600)
		if old == nil && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || old == nil || !os.SameFile(old, current) || old.Size() != current.Size() || !old.ModTime().Equal(current.ModTime()) {
			return ErrChanged
		}
	}
	return os.Chmod(dest, 0o600)
}

func copyPrivate(src, dest string) (os.FileInfo, error) {
	f, err := approval.OpenPrivate(src, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	w, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(w, f)
	return info, errors.Join(err, w.Close())
}

func sameActor(a, b *SignalActor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func snapshotHash(path string) (string, error) {
	f, err := approval.OpenPrivate(path, os.O_RDONLY)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func digest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

// The SQLite shell hashes logical rows and schema, including committed WAL. Comparing main-file bytes
// could miss a WAL-only write; comparing WAL bytes would also mistake a harmless checkpoint for new work.
func matchSource(ctx context.Context, sqlite, src, snapshot string) error {
	const hashSQL = ".sha3sum --schema --sha3-256"
	want, err := query(ctx, sqlite, snapshot, hashSQL)
	if err != nil {
		return err
	}
	got, err := query(ctx, sqlite, src, hashSQL)
	if err != nil {
		return err
	}
	if !digest(want) || !digest(got) || got != want {
		return ErrChanged
	}
	return nil
}

func stopped(ctx context.Context, dir string) (*os.File, error) {
	f, err := approval.OpenPrivate(filepath.Join(dir, "tether.lock"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("the Swift tether still holds its lock; quit Beacon.app from its menu")
	}
	if _, err := os.Lstat(filepath.Join(dir, "tether.session")); !errors.Is(err, os.ErrNotExist) {
		f.Close()
		return nil, errTetherUnconfirmed
	}
	if err := socketFree(ctx, dir); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func socketFree(ctx context.Context, dir string) error {
	run := filepath.Join(dir, "run")
	if _, err := approval.CheckPrivate(run, os.ModeDir|0o700); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	path := filepath.Join(run, "act.sock")
	if _, err := approval.CheckPrivate(path, os.ModeSocket|0o600); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "unix", path)
	if err == nil {
		c.Close() // never send anything to Swift
		return errors.New("a proposal listener is still running; quit Beacon.app from its menu")
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		return nil // Go checks again at listen time before removing a stale socket
	}
	return errors.New("could not verify the old proposal socket is free")
}

func readJSON(path string, v any) error {
	f, err := approval.OpenPrivate(path, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return errors.New("cannot read Swift migration settings")
	}
	if _, err := approval.ParseObject(b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func query(ctx context.Context, sqlite, db, sql string) (string, error) {
	return sqliteQuery(ctx, sqlite, db, sql, true)
}

func sqliteQuery(ctx context.Context, sqlite, db, sql string, readOnly bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := []string{"-batch", "-noheader", "-bail", "-init", "/dev/null"}
	if readOnly {
		args = append(args, "-readonly")
	}
	b, err := exec.CommandContext(ctx, sqlite, append(args, db, sql)...).Output()
	if err != nil {
		return "", errors.New("could not read or snapshot Swift's SQLite journal; it was not retired")
	}
	return strings.TrimSpace(string(b)), nil
}

func verify(ctx context.Context, sqlite, db string) (string, bool, error) {
	const checks = `PRAGMA quick_check;
SELECT value FROM meta WHERE name='journal_id';
SELECT count(*) FROM entries e WHERE
 e.kind NOT IN ('proposed','approved','declined','withdrawn','expired','started','outcome_unknown','executed','failed',
                'runner_working','runner_waiting','runner_stalled','runner_finished','runner_failed','runner_closed')
 OR NOT json_valid(e.body)
 OR (e.kind='started' AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.proposal=e.proposal AND r.seq>e.seq AND r.kind IN ('executed','failed')))
 OR (e.kind NOT GLOB 'runner_*' AND e.seq = (SELECT max(seq) FROM entries WHERE proposal=e.proposal AND kind NOT GLOB 'runner_*')
       AND (e.kind NOT IN ('declined','withdrawn','expired','executed','failed')
         OR (e.kind='executed' AND ((SELECT json_extract(p.body,'$.type') FROM entries p WHERE p.proposal=e.proposal AND p.kind='proposed')='agent.run')
             AND NOT EXISTS (SELECT 1 FROM entries c WHERE c.proposal=e.proposal AND c.kind='runner_closed' AND c.seq>e.seq))))
 OR NOT EXISTS (SELECT 1 FROM entries p WHERE p.proposal=e.proposal AND p.kind='proposed' AND p.seq<=e.seq);
SELECT count(*) FROM entries e LEFT JOIN outbox o ON e.seq=o.seq
 WHERE o.seq IS NULL OR o.state!='delivered' OR o.event_id IS NULL OR o.event_id=''
    OR o.key!='beacon:'||(SELECT value FROM meta WHERE name='journal_id')||':'||e.seq;
SELECT count(*) FROM outbox WHERE seq NOT IN (SELECT seq FROM entries);
SELECT EXISTS (SELECT 1 FROM entries);`
	out, err := query(ctx, sqlite, db, checks)
	if err != nil {
		return "", false, err
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 6 || lines[0] != "ok" || !approval.ValidID(lines[1], "jrn") {
		return "", false, fmt.Errorf("%w: invalid Swift journal metadata", approval.ErrCorrupt)
	}
	if lines[2] != "0" || lines[3] != "0" || lines[4] != "0" {
		return "", false, ErrDrain
	}
	return lines[1], lines[5] != "0", nil
}
