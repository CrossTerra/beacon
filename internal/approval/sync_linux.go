package approval

import (
	"os"

	"golang.org/x/sys/unix"
)

func syncJournal(f *os.File) error { return f.Sync() }

func syncParent(f *os.File) (string, error) {
	return flushDir(f, nil, unix.Fsync)
}
