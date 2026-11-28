package localstate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/config"
	"github.com/CrossTerra/beacon/internal/vault"
)

const (
	SignalPreparationDir = "signal-onboarding"
	signalManifest       = "connection.json"
	signalConfig         = "config.yaml"
	signalRules          = "rules.yaml"
)

var errSignalPreparation = errors.New("invalid or changed Signal preparation; keep it offline and do not replace its files")

// SignalPreparation is nonsecret and inert without the corresponding KDBX commit. Do not
// add mutable collector options here: the starter is always source-free and default-deny.
type SignalPreparation struct {
	Version        int    `json:"version"`
	Origin         string `json:"origin"`
	Person         string `json:"person_id"`
	Handle         string `json:"handle"`
	ApprovalActor  string `json:"approval_actor"`
	CollectorActor string `json:"collector_actor"`
	Journal        string `json:"journal"`
	LocalBase      string `json:"local_base"`
	VaultBase      string `json:"vault_base"`
}

func (p SignalPreparation) files() (manifest, collector, rules []byte, err error) {
	if p.Version != 1 || !strings.HasPrefix(p.Origin, "https://") || !approval.ValidID(p.Journal, "jrn") ||
		!approval.ValidID(p.Person, "act") || !approval.ValidSignalHandle(p.Handle) ||
		!approval.ValidID(p.ApprovalActor, "act") || !approval.ValidID(p.CollectorActor, "act") ||
		p.Person == p.ApprovalActor || p.Person == p.CollectorActor || p.ApprovalActor == p.CollectorActor ||
		!hexDigest(p.LocalBase) || !hexDigest(p.VaultBase) {
		return nil, nil, nil, errSignalPreparation
	}
	if _, err := approval.RecordEgress(p.Origin); err != nil {
		return nil, nil, nil, errSignalPreparation
	}
	cfg := p.starter()
	b, err := cfg.Marshal()
	if err != nil {
		return nil, nil, nil, errSignalPreparation
	}
	m, err := json.Marshal(p)
	return append(m, '\n'), b, []byte("rules: []\n"), err
}

func hexDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

func (p SignalPreparation) starter() config.Config {
	return config.Config{Signal: config.Signal{URL: p.Origin}, Person: config.Person{Handle: "@" + p.Handle},
		Rules: signalRules, State: "collector", Mode: config.DryRun, Sources: []config.Source{}}
}

func (p SignalPreparation) Pair() vault.SignalPair {
	b, cfg, rules, err := p.files()
	if err != nil {
		return vault.SignalPair{}
	}
	h := sha256.New()
	h.Write(b)
	h.Write(cfg)
	h.Write(rules)
	starter := p.starter()
	return vault.SignalPair{Origin: p.Origin, Person: p.Person, ApprovalActor: p.ApprovalActor, CollectorActor: p.CollectorActor,
		CollectorAccount: starter.KeychainAccount(), Activation: [32]byte(h.Sum(nil))}
}

// CheckInstance checks the durable attachment without replay or inspecting later action frames.
func (p SignalPreparation) CheckInstance(dir string) error {
	o, err := Load(dir)
	if err != nil || o.URL() != approval.LocalOrigin || approval.CheckJournalID(dir, p.Journal) != nil {
		return errSignalPreparation
	}
	return nil
}

func SignalCollectorPath(dir string) string {
	return filepath.Join(dir, SignalPreparationDir, signalConfig)
}

// SignalCollectorDir recognizes the reserved starter path, not a copied or alternate config.
func SignalCollectorDir(path string) (string, bool) {
	parent := filepath.Dir(path)
	return filepath.Dir(parent), filepath.Base(parent) == SignalPreparationDir && filepath.Base(path) == signalConfig
}

func (p SignalPreparation) CollectorConfig(dir string) *config.Config {
	c := p.starter()
	c.Path = SignalCollectorPath(dir)
	return &c
}

// HasSignalPreparation detects the reserved name even if its contents are damaged.
// Legacy setup must not treat an unreadable preparation as an unbound instance.
func HasSignalPreparation(dir string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(dir, SignalPreparationDir)); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, errSignalPreparation
	}
	return false, nil
}

// LoadSignalPreparation is read-only and never opens the vault. A half-written or edited
// canonical directory is an error, not fallback authorization for a legacy key.
func LoadSignalPreparation(dir string) (*SignalPreparation, error) {
	path := filepath.Join(dir, SignalPreparationDir)
	if _, err := approval.CheckPrivate(path, os.ModeDir|0o700); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, errSignalPreparation
	}
	b, err := readSignalFile(filepath.Join(path, signalManifest))
	var p SignalPreparation
	if err != nil || json.Unmarshal(b, &p) != nil {
		return nil, errSignalPreparation
	}
	m, c, r, err := p.files()
	if err != nil || !bytes.Equal(b, m) {
		return nil, errSignalPreparation
	}
	for _, file := range []struct {
		name string
		body []byte
	}{{signalConfig, c}, {signalRules, r}} {
		got, err := readSignalFile(filepath.Join(path, file.name))
		if err != nil || !bytes.Equal(got, file.body) {
			return nil, errSignalPreparation
		}
	}
	return &p, nil
}

func readSignalFile(path string) ([]byte, error) {
	f, err := approval.OpenPrivate(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const limit = 4096
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(b) > limit {
		return nil, errSignalPreparation
	}
	return b, nil
}

// PrepareSignal publishes a complete directory without replacing even an empty destination.
// A crash before rename leaves only ignored, nonsecret files. A crash after rename leaves
// this exact preparation available for re-verification, never an independently active config.
func PrepareSignal(lock *approval.OfflineLock, p SignalPreparation, before, after func() error) error {
	if err := lock.Check(); err != nil {
		return err
	}
	m, c, r, err := p.files()
	if err != nil {
		return err
	}
	dir := lock.Dir()
	stage, err := os.MkdirTemp(dir, "."+SignalPreparationDir+"-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(stage) }()
	for _, file := range []struct {
		name string
		body []byte
	}{{signalManifest, m}, {signalConfig, c}, {signalRules, r}} {
		path := filepath.Join(stage, file.name)
		f, err := approval.OpenPrivate(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			return err
		}
		defer os.Remove(path)
		_, err = f.Write(file.body)
		if err := errors.Join(err, approval.SyncPrivateFile(f), f.Close()); err != nil {
			return err
		}
	}
	if err := approval.FlushDirectory(stage); err != nil {
		return err
	}
	if before != nil {
		if err := before(); err != nil {
			return err
		}
	}
	if err := lock.Check(); err != nil {
		return err
	}
	if err := renameSignalPreparation(stage, filepath.Join(dir, SignalPreparationDir)); err != nil {
		return err
	}
	if after != nil {
		if err := after(); err != nil {
			return err
		}
	}
	return approval.FlushDirectory(dir)
}
