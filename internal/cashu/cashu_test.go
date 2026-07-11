package cashu

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/fxamacker/cbor/v2"
)

// SplitAmount must always sum back to the input and only emit powers of two,
// otherwise a mint would sign the wrong total and value is created or lost.
func TestSplitAmountSumsToInput(t *testing.T) {
	for _, amt := range []int64{1, 2, 3, 13, 21, 100, 2100, 1_000_000} {
		var sum int64
		for _, p := range SplitAmount(amt) {
			if p&(p-1) != 0 {
				t.Fatalf("SplitAmount(%d) produced non-power-of-two %d", amt, p)
			}
			sum += p
		}
		if sum != amt {
			t.Fatalf("SplitAmount(%d) sums to %d", amt, sum)
		}
	}
}

// Simulate the mint side of NUT-12 and check our verifier accepts a valid
// proof and rejects a tampered one.
func TestVerifyDLEQ(t *testing.T) {
	newKey := func() *btcec.PrivateKey {
		b := make([]byte, 32)
		rand.Read(b)
		k, _ := btcec.PrivKeyFromBytes(b)
		return k
	}
	k := newKey()      // mint private key
	A := k.PubKey()    // published mint key
	br, err := BlindMessage("test-secret")
	if err != nil {
		t.Fatal(err)
	}

	// Mint: C_ = k*B_
	mulPub := func(s *secp256k1.ModNScalar, P *btcec.PublicKey) *btcec.PublicKey {
		var pJ, outJ secp256k1.JacobianPoint
		P.AsJacobian(&pJ)
		secp256k1.ScalarMultNonConst(s, &pJ, &outJ)
		outJ.ToAffine()
		return btcec.NewPublicKey(&outJ.X, &outJ.Y)
	}
	C_ := mulPub(&k.Key, br.B_)

	// Mint DLEQ: nonce w; R1 = w*G, R2 = w*B_; e = hash_e(R1,R2,A,C_); s = w + e*k
	w := newKey()
	var gJ secp256k1.JacobianPoint
	secp256k1.ScalarBaseMultNonConst(&w.Key, &gJ)
	gJ.ToAffine()
	R1 := btcec.NewPublicKey(&gJ.X, &gJ.Y)
	R2 := mulPub(&w.Key, br.B_)
	eBytes := hashE(R1, R2, A, C_)
	var e secp256k1.ModNScalar
	e.SetByteSlice(eBytes)
	s := new(secp256k1.ModNScalar).Mul2(&e, &k.Key).Add(&w.Key)
	sBytes := s.Bytes()

	ok, err := VerifyDLEQ(hex.EncodeToString(eBytes), hex.EncodeToString(sBytes[:]), A, br.B_, C_)
	if err != nil || !ok {
		t.Fatalf("valid DLEQ rejected: ok=%v err=%v", ok, err)
	}

	// Tamper: wrong mint key must fail.
	ok, _ = VerifyDLEQ(hex.EncodeToString(eBytes), hex.EncodeToString(sBytes[:]), newKey().PubKey(), br.B_, C_)
	if ok {
		t.Fatal("DLEQ accepted with wrong mint key")
	}
}

// A V4 (cashuB, CBOR) token must decode into the internal representation.
func TestDeserializeV4(t *testing.T) {
	v4 := tokenV4{
		Mint: "https://mint.example",
		Unit: "sat",
		Memo: "hi",
		Token: []tokenV4Entry{{
			Id: []byte{0x00, 0xab},
			Proofs: []tokenV4Proof{
				{Amount: 8, Secret: "s1", C: []byte{0x02, 0x01}},
				{Amount: 2, Secret: "s2", C: []byte{0x02, 0x02}},
			},
		}},
	}
	raw, err := cbor.Marshal(v4)
	if err != nil {
		t.Fatal(err)
	}
	tokenStr := TokenPrefixV4 + base64.RawURLEncoding.EncodeToString(raw)

	got, err := Deserialize(tokenStr)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount() != 10 || got.Token[0].Mint != "https://mint.example" {
		t.Fatalf("V4 decode mismatch: %+v", got)
	}
	if got.Token[0].Proofs[0].Id != "00ab" {
		t.Fatalf("keyset id not hex-mapped: %s", got.Token[0].Proofs[0].Id)
	}
}

// A token must survive serialize -> deserialize unchanged, including its total.
func TestTokenRoundTrip(t *testing.T) {
	orig := &TokenV3{
		Token: []TokenEntry{{
			Mint: "https://mint.example",
			Proofs: []Proof{
				{Amount: 8, Id: "00abc", Secret: "deadbeef", C: "02aa"},
				{Amount: 2, Id: "00abc", Secret: "cafe", C: "02bb"},
			},
		}},
		Memo: "hi",
		Unit: "sat",
	}
	s, err := orig.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Deserialize(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount() != 10 {
		t.Fatalf("amount = %d, want 10", got.Amount())
	}
	if got.Token[0].Mint != orig.Token[0].Mint || len(got.Token[0].Proofs) != 2 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
