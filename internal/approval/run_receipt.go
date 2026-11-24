package approval

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

const receiptName = "exec.receipt"

func execReceipt(dir, runner string, pgid int, token string) (string, string, error) {
	if !ValidID(runner, "run") || pgid <= 1 || !replayNonce.matches(token) {
		return "", "", Invalid("/run_permit")
	}
	runs := filepath.Join(dir, "runs")
	run := filepath.Join(runs, runner)
	for _, path := range []string{runs, run} {
		if _, err := CheckPrivate(path, os.ModeDir|0o700); err != nil {
			return "", "", err
		}
	}
	return run, runner + " " + token + " " + strconv.Itoa(pgid) + "\n", nil
}

// PersistRunExec is only called by the reaper on positive exec evidence, before
// acknowledging Release. The permit nonce binds this receipt to that launch.
func PersistRunExec(dir, runner string, pgid int, token string) error {
	run, body, err := execReceipt(dir, runner, pgid, token)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(run, ".exec-receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := errors.Join(writeReceipt(f, body), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(run, receiptName)); err != nil {
		return err
	}
	for _, path := range []string{run, filepath.Dir(run), dir} {
		d, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = syncParent(d)
		if err := errors.Join(err, d.Close()); err != nil {
			return err
		}
	}
	return nil
}

func writeReceipt(f *os.File, body string) error {
	if _, err := f.WriteString(body); err != nil {
		return err
	}
	return syncJournal(f)
}

func hasRunExec(dir, runner string, pgid int, token string) bool {
	run, want, err := execReceipt(dir, runner, pgid, token)
	if err != nil {
		return false
	}
	f, err := OpenPrivate(filepath.Join(run, receiptName), os.O_RDONLY)
	if err != nil {
		return false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(len(want)+1)))
	return err == nil && string(b) == want
}
