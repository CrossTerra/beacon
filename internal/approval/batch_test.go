package approval

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBatchWithoutACommitMarkerIsNotApplied(t *testing.T) {
	dir := scratch(t)
	j := open(t, dir, t0)
	a, b := put(t, j, mergeRequest()), put(t, j, mergeRequest())
	must(t, j.Close())
	f, err := OpenPrivate(filepath.Join(dir, journalName), os.O_RDWR|os.O_APPEND)
	must(t, err)
	challenge := bytes.Repeat([]byte{7}, 32)
	cred := []byte("cred")
	for i, id := range []string{a.ID, b.ID} {
		must(t, appendFrame(f, frame{
			Kind: "approved", ID: id, Seq: int64(3 + i), Nonce: strings.Repeat("ab", 16),
			At: t0.UnixMilli(), BatchChallenge: challenge, CredentialID: cred,
			Frozen: &Frozen{ID: id, Params: Object{}, Binding: Binding{Destination: Object{}}},
		}))
	}
	must(t, f.Close())
	j = open(t, dir, t0.Add(timeSecond))
	status(t, j, a.ID, t0, "pending")
	status(t, j, b.ID, t0, "pending")
	body, err := os.ReadFile(filepath.Join(dir, journalName))
	must(t, err)
	if bytes.Contains(body, []byte("batch_challenge")) {
		t.Fatal("replay kept an uncommitted batch")
	}
}

func TestApprovedFrameRecordsTheCredentialAndTheBatchChallenge(t *testing.T) {
	dir := scratch(t)
	j := open(t, dir, t0)
	a, b := put(t, j, mergeRequest()), put(t, j, mergeRequest())
	cred := []byte{1, 2, 3, 4}
	challenge := bytes.Repeat([]byte{9}, 32)
	must(t, j.ApproveAll([]Frozen{freeze(t, a, ""), freeze(t, b, "")}, cred, challenge, t0))
	body, err := os.ReadFile(filepath.Join(dir, journalName))
	must(t, err)
	if !bytes.Contains(body, []byte(`"credential_id"`)) || !bytes.Contains(body, []byte(`"batch_challenge"`)) || !bytes.Contains(body, []byte(`"approved_commit"`)) {
		t.Fatal("batch approval was not one committed decision")
	}
	must(t, j.Close())
	j = open(t, dir, t0.Add(timeSecond))
	status(t, j, a.ID, t0.Add(timeSecond), "expired")
	status(t, j, b.ID, t0.Add(timeSecond), "expired")
}

const timeSecond = 1_000_000_000
