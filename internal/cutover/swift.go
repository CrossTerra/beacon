// Package cutover verifies and archives a drained Swift installation. No record from it is submitted to Go.
package cutover

import (
	"context"
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
	Journal string       `json:"journal"`
	Signal  *SignalActor `json:"signal,omitempty"`
}

type SignalActor struct {
	Origin string `json:"origin"`
	Actor  string `json:"actor"`
	Person string `json:"person"`
}

var ErrDrain = errors.New("Swift has pending work, a start without a result, an unclosed run, or an unacknowledged record; finish draining it before cutover")

// Load returns the ordering floor. Refuse to start or set up Go over unmigrated Swift data.
func Load(dir string) (Archive, error) {
	path := filepath.Join(dir, archiveName)
	if _, err := approval.CheckPrivate(path, os.ModeDir|0o700); errors.Is(err, os.ErrNotExist) {
		for _, p := range []string{"act.json", "tether.session", "app/journal.sqlite"} {
			if _, err := os.Lstat(filepath.Join(dir, p)); !errors.Is(err, os.ErrNotExist) {
				return Archive{}, errors.New("Swift state is present; drain and quit Beacon.app and use install.sh --migrate-swift")
			}
		}
		return Archive{}, nil
	} else if err != nil {
		return Archive{}, err
	}
	if _, err := approval.CheckPrivate(filepath.Join(path, "journal.sqlite"), 0o600); err != nil {
		return Archive{}, err
	}
	var m Archive
	if err := readJSON(filepath.Join(path, "migration.json"), &m); err != nil || !approval.ValidID(m.Journal, "jrn") || !validActor(m.Signal) {
		return Archive{}, errors.New("invalid Swift archive metadata; leave the service stopped")
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
	if lock != nil {
		defer lock.Close()
	}
	target := filepath.Join(dir, archiveName)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return Load(dir) // a completed archive is never replaced
	}
	if _, err := os.Lstat(filepath.Join(dir, "journal.log")); !errors.Is(err, os.ErrNotExist) {
		return Archive{}, errors.New("Go journal already exists without a Swift archive; nothing was changed")
	}
	app := filepath.Join(dir, "app")
	if _, err := approval.CheckPrivate(app, os.ModeDir|0o700); err != nil {
		return Archive{}, err
	}
	src := filepath.Join(app, "journal.sqlite")
	if _, err := approval.CheckPrivate(src, 0o600); err != nil {
		return Archive{}, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := approval.CheckPrivate(src+suffix, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Archive{}, err
		}
	}
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
	// VACUUM INTO takes one SQLite snapshot, including committed WAL, without checkpointing the source.
	// It must not see the person's .sqliterc or print SQL errors with legacy content in them.
	if _, err := query(ctx, sqlite, src, "VACUUM INTO '"+strings.ReplaceAll(snapshot, "'", "''")+"';"); err != nil {
		return Archive{}, err
	}
	if err := os.Chmod(snapshot, 0o600); err != nil {
		return Archive{}, err
	}
	id, err := verify(ctx, sqlite, snapshot)
	if err != nil {
		return Archive{}, err
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
	m := Archive{id, settings.Signal}
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

func stopped(ctx context.Context, dir string) (*os.File, error) {
	f, err := approval.OpenPrivate(filepath.Join(dir, "tether.lock"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("Swift tether still holds its lock; quit Beacon.app from its menu")
	}
	if _, err := os.Lstat(filepath.Join(dir, "tether.session")); !errors.Is(err, os.ErrNotExist) {
		f.Close()
		return nil, errors.New("Swift tether cleanup is unconfirmed; reopen Swift to finish its sweep before cutting over")
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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	b, err := exec.CommandContext(ctx, sqlite, "-batch", "-noheader", "-bail", "-init", "/dev/null", "-readonly", db, sql).Output()
	if err != nil {
		return "", errors.New("could not read or snapshot Swift's SQLite journal; it was not retired")
	}
	return strings.TrimSpace(string(b)), nil
}

func verify(ctx context.Context, sqlite, db string) (string, error) {
	const checks = `PRAGMA quick_check;
SELECT value FROM meta WHERE name='journal_id';
SELECT count(*) FROM entries e WHERE
 e.kind NOT IN ('proposed','approved','declined','withdrawn','expired','started','outcome_unknown','executed','failed',
                'runner_working','runner_waiting','runner_stalled','runner_finished','runner_failed','runner_closed')
 OR NOT json_valid(e.body)
 OR (e.kind NOT GLOB 'runner_*' AND e.seq = (SELECT max(seq) FROM entries WHERE proposal=e.proposal AND kind NOT GLOB 'runner_*')
       AND (e.kind NOT IN ('declined','withdrawn','expired','executed','failed')
         OR (e.kind='executed' AND ((SELECT json_extract(p.body,'$.type') FROM entries p WHERE p.proposal=e.proposal AND p.kind='proposed')='agent.run')
             AND NOT EXISTS (SELECT 1 FROM entries c WHERE c.proposal=e.proposal AND c.kind='runner_closed' AND c.seq>e.seq))))
 OR NOT EXISTS (SELECT 1 FROM entries p WHERE p.proposal=e.proposal AND p.kind='proposed' AND p.seq<=e.seq);
SELECT count(*) FROM entries e LEFT JOIN outbox o ON e.seq=o.seq
 WHERE o.seq IS NULL OR o.state!='delivered' OR o.event_id IS NULL OR o.event_id=''
    OR o.key!='beacon:'||(SELECT value FROM meta WHERE name='journal_id')||':'||e.seq;
SELECT count(*) FROM outbox WHERE seq NOT IN (SELECT seq FROM entries);`
	out, err := query(ctx, sqlite, db, checks)
	if err != nil {
		return "", err
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 5 || lines[0] != "ok" || !approval.ValidID(lines[1], "jrn") {
		return "", fmt.Errorf("%w: invalid Swift journal metadata", approval.ErrCorrupt)
	}
	if lines[2] != "0" || lines[3] != "0" || lines[4] != "0" {
		return "", ErrDrain
	}
	return lines[1], nil
}
