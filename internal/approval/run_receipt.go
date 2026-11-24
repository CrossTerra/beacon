package approval

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

const receiptName = "exec.receipt"

func receiptDir(dir, runner string) (string, error) {
	if !ValidID(runner, "run") {
		return "", Invalid("/run_permit")
	}
	runs := filepath.Join(dir, "runs")
	run := filepath.Join(runs, runner)
	for _, path := range []string{runs, run} {
		if _, err := CheckPrivate(path, os.ModeDir|0o700); err != nil {
			return "", err
		}
	}
	return run, nil
}

func receiptBody(runner string, pgid int, token string) string {
	return runner + " " + token + " " + strconv.Itoa(pgid) + "\n"
}

// PersistRunExec is only called by the reaper on positive exec evidence, before
// acknowledging Release. The permit nonce binds this receipt to that launch.
func PersistRunExec(dir, runner string, pgid int, token string) error {
	if pgid <= 1 || !replayNonce.matches(token) {
		return Invalid("/run_permit")
	}
	run, err := receiptDir(dir, runner)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(run, ".exec-receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := errors.Join(writeReceipt(f, receiptBody(runner, pgid, token)), f.Close()); err != nil {
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

func hasRunExec(dir, runner string, commitment []byte) bool {
	if len(commitment) != sha256.Size {
		return false // a legacy permit cannot establish exec
	}
	run, err := receiptDir(dir, runner)
	if err != nil {
		return false
	}
	f, err := OpenPrivate(filepath.Join(run, receiptName), os.O_RDONLY)
	if err != nil {
		return false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 128)) // receipts are shorter than this, including the longest pid
	sum := sha256.Sum256(b)
	return err == nil && len(b) < 128 && subtle.ConstantTimeCompare(sum[:], commitment) == 1
}
