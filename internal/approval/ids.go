package approval

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

func newID(prefix string, at time.Time) (string, error) {
	ms := at.UnixMilli()
	if ms < 0 || ms >= 1<<48 {
		return "", errors.New("time outside UUIDv7 range")
	}
	var b [16]byte
	binary.BigEndian.PutUint64(b[:], uint64(ms)<<16)
	_, _ = rand.Read(b[6:])
	b[6] = b[6]&15 | 0x70
	b[8] = b[8]&63 | 0x80
	hi, lo := binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
	var encoded [26]byte
	for i := len(encoded) - 1; i >= 0; i-- {
		encoded[i] = crockford[lo&31]
		lo, hi = lo>>5|hi<<59, hi>>5
	}
	return prefix + "_" + string(encoded[:]), nil
}

// ValidID checks the prefix and Signal's Crockford encoding of an RFC 9562 UUIDv7.
func ValidID(id, prefix string) bool {
	s, ok := strings.CutPrefix(id, prefix+"_")
	if !ok || len(s) != 26 || s[0] > '7' {
		return false
	}
	var hi, lo uint64
	for i := range len(s) {
		d := strings.IndexByte(crockford, s[i])
		if d < 0 {
			return false
		}
		hi, lo = hi<<5|lo>>59, lo<<5|uint64(d)
	}
	return (hi>>12)&15 == 7 && lo>>62 == 2
}

func nonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
