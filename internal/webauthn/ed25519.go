package webauthn

import "math/big"

// ed25519Point reports whether pub is a canonical Ed25519 point encoding.
// crypto/ed25519 does not export the check. The public key is not secret, so
// math/big is enough: this only runs when a credential is registered.
//
// The arithmetic is RFC 8032 section 5.1.3. p = 2^255 - 19, and p ≡ 5 (mod 8),
// so a square root is a^((p+3)/8).
func ed25519Point(pub []byte) bool {
	if len(pub) != 32 {
		return false
	}
	p := ed25519P()
	y := new(big.Int).SetBytes(reverse32(pub))
	sign := new(big.Int).Rsh(y, 255)
	y.And(y, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(1)))
	if y.Cmp(p) >= 0 {
		return false
	}
	d := ed25519D(p)
	y2 := new(big.Int).Mul(y, y)
	y2.Mod(y2, p)
	u := new(big.Int).Sub(y2, big.NewInt(1))
	u.Mod(u, p)
	v := new(big.Int).Mul(d, y2)
	v.Add(v, big.NewInt(1))
	v.Mod(v, p)
	if v.Sign() == 0 {
		return false
	}
	// x^2 = u/v. Candidate x = (u/v)^((p+3)/8).
	vinv := new(big.Int).ModInverse(v, p)
	if vinv == nil {
		return false
	}
	x2 := new(big.Int).Mul(u, vinv)
	x2.Mod(x2, p)
	exp := new(big.Int).Add(p, big.NewInt(3))
	exp.Rsh(exp, 3)
	x := new(big.Int).Exp(x2, exp, p)
	vx2 := new(big.Int).Mul(v, new(big.Int).Mul(x, x))
	vx2.Mod(vx2, p)
	if vx2.Cmp(u) != 0 {
		// v*x^2 == -u means multiply by 2^((p-1)/4).
		negU := new(big.Int).Neg(u)
		negU.Mod(negU, p)
		if vx2.Cmp(negU) != 0 {
			return false
		}
		sq := new(big.Int).Sub(p, big.NewInt(1))
		sq.Rsh(sq, 2)
		x.Mul(x, new(big.Int).Exp(big.NewInt(2), sq, p))
		x.Mod(x, p)
		vx2.Mul(v, new(big.Int).Mul(x, x))
		vx2.Mod(vx2, p)
		if vx2.Cmp(u) != 0 {
			return false
		}
	}
	if x.Sign() == 0 && sign.Sign() != 0 {
		return false
	}
	if new(big.Int).And(x, big.NewInt(1)).Cmp(sign) != 0 {
		x.Sub(p, x)
	}
	return true
}

func ed25519P() *big.Int {
	p := new(big.Int).Lsh(big.NewInt(1), 255)
	return p.Sub(p, big.NewInt(19))
}

func ed25519D(p *big.Int) *big.Int {
	// d = -121665 * inv(121666) mod p
	inv := new(big.Int).ModInverse(big.NewInt(121666), p)
	d := new(big.Int).Mul(big.NewInt(-121665), inv)
	return d.Mod(d, p)
}

func reverse32(b []byte) []byte {
	out := make([]byte, 32)
	for i := 0; i < 32; i++ {
		out[i] = b[31-i]
	}
	return out
}
