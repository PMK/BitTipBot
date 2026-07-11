package cashu

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/imroc/req"
	log "github.com/sirupsen/logrus"
)

// Client is an HTTP client for communicating with a Cashu mint.
type Client struct {
	mintURL string
	header  req.Header
	http    *req.Req
}

// NewClient creates a new Cashu mint client.
func NewClient(mintURL string) *Client {
	// Own req instance with a timeout so a hung mint can't wedge a user's
	// lock and goroutine forever (handlers hold a per-user lock while calling).
	r := req.New()
	r.SetTimeout(30 * time.Second)
	return &Client{
		mintURL: mintURL,
		http:    r,
		header: req.Header{
			"Content-Type": "application/json",
			"Accept":       "application/json",
		},
	}
}

// MintURL returns the configured mint URL.
func (c *Client) MintURL() string {
	return c.mintURL
}

// GetInfo fetches mint information (NUT-06).
func (c *Client) GetInfo() (*MintInfo, error) {
	resp, err := c.http.Get(c.mintURL+"/v1/info", c.header)
	if err != nil {
		return nil, fmt.Errorf("failed to get mint info: %w", err)
	}
	if resp.Response().StatusCode >= 300 {
		return nil, fmt.Errorf("mint info request failed with status %d", resp.Response().StatusCode)
	}
	var info MintInfo
	err = resp.ToJSON(&info)
	return &info, err
}

// GetKeysets fetches active keysets from the mint (NUT-01).
func (c *Client) GetKeysets() (*KeysResponse, error) {
	resp, err := c.http.Get(c.mintURL+"/v1/keys", c.header)
	if err != nil {
		return nil, fmt.Errorf("failed to get keysets: %w", err)
	}
	if resp.Response().StatusCode >= 300 {
		return nil, fmt.Errorf("keysets request failed with status %d", resp.Response().StatusCode)
	}
	var keys KeysResponse
	err = resp.ToJSON(&keys)
	return &keys, err
}

// MintQuote requests a mint quote (NUT-04, step 1).
// The mint returns a Lightning invoice that must be paid before tokens can be minted.
func (c *Client) MintQuote(amount int64, unit string) (*MintQuoteResponse, error) {
	body := MintQuoteRequest{
		Amount: amount,
		Unit:   unit,
	}
	resp, err := c.http.Post(c.mintURL+"/v1/mint/quote/bolt11", c.header, req.BodyJSON(&body))
	if err != nil {
		return nil, fmt.Errorf("failed to request mint quote: %w", err)
	}
	if resp.Response().StatusCode >= 300 {
		return nil, fmt.Errorf("mint quote failed with status %d: %s", resp.Response().StatusCode, resp.String())
	}
	var quote MintQuoteResponse
	err = resp.ToJSON(&quote)
	return &quote, err
}

// CheckMintQuote checks the status of a mint quote (NUT-04).
func (c *Client) CheckMintQuote(quoteId string) (*MintQuoteResponse, error) {
	resp, err := c.http.Get(c.mintURL+"/v1/mint/quote/bolt11/"+quoteId, c.header)
	if err != nil {
		return nil, fmt.Errorf("failed to check mint quote: %w", err)
	}
	if resp.Response().StatusCode >= 300 {
		return nil, fmt.Errorf("check mint quote failed with status %d", resp.Response().StatusCode)
	}
	var quote MintQuoteResponse
	err = resp.ToJSON(&quote)
	return &quote, err
}

// Mint exchanges blinded messages for blind signatures after the quote invoice is paid (NUT-04, step 2).
func (c *Client) Mint(quoteId string, outputs []BlindedMessage) (*MintResponse, error) {
	body := MintRequest{
		Quote:   quoteId,
		Outputs: outputs,
	}
	resp, err := c.http.Post(c.mintURL+"/v1/mint/bolt11", c.header, req.BodyJSON(&body))
	if err != nil {
		return nil, fmt.Errorf("failed to mint tokens: %w", err)
	}
	if resp.Response().StatusCode >= 300 {
		return nil, fmt.Errorf("mint failed with status %d: %s", resp.Response().StatusCode, resp.String())
	}
	var mintResp MintResponse
	err = resp.ToJSON(&mintResp)
	return &mintResp, err
}

