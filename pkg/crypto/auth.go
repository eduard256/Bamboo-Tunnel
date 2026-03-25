package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// Cookie name looks like a standard analytics/session cookie
const CookieName = "_sid"

// ValidateRequest checks if the request carries a valid tunnel token.
// Token is passed as a standard-looking session cookie.
func ValidateRequest(r *http.Request, token string) bool {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	return SecureCompare(cookie.Value, token)
}

// SecureCompare prevents timing attacks on token comparison
func SecureCompare(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

// TokenFromSecret derives a hex token from a secret string.
// Useful for generating cookie value from env secret.
func TokenFromSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
