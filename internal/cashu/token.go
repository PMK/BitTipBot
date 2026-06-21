package cashu

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// TokenPrefixV3 is the prefix for Cashu V3 tokens.
	TokenPrefixV3 = "cashuA"
)

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

// Deserialize decodes a cashuA... string to a TokenV3.
func Deserialize(tokenStr string) (*TokenV3, error) {
	tokenStr = strings.TrimSpace(tokenStr)

	if !strings.HasPrefix(tokenStr, TokenPrefixV3) {
		return nil, fmt.Errorf("invalid token: must start with %s", TokenPrefixV3)
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
