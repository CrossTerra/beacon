package webauthn

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"math/big"
)

// PublicKey is a COSE key that parsed and is on its curve.
type PublicKey struct {
	Algorithm Algorithm
	COSE      []byte
	key       any
}

// ParseCOSEKey decodes a COSE_Key and refuses a key that is not ES256, EdDSA
// or RS256, or that is not on its curve.
func ParseCOSEKey(raw []byte) (PublicKey, error) {
	if len(raw) == 0 || len(raw) > maxAuthData {
		return PublicKey{}, errCode(CodePublicKey)
	}
	var fields map[int64]cborRaw
	rest, err := unmarshalOne(raw, &fields)
	if err != nil || len(rest) != 0 || fields == nil {
		return PublicKey{}, errCode(CodePublicKey)
	}
	kty, err := cborInt(fields[1])
	if err != nil {
		return PublicKey{}, errCode(CodePublicKey)
	}
	alg, err := cborInt(fields[3])
	if err != nil {
		return PublicKey{}, errCode(CodePublicKey)
	}
	switch Algorithm(alg) {
	case AlgES256:
		if kty != 2 { // EC2
			return PublicKey{}, errCode(CodePublicKey)
		}
		return parseES256(raw, fields)
	case AlgEdDSA:
		if kty != 1 { // OKP
			return PublicKey{}, errCode(CodePublicKey)
		}
		return parseEdDSA(raw, fields)
	case AlgRS256:
		if kty != 3 { // RSA
			return PublicKey{}, errCode(CodePublicKey)
		}
		return parseRS256(raw, fields)
	default:
		return PublicKey{}, errCode(CodePublicKey)
	}
}

func parseES256(raw []byte, fields map[int64]cborRaw) (PublicKey, error) {
	crv, err := cborInt(fields[-1])
	if err != nil || crv != 1 { // P-256
		return PublicKey{}, errCode(CodePublicKey)
	}
	x, err := cborBytes(fields[-2])
	if err != nil || len(x) != 32 {
		return PublicKey{}, errCode(CodePublicKey)
	}
	y, err := cborBytes(fields[-3])
	if err != nil || len(y) != 32 {
		return PublicKey{}, errCode(CodePublicKey)
	}
	uncompressed := make([]byte, 65)
	uncompressed[0] = 4
	copy(uncompressed[1:33], x)
	copy(uncompressed[33:], y)
	// NewPublicKey rejects the point at infinity, compressed points and a
	// point that is not on P-256. elliptic.Unmarshal is deprecated.
	if _, err := ecdh.P256().NewPublicKey(uncompressed); err != nil {
		return PublicKey{}, errCode(CodePublicKey)
	}
	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}
	return PublicKey{Algorithm: AlgES256, COSE: append([]byte(nil), raw...), key: pub}, nil
}

func parseEdDSA(raw []byte, fields map[int64]cborRaw) (PublicKey, error) {
	crv, err := cborInt(fields[-1])
	if err != nil || crv != 6 { // Ed25519
		return PublicKey{}, errCode(CodePublicKey)
	}
	x, err := cborBytes(fields[-2])
	if err != nil || !ed25519Point(x) {
		return PublicKey{}, errCode(CodePublicKey)
	}
	pub := ed25519.PublicKey(append([]byte(nil), x...))
	return PublicKey{Algorithm: AlgEdDSA, COSE: append([]byte(nil), raw...), key: pub}, nil
}

func parseRS256(raw []byte, fields map[int64]cborRaw) (PublicKey, error) {
	nBytes, err := cborBytes(fields[-1])
	if err != nil || len(nBytes) < 256 { // 2048-bit minimum
		return PublicKey{}, errCode(CodePublicKey)
	}
	eBytes, err := cborBytes(fields[-2])
	if err != nil || len(eBytes) == 0 || len(eBytes) > 4 {
		return PublicKey{}, errCode(CodePublicKey)
	}
	n := new(big.Int).SetBytes(nBytes)
	if n.BitLen() < 2048 || n.Sign() <= 0 {
		return PublicKey{}, errCode(CodePublicKey)
	}
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() {
		return PublicKey{}, errCode(CodePublicKey)
	}
	ev := e.Int64()
	if ev < 3 || ev%2 == 0 || ev > 1<<31-1 {
		return PublicKey{}, errCode(CodePublicKey)
	}
	pub := &rsa.PublicKey{N: n, E: int(ev)}
	return PublicKey{Algorithm: AlgRS256, COSE: append([]byte(nil), raw...), key: pub}, nil
}

// cborRaw is a decoded CBOR value. Integer keys decode to int64 under
// IntDecConvertNone; byte strings decode to []byte; nested maps are rare in a
// COSE key and are left as whatever the decoder produced.
type cborRaw = any

func cborInt(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case uint64:
		if n > 1<<63-1 {
			return 0, errCode(CodePublicKey)
		}
		return int64(n), nil
	default:
		return 0, errCode(CodePublicKey)
	}
}

func cborBytes(v any) ([]byte, error) {
	b, ok := v.([]byte)
	if !ok {
		return nil, errCode(CodePublicKey)
	}
	return b, nil
}
