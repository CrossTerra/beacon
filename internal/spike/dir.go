package spike

import (
	"os"
	"path/filepath"
	"runtime"
)

// DefaultDataDir is where the spike keeps its credential file when --data is omitted.
// It is not Beacon's state directory.
func DefaultDataDir() (string, error) {
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "beacon-spike"), nil
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "beacon-spike"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "beacon-spike"), nil
}
