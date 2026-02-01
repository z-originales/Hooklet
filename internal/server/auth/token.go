package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// GenerateSecureToken returns a cryptographically secure random token.
// Returns 32 random bytes encoded as 64 hex characters.
func GenerateSecureToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}
