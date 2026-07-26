package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// tokenPayload is the (unencrypted, signed) claim set carried by an API token.
// It is not secret — the signature is what authenticates it. iat is informational.
type tokenPayload struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
}

var b64 = base64.RawURLEncoding

// sign returns the base64url HMAC-SHA256 of msg under masterKey.
func sign(masterKey, msg string) string {
	mac := hmac.New(sha256.New, []byte(masterKey))
	mac.Write([]byte(msg))
	return b64.EncodeToString(mac.Sum(nil))
}

// MintToken issues a compact signed token of the form
// "<b64url(payloadJSON)>.<b64url(HMAC-SHA256(masterKey, b64url(payloadJSON)))>".
// The payload is not secret; the signature authenticates it. iat is the issue
// time (Unix seconds) — pass 0 to omit a meaningful timestamp.
func MintToken(masterKey string, iat int64) (string, error) {
	raw, err := json.Marshal(tokenPayload{Sub: "varhub", Iat: iat})
	if err != nil {
		return "", err
	}
	body := b64.EncodeToString(raw)
	return body + "." + sign(masterKey, body), nil
}

// VerifyToken reports whether tok is a well-formed token correctly signed by
// masterKey. The comparison is constant-time. There is no expiry check (tokens
// do not expire in this version).
func VerifyToken(masterKey, tok string) bool {
	body, sig, ok := strings.Cut(tok, ".")
	if !ok || body == "" || sig == "" {
		return false
	}
	if _, err := b64.DecodeString(body); err != nil {
		return false // body must be valid base64url
	}
	return hmac.Equal([]byte(sig), []byte(sign(masterKey, body)))
}

// requireToken wraps h so it is reached only with a valid "Authorization: Bearer
// <token>" header signed by masterKey; otherwise it responds 401.
func requireToken(masterKey string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok || !VerifyToken(masterKey, strings.TrimSpace(tok)) {
			writeJSONError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		h.ServeHTTP(w, r)
	})
}
