package actionexec

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/CrossTerra/beacon/internal/approval"
	"golang.org/x/sys/unix"
)

const runSettingsName = "agent-runs.json"

type Checkout struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type RunSettings struct {
	Opencode  string     `json:"opencode"`
	Models    []string   `json:"models"`
	Checkouts []Checkout `json:"checkouts"`
}

var errRunSettings = errors.New("invalid agent-runs.json")

// LoadRunSettings reads only explicit owner settings. Resolving a proposal never runs a login
// shell, Git hook or OpenCode plugin to decide which models or paths a person may approve.
func LoadRunSettings(dir string) (*RunSettings, error) {
	f, err := approval.OpenPrivate(filepath.Join(dir, runSettingsName), os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errRunSettings
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(b) > 64<<10 {
		return nil, errRunSettings
	}
	if _, err := approval.ParseObject(b); err != nil {
		return nil, errRunSettings
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var s RunSettings
	if d.Decode(&s) != nil || !absolutePath(s.Opencode) || strings.ContainsRune(s.Opencode, ':') || len(s.Models) == 0 || len(s.Models) > 128 ||
		len(s.Checkouts) == 0 || len(s.Checkouts) > 64 {
		return nil, errRunSettings
	}
	models, names := make(map[string]bool), make(map[string]bool)
	for _, model := range s.Models {
		if strings.HasPrefix(model, "-") || models[model] || approval.Run.CheckParams(approval.Object{
			"checkout": "test", "prompt": "p", "model": model,
		}) != nil {
			return nil, errRunSettings
		}
		models[model] = true
	}
	for i, c := range s.Checkouts {
		if !absolutePath(c.Path) || names[c.Name] || approval.Run.CheckParams(approval.Object{
			"checkout": c.Name, "prompt": "p", "model": s.Models[0],
		}) != nil {
			return nil, errRunSettings
		}
		names[c.Name] = true
		// Keep a missing checkout in settings, but never resolve an approval for it until present.
		if path, err := filepath.EvalSymlinks(c.Path); err == nil && absolutePath(path) {
			s.Checkouts[i].Path = path
		}
	}
	return &s, nil
}

func absolutePath(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) == s && len(s) <= 4096 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func executableFile(path string) bool {
	s, err := os.Stat(path)
	return err == nil && s.Mode().IsRegular() && unix.Access(path, unix.X_OK) == nil
}
