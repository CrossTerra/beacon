// Package localstate holds the service's paths and its one persisted browser origin.
package localstate

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/CrossTerra/beacon/internal/approval"
)

const settingsFile = "settings.json"

func DefaultDir() (string, error) {
	if runtime.GOOS == "darwin" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "beacon"), nil
	}
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) { // The XDG spec ignores relative paths.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "beacon"), nil
}

// Dir canonicalizes ancestors, never the state directory itself. Only setup may create it.
func Dir(path string, create bool) (string, error) {
	var err error
	if path == "" {
		path, err = DefaultDir()
	} else {
		path, err = filepath.Abs(path)
	}
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(path)
	if create {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return "", err
		}
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(parent, filepath.Base(path))
	if create {
		err = approval.PrivateDir(dir)
	} else {
		_, err = approval.CheckPrivate(dir, os.ModeDir|0o700)
	}
	return dir, err
}

type settings struct {
	Origin string `json:"origin"`
	Login  string `json:"tailscale_login,omitempty"`
}

func Load(dir string) (approval.Origin, error) {
	o, _, err := LoadSnapshot(dir)
	return o, err
}

// LoadSnapshot returns the origin and the digest of exactly the bytes validated.
func LoadSnapshot(dir string) (approval.Origin, [32]byte, error) {
	f, err := approval.OpenPrivate(filepath.Join(dir, settingsFile), os.O_RDONLY)
	if err != nil {
		return approval.Origin{}, [32]byte{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return approval.Origin{}, [32]byte{}, errors.New("could not read settings.json")
	}
	if obj, err := approval.ParseObject(b); err != nil || approval.CheckKeys(obj, "", "origin", "tailscale_login") != nil {
		return approval.Origin{}, [32]byte{}, errors.New("invalid settings.json")
	}
	var s settings
	if err := json.Unmarshal(b, &s); err != nil {
		return approval.Origin{}, [32]byte{}, errors.New("invalid settings.json")
	}
	o, err := approval.ParseOrigin(s.Origin, s.Login)
	return o, sha256.Sum256(b), err
}

// Save writes the fixed settings shape, including the origin on a separate line for the installer's preflight.
// The caller holds the instance lock.
func Save(dir string, origin approval.Origin) error {
	b, err := json.MarshalIndent(settings{origin.URL(), origin.TailscaleLogin()}, "", "  ")
	if err != nil {
		return err
	}
	return approval.WritePrivate(filepath.Join(dir, settingsFile), append(b, '\n'))
}
