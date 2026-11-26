package cutover

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
)

const oldID = "jrn_01m6a6ft00e40r2081040g2081"

func fixture(t *testing.T, events string) (dir, sqlite, db string) {
	t.Helper()
	var err error
	sqlite, err = exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 CLI is needed only for the Swift archive fixture")
	}
	dir, err = os.MkdirTemp("", "bc-m-") // short enough for the same UNIX socket on Darwin
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	must(t, os.Mkdir(filepath.Join(dir, "app"), 0o700))
	db = filepath.Join(dir, "app", "journal.sqlite")
	sql(t, sqlite, db, `CREATE TABLE meta(name TEXT PRIMARY KEY,value TEXT NOT NULL);
        CREATE TABLE entries(seq INTEGER PRIMARY KEY,proposal TEXT NOT NULL,kind TEXT NOT NULL,at INTEGER NOT NULL,body TEXT NOT NULL);
        CREATE TABLE outbox(seq INTEGER PRIMARY KEY,key TEXT NOT NULL,nonce TEXT,bytes BLOB,state TEXT,event_id TEXT,stopped TEXT);
        INSERT INTO meta VALUES('journal_id','`+oldID+`');`+events)
	must(t, os.Chmod(db, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "app", "settings.json"), []byte(`{"signal":{"origin":"https://signal.fixture.test","actor":"act_01m6a6ft00e40r2081040g2082","person":"act_01m6a6ft00e40r2081040g2081"}}`), 0o600))
	return dir, sqlite, db
}

func sql(t *testing.T, tool, db, query string) string {
	t.Helper()
	b, err := exec.Command(tool, "-batch", "-bail", "-init", "/dev/null", db, query).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture SQL: %v: %s", err, b)
	}
	return string(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func entry(seq int, kind, body, ack string) string {
	return fmt.Sprintf(`INSERT INTO entries VALUES(%d,'fixture','%s',0,'%s');
INSERT INTO outbox VALUES(%d,'beacon:%s:%d','n',X'010203','%s','event-fixture',NULL);`, seq, kind, body, seq, oldID, seq, ack)
}

func lockDir(t *testing.T, dir string) func() {
	t.Helper()
	f, err := approval.LockInstance(dir)
	must(t, err)
	close := func() { approval.CloseInstance(dir, f) }
	t.Cleanup(close)
	return close
}

func TestLoadRefusesUnconfirmedTetherWithoutJournal(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "tether.session"), []byte("fixture"), 0o600))
	if _, err := Load(dir); !errors.Is(err, errTetherUnconfirmed) {
		t.Fatalf("unconfirmed tether without journal: %v", err)
	}
}

func TestDrainPreconditions(t *testing.T) {
	const post = `{"type":"slack.post"}`
	for _, tc := range []struct{ name, events string }{
		{"pending", entry(1, "proposed", post, "delivered")},
		{"approved", entry(1, "proposed", post, "delivered") + entry(2, "approved", `{}`, "delivered")},
		{"started", entry(1, "proposed", post, "delivered") + entry(2, "started", `{}`, "delivered")},
		{"unknown", entry(1, "proposed", post, "delivered") + entry(2, "outcome_unknown", `{}`, "delivered")},
		{"unclosed", entry(1, "proposed", `{"type":"agent.run"}`, "delivered") + entry(2, "executed", `{}`, "delivered") + entry(3, "runner_finished", `{}`, "delivered")},
		{"not acknowledged", entry(1, "proposed", post, "delivered") + entry(2, "withdrawn", `{}`, "stopped")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, sqlite, _ := fixture(t, tc.events)
			lockDir(t, dir)
			if _, err := Migrate(t.Context(), dir, sqlite); !errors.Is(err, ErrDrain) {
				t.Fatalf("drain: %v", err)
			}
			if _, err := Load(dir); err == nil {
				t.Fatal("undrained state was ready")
			}
		})
	}
}

