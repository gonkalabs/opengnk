package signer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// Signer produces ECDSA-SHA256 signatures over secp256k1, matching the
// official gonka-openai Python SDK v0.2.4 signing scheme exactly.
type Signer struct {
	key *ecdsa.PrivateKey
}

// New creates a Signer from a hex-encoded private key (0x prefix optional).
func New(hexKey string) (*Signer, error) {
	hexKey = strings.TrimPrefix(hexKey, "0x")
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("signer: invalid hex key: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("signer: key must be 32 bytes, got %d", len(raw))
	}
	key, err := crypto.ToECDSA(raw)
	if err != nil {
		return nil, fmt.Errorf("signer: %w", err)
	}
	return &Signer{key: key}, nil
}

// Sign returns (base64-encoded signature, timestamp in nanoseconds).
//
// Signing scheme (matching Python SDK v0.2.4):
//   1. payload_hash = hex(SHA256(payload_bytes))
//   2. signature_input = payload_hash + str(timestamp_ns) + transfer_address
//   3. Sign SHA256(signature_input) with deterministic ECDSA (RFC 6979), low-S normalised
//   4. Encode r(32 bytes) || s(32 bytes) as base64
func (s *Signer) Sign(payload []byte, transferAddress string) (sig string, tsNano int64) {
	ts := time.Now().UnixNano()

	// Step 1: SHA256 hash of payload, then hex encode
	payloadHash := sha256.Sum256(payload)
	payloadHex := hex.EncodeToString(payloadHash[:])

	// Step 2: Build signature input string
	tsStr := fmt.Sprintf("%d", ts)
	sigInput := payloadHex + tsStr + transferAddress

	// Step 3: Deterministic ECDSA (RFC 6979) sign of SHA256(sigInput)
	msgHash := sha256.Sum256([]byte(sigInput))
	r, sBig := rfc6979Sign(s.key, msgHash[:])

	// Low-S normalisation
	curveOrder := s.key.Params().N
	halfOrder := new(big.Int).Rsh(curveOrder, 1)
	if sBig.Cmp(halfOrder) > 0 {
		sBig = new(big.Int).Sub(curveOrder, sBig)
	}

	// Step 4: Encode r||s as 64 bytes (zero-padded to 32 each), base64
	out := make([]byte, 64)
	rBytes := r.Bytes()
	sBytes := sBig.Bytes()
	copy(out[32-len(rBytes):32], rBytes)
	copy(out[64-len(sBytes):64], sBytes)

	return base64.StdEncoding.EncodeToString(out), ts
}

// rfc6979Sign implements deterministic ECDSA signing per RFC 6979.
// This matches Python's ecdsa library sign_deterministic with SHA-256.
func rfc6979Sign(key *ecdsa.PrivateKey, hash []byte) (*big.Int, *big.Int) {
	curve := key.Curve
	N := curve.Params().N
	D := key.D

	// RFC 6979 deterministic nonce generation
	k := generateRFC6979K(N, D, hash)

	// Standard ECDSA: (x1, _) = k*G; r = x1 mod n
	rx, _ := curve.ScalarBaseMult(k.Bytes())
	r := new(big.Int).Mod(rx, N)

	// s = k^-1 * (hash + r*D) mod n
	kInv := new(big.Int).ModInverse(k, N)
	e := new(big.Int).SetBytes(hash)
	s := new(big.Int).Mul(r, D)
	s.Add(s, e)
	s.Mul(s, kInv)
	s.Mod(s, N)

	return r, s
}

// generateRFC6979K generates a deterministic k value per RFC 6979.
func generateRFC6979K(N, D *big.Int, hash []byte) *big.Int {
	qlen := N.BitLen()
	holen := sha256.Size // 32 bytes for SHA-256

	// Step a: h1 = hash (already provided)
	// Ensure hash is exactly qlen bits
	bx := int2octets(D, qlen)
	bh := bits2octets(hash, N, qlen)

	// Step b: V = 0x01 0x01 ... (holen bytes)
	v := make([]byte, holen)
	for i := range v {
		v[i] = 0x01
	}

	// Step c: K = 0x00 0x00 ... (holen bytes)
	kk := make([]byte, holen)

	// Step d: K = HMAC_K(V || 0x00 || int2octets(x) || bits2octets(h1))
	mac := hmac.New(sha256.New, kk)
	mac.Write(v)
	mac.Write([]byte{0x00})
	mac.Write(bx)
	mac.Write(bh)
	kk = mac.Sum(nil)

	// Step e: V = HMAC_K(V)
	mac = hmac.New(sha256.New, kk)
	mac.Write(v)
	v = mac.Sum(nil)

	// Step f: K = HMAC_K(V || 0x01 || int2octets(x) || bits2octets(h1))
	mac = hmac.New(sha256.New, kk)
	mac.Write(v)
	mac.Write([]byte{0x01})
	mac.Write(bx)
	mac.Write(bh)
	kk = mac.Sum(nil)

	// Step g: V = HMAC_K(V)
	mac = hmac.New(sha256.New, kk)
	mac.Write(v)
	v = mac.Sum(nil)

	// Step h: Generate k
	for {
		var t []byte
		for len(t)*8 < qlen {
			mac = hmac.New(sha256.New, kk)
			mac.Write(v)
			v = mac.Sum(nil)
			t = append(t, v...)
		}

		secret := bits2int(t, qlen)
		if secret.Sign() > 0 && secret.Cmp(N) < 0 {
			return secret
		}

		// k is not suitable, update K and V
		mac = hmac.New(sha256.New, kk)
		mac.Write(v)
		mac.Write([]byte{0x00})
		kk = mac.Sum(nil)

		mac = hmac.New(sha256.New, kk)
		mac.Write(v)
		v = mac.Sum(nil)
	}
}

func int2octets(v *big.Int, qlen int) []byte {
	rlen := (qlen + 7) / 8
	out := v.Bytes()
	if len(out) < rlen {
		pad := make([]byte, rlen-len(out))
		out = append(pad, out...)
	}
	if len(out) > rlen {
		out = out[len(out)-rlen:]
	}
	return out
}

func bits2int(b []byte, qlen int) *big.Int {
	v := new(big.Int).SetBytes(b)
	blen := len(b) * 8
	if blen > qlen {
		v.Rsh(v, uint(blen-qlen))
	}
	return v
}

func bits2octets(b []byte, q *big.Int, qlen int) []byte {
	z1 := bits2int(b, qlen)
	z2 := new(big.Int).Sub(z1, q)
	if z2.Sign() < 0 {
		z2 = z1
	}
	return int2octets(z2, qlen)
}

// Ensure the curve is secp256k1 (unused but kept for verification).
var _ elliptic.Curve = crypto.S256()