// MeltQuote requests a melt quote to pay a Lightning invoice with ecash (NUT-05, step 1).
func (c *Client) MeltQuote(bolt11 string, unit string) (*MeltQuoteResponse, error) {
	body := MeltQuoteRequest{
		Request: bolt11,
		Unit:    unit,
	}
	resp, err := c.http.Post(c.mintURL+"/v1/melt/quote/bolt11", c.header, req.BodyJSON(&body))
	if err != nil {
		return nil, fmt.Errorf("failed to request melt quote: %w", err)
	}
	if resp.Response().StatusCode >= 300 {
		return nil, fmt.Errorf("melt quote failed with status %d: %s", resp.Response().StatusCode, resp.String())
	}
	var quote MeltQuoteResponse
	err = resp.ToJSON(&quote)
	return &quote, err
}

// Melt submits proofs to the mint which pays the Lightning invoice (NUT-05, step 2).
func (c *Client) Melt(quoteId string, inputs []Proof) (*MeltResponse, error) {
	body := MeltRequest{
		Quote:  quoteId,
		Inputs: inputs,
	}
	resp, err := c.http.Post(c.mintURL+"/v1/melt/bolt11", c.header, req.BodyJSON(&body))
	if err != nil {
		return nil, fmt.Errorf("failed to melt tokens: %w", err)
	}
	if resp.Response().StatusCode >= 300 {
		return nil, fmt.Errorf("melt failed with status %d: %s", resp.Response().StatusCode, resp.String())
	}
	var meltResp MeltResponse
	err = resp.ToJSON(&meltResp)
	return &meltResp, err
}

// CheckState checks whether proofs have been spent (NUT-07).
func (c *Client) CheckState(proofs []Proof) (*CheckStateResponse, error) {
	ys := make([]string, len(proofs))
	for i, p := range proofs {
		y, err := CalculateY(p.Secret)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate Y for proof: %w", err)
		}
		ys[i] = y
	}
	body := CheckStateRequest{Ys: ys}
	resp, err := c.http.Post(c.mintURL+"/v1/checkstate", c.header, req.BodyJSON(&body))
	if err != nil {
		return nil, fmt.Errorf("failed to check state: %w", err)
	}
	if resp.Response().StatusCode >= 300 {
		return nil, fmt.Errorf("check state failed with status %d", resp.Response().StatusCode)
	}
	var stateResp CheckStateResponse
	err = resp.ToJSON(&stateResp)
	return &stateResp, err
}

// WaitQuotePaid polls the mint until it sees the quote's invoice as paid.
// LNbits returning from Pay only means our side sent the payment; the mint's
// view can lag by seconds (observed with 21mint.me), and minting before it
// settles fails with "quote not paid".
func (c *Client) WaitQuotePaid(quoteId string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		q, err := c.CheckMintQuote(quoteId)
		if err == nil && q.IsPaid() {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, err
		}
		time.Sleep(2 * time.Second)
	}
}

