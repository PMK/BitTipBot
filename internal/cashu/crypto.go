package cashu

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// Domain separator per NUT-00 specification for hash_to_curve.
var domainSeparator = []byte("Secp256k1_HashToCurve_Cashu_")

// HashToCurve deterministically maps a message to a point on the secp256k1 curve.
// Follows the NUT-00 specification: SHA256(domain_separator || msg), then
// try interpreting as x-coordinate with even y (02 prefix), incrementing a
// counter byte until a valid point is found.
func HashToCurve(message []byte) (*btcec.PublicKey, error) {
	msgHash := sha256.Sum256(append(domainSeparator, message...))
	for counter := uint32(0); counter < 65536; counter++ {
		// hash = SHA256(msgHash || counter_le_bytes)
		counterBytes := []byte{byte(counter), byte(counter >> 8), byte(counter >> 16), byte(counter >> 24)}
		h := sha256.Sum256(append(msgHash[:], counterBytes...))
		// Try to parse as compressed point with 0x02 prefix (even y)
		compressed := make([]byte, 33)
		compressed[0] = 0x02
		copy(compressed[1:], h[:])
		pk, err := btcec.ParsePubKey(compressed)
		if err == nil {
			return pk, nil
		}
	}
	return nil, fmt.Errorf("could not find valid point for hash_to_curve")
}

// BlindingResult holds the output of the blinding step.
type BlindingResult struct {
	B_     *btcec.PublicKey  // blinded message point
	R      *btcec.PrivateKey // blinding factor (needed for unblinding)
	Secret string            // the original secret
}

// BlindMessage blinds a secret for sending to the mint (Step 1 / Alice).
// B_ = Y + r*G, where Y = hash_to_curve(secret), r is a random blinding factor.
func BlindMessage(secret string) (*BlindingResult, error) {
	Y, err := HashToCurve([]byte(secret))
	if err != nil {
		return nil, fmt.Errorf("hash_to_curve failed: %w", err)
	}

	// Generate random blinding factor r. A zero r would send Y (and thus the
	// secret's point) to the mint unblinded, so reject it outright.
	var r *btcec.PrivateKey
	for {
		rBytes := make([]byte, 32)
		if _, err := rand.Read(rBytes); err != nil {
			return nil, fmt.Errorf("failed to generate random blinding factor: %w", err)
		}
		r, _ = btcec.PrivKeyFromBytes(rBytes)
		if !r.Key.IsZero() {
			break
		}
	}

	// r*G (the public key corresponding to r)
	rG := r.PubKey()

	// B_ = Y + r*G
	var yJ, rGJ, bJ secp256k1.JacobianPoint
	Y.AsJacobian(&yJ)
	rG.AsJacobian(&rGJ)
	secp256k1.AddNonConst(&yJ, &rGJ, &bJ)
	bJ.ToAffine()
	B_ := btcec.NewPublicKey(&bJ.X, &bJ.Y)

	return &BlindingResult{
		B_:     B_,
		R:      r,
		Secret: secret,
	}, nil
}

// UnblindSignature removes the blinding factor from the mint's signature (Step 3 / Alice).
// C = C_ - r*K, where K is the mint's public key for this denomination.
func UnblindSignature(C_hex string, r *btcec.PrivateKey, mintPubKeyHex string) (string, error) {
	C_bytes, err := hex.DecodeString(C_hex)
	if err != nil {
		return "", fmt.Errorf("invalid C_ hex: %w", err)
	}
	C_, err := btcec.ParsePubKey(C_bytes)
	if err != nil {
		return "", fmt.Errorf("invalid C_ point: %w", err)
	}

	kBytes, err := hex.DecodeString(mintPubKeyHex)
	if err != nil {
		return "", fmt.Errorf("invalid mint pubkey hex: %w", err)
	}
	K, err := btcec.ParsePubKey(kBytes)
	if err != nil {
		return "", fmt.Errorf("invalid mint pubkey point: %w", err)
	}

	// rK = r * K
	var kJ, rkJ secp256k1.JacobianPoint
	K.AsJacobian(&kJ)
	// Scalar multiply: r * K
	rScalar := new(secp256k1.ModNScalar)
	rScalar.SetByteSlice(r.Serialize())
	secp256k1.ScalarMultNonConst(rScalar, &kJ, &rkJ)

	// Negate rK to get -rK
	rkJ.ToAffine()
	negY := new(secp256k1.FieldVal).NegateVal(&rkJ.Y, 1).Normalize()
	negRK := btcec.NewPublicKey(&rkJ.X, negY)

	// C = C_ + (-rK) = C_ - rK
	var c_J, negRKJ, cJ secp256k1.JacobianPoint
	C_.AsJacobian(&c_J)
	negRK.AsJacobian(&negRKJ)
	secp256k1.AddNonConst(&c_J, &negRKJ, &cJ)
	cJ.ToAffine()
	C := btcec.NewPublicKey(&cJ.X, &cJ.Y)

	return hex.EncodeToString(C.SerializeCompressed()), nil
}

// CalculateY computes Y = hash_to_curve(secret) for use in NUT-07 state checking.
func CalculateY(secret string) (string, error) {
	Y, err := HashToCurve([]byte(secret))
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(Y.SerializeCompressed()), nil
}

// GenerateSecret creates a random 32-byte hex-encoded secret for a proof.
// The RNG error must not be ignored: a predictable secret is spendable by
// anyone who can guess it.
func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate proof secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// SplitAmount decomposes a satoshi amount into powers of 2.
// For example: 13 -> [1, 4, 8]
func SplitAmount(amount int64) []int64 {
	var amounts []int64
	for i := 0; amount > 0; i++ {
		if amount&1 == 1 {
			amounts = append(amounts, int64(1)<<i)
		}
		amount >>= 1
	}
	return amounts
}

// PointToHex serializes a public key point to compressed hex string.
func PointToHex(p *btcec.PublicKey) string {
	return hex.EncodeToString(p.SerializeCompressed())
}

// dummy use to satisfy the big import for potential future use
var _ = new(big.Int)
