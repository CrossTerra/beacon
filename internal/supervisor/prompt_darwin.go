package supervisor

import (
	"os"

	"github.com/CrossTerra/beacon/internal/approval"
	"golang.org/x/sys/unix"
)

// Unlink the empty file before putting any prompt bytes into it. The child
// gets the read-only descriptor at offset zero, never the writer or a path.
func stagePrompt(input []byte) (*os.File, error) {
	w, err := os.CreateTemp("", ".beacon-prompt-")
	if err != nil {
		return nil, err
	}
	defer w.Close()
	unlinked := false
	defer func() {
		if !unlinked {
			_ = os.Remove(w.Name())
		}
	}()
	r, err := approval.OpenPrivate(w.Name(), os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(w.Name()); err != nil {
		r.Close()
		return nil, err
	}
	unlinked = true
	var writer, reader unix.Stat_t
	if unix.Fstat(int(w.Fd()), &writer) != nil || unix.Fstat(int(r.Fd()), &reader) != nil ||
		writer.Nlink != 0 || writer.Dev != reader.Dev || writer.Ino != reader.Ino {
		r.Close()
		return nil, ErrLaunch
	}
	if _, err := w.Write(input); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