// MintTokens is a high-level function that orchestrates the full minting flow:
// 1. Get active keyset
// 2. Split amount into power-of-2 denominations
// 3. Generate secrets and blinding factors
// 4. Create blinded messages
// 5. Send to mint and receive blind signatures
// 6. Unblind signatures to get valid proofs
// 7. Serialize to cashuA token string
//
// The caller must have already paid the mint quote's Lightning invoice.
func (c *Client) MintTokens(quoteId string, amount int64, memo string) (string, error) {
	// Get active keyset
	keysResp, err := c.GetKeysets()
	if err != nil {
		return "", fmt.Errorf("failed to get keysets: %w", err)
	}

	// Find the active sat keyset
	var activeKeyset *Keyset
	for i := range keysResp.Keysets {
		ks := &keysResp.Keysets[i]
		// /v1/keys (NUT-01) returns only active keysets and has no "active" field,
		// so match on unit alone. ponytail: first sat keyset wins, fine for one mint.
		if ks.Unit == "sat" {
			activeKeyset = ks
			break
		}
	}
	if activeKeyset == nil {
		return "", fmt.Errorf("no active sat keyset found at mint")
	}

	// Split amount into powers of 2
	amounts := SplitAmount(amount)

	// Generate blinded messages
	outputs := make([]BlindedMessage, len(amounts))
	blindingResults := make([]*BlindingResult, len(amounts))
	for i, amt := range amounts {
		secret, err := GenerateSecret()
		if err != nil {
			return "", err
		}
		br, err := BlindMessage(secret)
		if err != nil {
			return "", fmt.Errorf("failed to blind message: %w", err)
		}
		blindingResults[i] = br
		outputs[i] = BlindedMessage{
			Amount: amt,
			Id:     activeKeyset.Id,
			B_:     hex.EncodeToString(br.B_.SerializeCompressed()),
		}
	}

	// Send to mint
	mintResp, err := c.Mint(quoteId, outputs)
	if err != nil {
		return "", fmt.Errorf("mint request failed: %w", err)
	}

	if len(mintResp.Signatures) != len(outputs) {
		return "", fmt.Errorf("expected %d signatures, got %d", len(outputs), len(mintResp.Signatures))
	}

	// Unblind signatures to get valid proofs
	proofs := make([]Proof, len(mintResp.Signatures))
	dleqMissing := 0
	for i, sig := range mintResp.Signatures {
		// Look up the mint's public key for this denomination
		amtStr := strconv.FormatInt(sig.Amount, 10)
		mintPubKey, ok := activeKeyset.Keys[amtStr]
		if !ok {
			return "", fmt.Errorf("no mint key found for amount %d", sig.Amount)
		}

		// NUT-12: verify the mint signed with its published key. Invalid proof =
		// hard fail (the mint could later disown these tokens). Missing proof is
		// tolerated with a warning since not every mint ships NUT-12.
		if sig.DLEQ != nil {
			A, err := parsePubKeyHex(mintPubKey)
			if err != nil {
				return "", fmt.Errorf("invalid mint key for amount %d: %w", sig.Amount, err)
			}
			C_, err := parsePubKeyHex(sig.C_)
			if err != nil {
				return "", fmt.Errorf("invalid C_ from mint: %w", err)
			}
			valid, err := VerifyDLEQ(sig.DLEQ.E, sig.DLEQ.S, A, blindingResults[i].B_, C_)
			if err != nil || !valid {
				return "", fmt.Errorf("mint returned invalid DLEQ proof for amount %d (err=%v)", sig.Amount, err)
			}
		} else {
			dleqMissing++
		}

		// Unblind: C = C_ - r*K
		C, err := UnblindSignature(sig.C_, blindingResults[i].R, mintPubKey)
		if err != nil {
			return "", fmt.Errorf("failed to unblind signature: %w", err)
		}

		proofs[i] = Proof{
			Amount: sig.Amount,
			Id:     sig.Id,
			Secret: blindingResults[i].Secret,
			C:      C,
		}
	}

	// Serialize to cashuA token
	token := &TokenV3{
		Token: []TokenEntry{
			{
				Mint:   c.mintURL,
				Proofs: proofs,
			},
		},
		Memo: memo,
		Unit: "sat",
	}

	tokenStr, err := token.Serialize()
	if err != nil {
		return "", fmt.Errorf("failed to serialize token: %w", err)
	}

	if dleqMissing > 0 {
		log.Warnf("[cashu] mint did not include DLEQ proofs for %d/%d signatures (NUT-12 unsupported?)", dleqMissing, len(mintResp.Signatures))
	}
	log.Infof("[cashu] Minted %d sat token with %d proofs", amount, len(proofs))
	return tokenStr, nil
}

// parsePubKeyHex parses a compressed secp256k1 point from hex.
func parsePubKeyHex(h string) (*btcec.PublicKey, error) {
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, err
	}
	return btcec.ParsePubKey(b)
}

// AllProofsUnspent checks if all proofs in a token are unspent.
func (c *Client) AllProofsUnspent(proofs []Proof) (bool, error) {
	stateResp, err := c.CheckState(proofs)
	if err != nil {
		return false, err
	}
	for _, s := range stateResp.States {
		if s.State != "UNSPENT" {
			return false, nil
		}
	}
	return true, nil
}