func leaveCommittedWAL(t *testing.T, sqlite, db, events string) {
	t.Helper()
	// Copy the committed WAL before its writer closes. Leave it for the archive reader as after a crash.
	cmd := exec.CommandContext(t.Context(), sqlite, "-batch", "-init", "/dev/null", db)
	w, err := cmd.StdinPipe()
	must(t, err)
	r, err := cmd.StdoutPipe()
	must(t, err)
	must(t, cmd.Start())
	defer func() { w.Close(); cmd.Wait() }()
	_, err = io.WriteString(w, "PRAGMA journal_mode=WAL;\n"+events+"\nSELECT 'ready';\n")
	must(t, err)
	reader := bufio.NewReader(r)
	if a, _ := reader.ReadString('\n'); a != "wal\n" {
		t.Fatalf("wal: %q", a)
	}
	if b, _ := reader.ReadString('\n'); b != "ready\n" {
		t.Fatalf("ready: %q", b)
	}
	main, err := os.ReadFile(db)
	must(t, err)
	wal, err := os.ReadFile(db + "-wal")
	must(t, err)
	must(t, w.Close())
	must(t, cmd.Wait())
	must(t, os.WriteFile(db, main, 0o600))
	must(t, os.WriteFile(db+"-wal", wal, 0o600))
}

func TestTakeoverArchivesCommittedWALAndNeverImportsARecord(t *testing.T) {
	dir, sqlite, db := fixture(t, entry(1, "proposed", `{"type":"agent.run"}`, "delivered")+
		entry(2, "executed", `{}`, "delivered"))
	leaveCommittedWAL(t, sqlite, db, entry(3, "runner_closed", `{}`, "delivered"))
	must(t, os.Mkdir(filepath.Join(dir, "run"), 0o700))
	socket := filepath.Join(dir, "run", "act.sock")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	must(t, err)
	old.SetUnlinkOnClose(false)
	must(t, os.Chmod(socket, 0o600))
	must(t, old.Close())
	closeLock := lockDir(t, dir)
	m, err := Migrate(t.Context(), dir, sqlite)
	must(t, err)
	if m.Journal != oldID || strings.TrimSpace(sql(t, sqlite, filepath.Join(dir, archiveName, "journal.sqlite"),
		"SELECT kind FROM entries WHERE seq=3;")) != "runner_closed" {
		t.Fatalf("archive missing the committed WAL: %+v", m)
	}
	sql(t, sqlite, db, "PRAGMA wal_checkpoint(TRUNCATE);")
	if next, err := Migrate(t.Context(), dir, sqlite); err != nil || next != (Archive{}) {
		t.Fatalf("verification after a WAL checkpoint should not report a new archive: %+v, %v", next, err)
	}
	closeLock()
	if j, err := approval.OpenJournal(dir, time.Unix(0, 0), m.Journal); !errors.Is(err, approval.ErrJournalAge) {
		if j != nil {
			j.Close()
		}
		t.Fatalf("accepted a journal that cannot sort after Swift: %v", err)
	}
	j, err := approval.OpenJournal(dir, time.Now().Add(24*time.Hour), m.Journal)
	must(t, err)
	defer j.Close()
	if j.ID() <= oldID || j.DeliveryStatus().Waiting != 0 {
		t.Fatal("Go did not start with a strictly newer, empty outbox")
	}
	at := time.Now().UTC()
	must(t, j.RecordLocal("settings_changed", at))
	origin, err := approval.ParseOrigin(approval.LocalOrigin, "")
	must(t, err)
	s, err := actsocket.Listen(dir, j, actsocket.Options{Origin: origin, Enabled: []approval.Action{approval.Merge}})
	must(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); s.Close(); <-done }()
	reply, err := actsocket.Call(ctx, dir, approval.Object{"op": "propose", "type": "github.merge", "params": approval.Object{
		"repo": "fixture/project", "number": int64(1), "base": "main", "head": strings.Repeat("a", 40), "method": "squash"},
		"agent": approval.Object{"name": "fixture"}, "reason": "new work"}, 0)
	must(t, err)
	if !strings.Contains(string(reply), `"review_url":"`+approval.LocalOrigin+`/p/apr_`) {
		t.Fatalf("Go did not take over the same proposal socket: %s", reply)
	}
	posts := 0
	who := approval.SignalIdentity{Person: "act_01m6a6ft00e40r2081040g2081", Handle: "fixture"}
	result, err := j.Drain(t.Context(), who, recordSink(func(b []byte) approval.RecordAnswer {
		posts++
		if !strings.Contains(string(b), `"beacon:`+j.ID()+`:1"`) || strings.Contains(string(b), oldID) {
			t.Fatal("the first new Signal record was not sequence 1 or included the retired journal")
		}
		return approval.RecordAnswer{EventID: "evt_01m6a6ft00e40r2081040g2081"}
	}))
	if err != nil || posts != 1 || result.Sent != 1 {
		t.Fatalf("new journal delivery: %v, %d, %v", result, posts, err)
	}
}

