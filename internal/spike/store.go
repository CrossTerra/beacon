package spike

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/CrossTerra/beacon/internal/webauthn"
)

const maxCredentials = 16

// credential is a registered passkey. PRFHash is the SHA-256 of the first PRF
// output, hex. The output itself is never stored.
type credential struct {
	ID        string    `json:"id"`
	RPID      string    `json:"rp_id"`
	Algorithm int64     `json:"alg"`
	COSEKey   string    `json:"cose_key"`
	SignCount uint32    `json:"sign_count"`
	PRFSalt   string    `json:"prf_salt"`
	PRFHash   string    `json:"prf_hash,omitempty"`
	AAGUID    string    `json:"aaguid"`
	Added     time.Time `json:"added_at"`
}

type diskState struct {
	UserID      string       `json:"user_id"`
	Credentials []credential `json:"credentials"`
}

func loadState(dir string) (diskState, error) {
	if err := checkOwned(dir); err != nil {
		return diskState{}, err
	}
	path := filepath.Join(dir, "credentials.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		id := make([]byte, 32)
		if _, err := rand.Read(id); err != nil {
			return diskState{}, err
		}
		st := diskState{UserID: base64.RawURLEncoding.EncodeToString(id)}
		return st, saveState(dir, st)
	}
	if err != nil {
		return diskState{}, err
	}
	if err := checkOwned(path); err != nil {
		return diskState{}, err
	}
	var st diskState
	if err := json.Unmarshal(b, &st); err != nil || st.UserID == "" {
		return diskState{}, errState
	}
	for _, c := range st.Credentials {
		raw, err := base64.RawURLEncoding.DecodeString(c.COSEKey)
		if err != nil {
			return diskState{}, errState
		}
		if _, err := webauthn.ParseCOSEKey(raw); err != nil {
			return diskState{}, errState
		}
	}
	return st, nil
}

func saveState(dir string, st diskState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if err := checkOwned(dir); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := filepath.Join(dir, "credentials.json")
	if err := checkOwned(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// The temporary file is already 0600; rename commits the save.
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func checkOwned(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errSymlink
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return errOwner
	}
	return nil
}

var (
	errState   = errors.New("spike: credential file is not usable")
	errSymlink = errors.New("spike: refusing a symlink")
	errOwner   = errors.New("spike: refusing a file owned by someone else")
)
