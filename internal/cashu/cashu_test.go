package cashu

import "testing"

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
