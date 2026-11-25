package vault

import (
	"context"

	"golang.org/x/sys/unix"
)

func saveKeyToSession(context.Context, string, []byte) (bool, error) { return false, nil }
func loadSessionKey(string) ([]byte, error)                          { return nil, ErrKeyMissing }

func excludeFromBackup(path string) error {
	// Time Machine's sticky exclusion: a binary plist with the string "com.apple.backupd".
	const value = "bplist00_\x10\x11com.apple.backupd\x08\x00\x00\x00\x00\x00\x00\x01\x01" +
		"\x00\x00\x00\x00\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x1c"
	return unix.Setxattr(path, "com.apple.metadata:com_apple_backup_excludeItem", []byte(value), 0)
}
