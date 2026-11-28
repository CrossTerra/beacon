package approval

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

const (
	journalName = "journal.log"
	magic       = "BCN1"
	headerSize  = 12
	checksumLen = 4
	maxFrame    = 2 << 20 // a 1 MiB wire body plus base64 and local metadata
)

var ErrCorrupt = errors.New("approval journal is corrupt or from an unsupported format")

// A frame is magic, big-endian length and its complement, JSON payload, CRC32 of the payload. Only a
// short final frame is recoverable; a bad length/checksum or invalid complete entry stops startup.
func appendFrame(f *os.File, entry frame) error {
	body, err := json.Marshal(entry)
	if err != nil || len(body) == 0 || len(body) > maxFrame {
		return ErrCorrupt
	}
	n := uint32(len(body))
	b := make([]byte, 0, headerSize+len(body)+checksumLen)
	b = append(b, magic...)
	b = binary.BigEndian.AppendUint32(b, n)
	b = binary.BigEndian.AppendUint32(b, ^n)
	b = append(b, body...)
	b = binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(body))
	_, err = f.Write(b)
	return err
}

func readFrame(r *bufio.Reader) (frame, int64, error) {
	var entry frame
	b, err := readFrameBody(r)
	if err != nil {
		return entry, 0, err
	}
	if json.Unmarshal(b, &entry) != nil {
		return entry, 0, ErrCorrupt
	}
	return entry, headerSize + int64(len(b)) + checksumLen, nil
}

func readFrameBody(r *bufio.Reader) ([]byte, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[4:8])
	if string(header[:4]) != magic || n == 0 || n > maxFrame || binary.BigEndian.Uint32(header[8:]) != ^n {
		return nil, ErrCorrupt
	}
	b := make([]byte, int(n)+checksumLen)
	if _, err := io.ReadFull(r, b); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF // the header itself was not short
		}
		return nil, err
	}
	if crc32.ChecksumIEEE(b[:n]) != binary.BigEndian.Uint32(b[n:]) {
		return nil, ErrCorrupt
	}
	return b[:n], nil
}

// Publish a complete, synced header by rename under the instance lock. An empty existing journal is never
// treated as new, so damage to its header cannot reset the sequence or generate a new id.
func createJournal(path, id string, sync func(*os.File) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".journal-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := appendFrame(f, frame{Kind: "journal", ID: id}); err != nil {
		f.Close()
		return err
	}
	syncErr := sync(f)
	if err := errors.Join(syncErr, f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	p, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer p.Close()
	// The hook is notified. It is not the flush: on macOS its File.Sync is F_FULLFSYNC, and returning
	// that error would skip the fsync syscall. syncParent owns the directory descriptor.
	sync(p)
	_, err = syncParent(p)
	return err
}

const (
	flushFull = "F_FULLFSYNC"
	flushSync = "fsync"
)

// flushDir reports the call that reached the disk. full is nil where F_FULLFSYNC does not exist.
// Any F_FULLFSYNC failure falls back to plain, which must be the fsync syscall, not os.File.Sync.
func flushDir(f *os.File, full, plain func(int) error) (string, error) {
	fd := int(f.Fd())
	if full != nil {
		if err := full(fd); err == nil {
			return flushFull, nil
		}
	}
	if plain == nil {
		return "", errors.New("directory fsync is unavailable")
	}
	if err := plain(fd); err != nil {
		return "", err
	}
	return flushSync, nil
}
