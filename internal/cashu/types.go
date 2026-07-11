package cashu

// Core Cashu ecash protocol types per NUT-00 through NUT-07.

// Proof represents a single ecash proof (a signed token for a specific amount).
type Proof struct {
	Amount int64  `json:"amount"`
	Id     string `json:"id"`     // keyset ID
	Secret string `json:"secret"` // spending secret
	C      string `json:"C"`      // mint signature (compressed point hex)
}

// BlindedMessage is sent to the mint during minting (Step 1 of BDHKE).
type BlindedMessage struct {
	Amount int64  `json:"amount"`
	Id     string `json:"id"`  // keyset ID
	B_     string `json:"B_"` // blinded secret (compressed point hex)
}

// BlindSignature is returned by the mint (Step 2 of BDHKE).
type BlindSignature struct {
	Amount int64      `json:"amount"`
	Id     string     `json:"id"`
	C_     string     `json:"C_"` // blinded signature (compressed point hex)
	DLEQ   *DLEQProof `json:"dleq,omitempty"`
}

// DLEQProof proves the mint signed with its published key (NUT-12).
type DLEQProof struct {
	E string `json:"e"`
	S string `json:"s"`
	R string `json:"r,omitempty"` // only present on proofs forwarded in tokens
}

// Keyset represents a mint keyset (NUT-01/NUT-02).
type Keyset struct {
	Id     string            `json:"id"`
	Unit   string            `json:"unit"`
	Keys   map[string]string `json:"keys"`   // denomination string -> pubkey hex
	Active bool              `json:"active"`
}

// KeysResponse is the response from GET /v1/keys.
type KeysResponse struct {
	Keysets []Keyset `json:"keysets"`
}

// TokenV3 is the top-level Cashu V3 token structure (cashuA prefix).
type TokenV3 struct {
	Token []TokenEntry `json:"token"`
	Memo  string       `json:"memo,omitempty"`
	Unit  string       `json:"unit,omitempty"`
}

// TokenEntry groups proofs by mint URL.
type TokenEntry struct {
	Mint   string  `json:"mint"`
	Proofs []Proof `json:"proofs"`
}

// Amount returns the total satoshi value of all proofs in the token.
func (t *TokenV3) Amount() int64 {
	var total int64
	for _, entry := range t.Token {
		for _, p := range entry.Proofs {
			total += p.Amount
		}
	}
	return total
}

// MintQuoteRequest is sent to POST /v1/mint/quote/bolt11 (NUT-04 step 1).
type MintQuoteRequest struct {
	Amount int64  `json:"amount"`
	Unit   string `json:"unit"`
}

// MintQuoteResponse is returned by the mint with a Lightning invoice to pay.
type MintQuoteResponse struct {
	Quote   string `json:"quote"`
	Request string `json:"request"` // bolt11 invoice
	State   string `json:"state"`   // "UNPAID", "PAID", "ISSUED" (newer NUT-04)
	Paid    bool   `json:"paid"`    // legacy NUT-04 field, pre-"state" mints
	Expiry  int64  `json:"expiry"`
}

// IsPaid reports whether the quote's invoice was paid, handling both the
// current "state" field and the legacy "paid" bool.
func (q *MintQuoteResponse) IsPaid() bool {
	return q.State == "PAID" || (q.State == "" && q.Paid)
}

// MintRequest is sent to POST /v1/mint/bolt11 after paying the invoice (NUT-04 step 2).
type MintRequest struct {
	Quote   string           `json:"quote"`
	Outputs []BlindedMessage `json:"outputs"`
}

// MintResponse contains the blind signatures from the mint.
type MintResponse struct {
	Signatures []BlindSignature `json:"signatures"`
}

// MeltQuoteRequest is sent to POST /v1/melt/quote/bolt11 (NUT-05 step 1).
type MeltQuoteRequest struct {
	Request string `json:"request"` // bolt11 invoice for the mint to pay
	Unit    string `json:"unit"`
}

// MeltQuoteResponse tells us how much the melt will cost.
type MeltQuoteResponse struct {
	Quote      string `json:"quote"`
	Amount     int64  `json:"amount"`
	FeeReserve int64  `json:"fee_reserve"`
	State      string `json:"state"` // "UNPAID", "PENDING", "PAID"
	Paid       bool   `json:"paid"`  // legacy field, pre-"state" mints
	Expiry     int64  `json:"expiry"`
}

// IsPaid reports whether the melt completed, handling both API shapes.
func (q *MeltQuoteResponse) IsPaid() bool {
	return q.State == "PAID" || (q.State == "" && q.Paid)
}

// MeltRequest is sent to POST /v1/melt/bolt11 with the proofs (NUT-05 step 2).
type MeltRequest struct {
	Quote  string  `json:"quote"`
	Inputs []Proof `json:"inputs"`
}

// MeltResponse indicates whether the melt (Lightning payment) succeeded.
type MeltResponse struct {
	State    string `json:"state"` // "PAID", "UNPAID", "PENDING"
	Preimage string `json:"payment_preimage,omitempty"`
}

// CheckStateRequest is sent to POST /v1/checkstate (NUT-07).
type CheckStateRequest struct {
	Ys []string `json:"Ys"` // Y = hash_to_curve(secret) for each proof
}

// ProofState represents the state of a single proof.
type ProofState struct {
	Y     string `json:"Y"`
	State string `json:"state"` // "UNSPENT", "SPENT", "PENDING"
}

// CheckStateResponse returns the state of requested proofs.
type CheckStateResponse struct {
	States []ProofState `json:"states"`
}

// MintInfo is the response from GET /v1/info (NUT-06).
type MintInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// SwapRequest is sent to POST /v1/swap (NUT-03).
type SwapRequest struct {
	Inputs  []Proof          `json:"inputs"`
	Outputs []BlindedMessage `json:"outputs"`
}

// SwapResponse contains the new blind signatures after a swap.
type SwapResponse struct {
	Signatures []BlindSignature `json:"signatures"`
}