func TestRetryRefusesChangedSwiftState(t *testing.T) {
	for _, what := range []string{"source WAL", "archive", "archive metadata", "Signal binding"} {
		t.Run(what, func(t *testing.T) {
			dir, sqlite, db := fixture(t, entry(1, "proposed", `{"type":"slack.post"}`, "delivered")+
				entry(2, "withdrawn", `{}`, "delivered"))
			sql(t, sqlite, db, "PRAGMA journal_mode=WAL;")
			lockDir(t, dir)
			_, err := Migrate(t.Context(), dir, sqlite)
			must(t, err)
			archived := filepath.Join(dir, archiveName, "journal.sqlite")
			if _, err := Migrate(t.Context(), dir, sqlite); err != nil {
				t.Fatalf("unchanged retry: %v", err)
			}
			switch what {
			case "source WAL":
				main, err := os.ReadFile(db)
				must(t, err)
				// Even work drained again after Swift resumed is a different history.
				leaveCommittedWAL(t, sqlite, db, strings.ReplaceAll(entry(3, "proposed", `{"type":"slack.post"}`, "delivered")+
					entry(4, "withdrawn", `{}`, "delivered"), "'fixture'", "'after-first-cutover'"))
				next, err := os.ReadFile(db)
				must(t, err)
				if !bytes.Equal(main, next) {
					t.Fatal("fixture change was not confined to the WAL")
				}
			case "archive":
				sql(t, sqlite, archived, "UPDATE outbox SET event_id='changed' WHERE seq=2;")
			case "archive metadata":
				path := filepath.Join(dir, archiveName, "migration.json")
				metadata, err := os.ReadFile(path)
				must(t, err)
				must(t, os.WriteFile(path, bytes.ReplaceAll(metadata, []byte(oldID), []byte("jrn_01m6a6ft00e40r2081040g2082")), 0o600))
			case "Signal binding":
				must(t, os.WriteFile(filepath.Join(dir, "app", "settings.json"), []byte(`{"signal":null}`), 0o600))
			}
			before, err := os.ReadFile(archived)
			must(t, err)
			if _, err := Migrate(t.Context(), dir, sqlite); !errors.Is(err, ErrChanged) {
				t.Fatalf("retry accepted changed Swift state: %v", err)
			}
			after, err := os.ReadFile(archived)
			must(t, err)
			if !bytes.Equal(before, after) {
				t.Fatal("retry rewrote the old archive")
			}
		})
	}
}

type recordSink func([]byte) approval.RecordAnswer

func (f recordSink) PostRecord(_ context.Context, b []byte) approval.RecordAnswer { return f(b) }

func TestLiveSocketAndUnconfirmedTetherAreRefused(t *testing.T) {
	dir, sqlite, _ := fixture(t, "")
	lockDir(t, dir)
	run := filepath.Join(dir, "run")
	must(t, os.Mkdir(run, 0o700))
	path := filepath.Join(run, "act.sock")
	l, err := net.Listen("unix", path)
	must(t, err)
	defer l.Close()
	must(t, os.Chmod(path, 0o600))
	if _, err := Migrate(t.Context(), dir, sqlite); err == nil || !strings.Contains(err.Error(), "listener is still running") {
		t.Fatalf("live listener: %v", err)
	}
	must(t, l.Close())
	must(t, os.WriteFile(filepath.Join(dir, "tether.session"), []byte("fixture"), 0o600))
	if _, err := Migrate(t.Context(), dir, sqlite); err == nil || !strings.Contains(err.Error(), "unconfirmed") {
		t.Fatalf("tether marker: %v", err)
	}
}
