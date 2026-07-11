package cashu

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

const (
	// TokenPrefixV3 is the prefix for Cashu V3 tokens.
	TokenPrefixV3 = "cashuA"
	// TokenPrefixV4 is the prefix for Cashu V4 (CBOR) tokens.
	TokenPrefixV4 = "cashuB"
)

// tokenV4 is the NUT-00 V4 CBOR wire format (single-letter keys).
// We only deserialize V4; emitting stays V3 for maximum wallet compatibility.
type tokenV4 struct {
	Mint  string          `cbor:"m"`
	Unit  string          `cbor:"u"`
	Memo  string          `cbor:"d,omitempty"`
	Token []tokenV4Entry  `cbor:"t"`
}

type tokenV4Entry struct {
	Id     []byte          `cbor:"i"` // keyset id, raw bytes
	Proofs []tokenV4Proof  `cbor:"p"`
}

type tokenV4Proof struct {
	Amount int64  `cbor:"a"`
	Secret string `cbor:"s"`
	C      []byte `cbor:"c"` // signature point, raw bytes
}

// Serialize encodes a TokenV3 to the cashuA... string format.
// Format: "cashuA" + base64url(json(TokenV3))
func (t *TokenV3) Serialize() (string, error) {
	jsonBytes, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("failed to marshal token: %w", err)
	}
	encoded := base64.URLEncoding.EncodeToString(jsonBytes)
	return TokenPrefixV3 + encoded, nil
}

// Deserialize decodes a cashuA (V3, JSON) or cashuB (V4, CBOR) token string
// into the internal TokenV3 representation.
func Deserialize(tokenStr string) (*TokenV3, error) {
	tokenStr = strings.TrimSpace(tokenStr)

	if strings.HasPrefix(tokenStr, TokenPrefixV4) {
		return deserializeV4(strings.TrimPrefix(tokenStr, TokenPrefixV4))
	}
	if !strings.HasPrefix(tokenStr, TokenPrefixV3) {
		return nil, fmt.Errorf("invalid token: must start with %s or %s", TokenPrefixV3, TokenPrefixV4)
	}

	encoded := strings.TrimPrefix(tokenStr, TokenPrefixV3)

	// Try standard base64url first, then try with padding
	jsonBytes, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		// Try without padding
		jsonBytes, err = base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("failed to decode token base64: %w", err)
		}
	}

	var token TokenV3
	err = json.Unmarshal(jsonBytes, &token)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal token JSON: %w", err)
	}

	if len(token.Token) == 0 {
		return nil, fmt.Errorf("token contains no entries")
	}

	return &token, nil
}

// deserializeV4 decodes the base64url CBOR payload of a cashuB token and maps
// it onto the internal TokenV3 structure.
func deserializeV4(encoded string) (*TokenV3, error) {
	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("failed to decode V4 token base64: %w", err)
		}
	}

	var v4 tokenV4
	if err := cbor.Unmarshal(raw, &v4); err != nil {
		return nil, fmt.Errorf("failed to decode V4 token CBOR: %w", err)
	}
	if len(v4.Token) == 0 {
		return nil, fmt.Errorf("token contains no entries")
	}

	// V4 groups proofs per keyset under one mint; flatten to V3 shape.
	var proofs []Proof
	for _, entry := range v4.Token {
		id := hex.EncodeToString(entry.Id)
		for _, p := range entry.Proofs {
			proofs = append(proofs, Proof{
				Amount: p.Amount,
				Id:     id,
				Secret: p.Secret,
				C:      hex.EncodeToString(p.C),
			})
		}
	}
	if len(proofs) == 0 {
		return nil, fmt.Errorf("token contains no proofs")
	}

	return &TokenV3{
		Token: []TokenEntry{{Mint: v4.Mint, Proofs: proofs}},
		Memo:  v4.Memo,
		Unit:  v4.Unit,
	}, nil
}
