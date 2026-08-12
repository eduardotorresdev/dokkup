package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// tokenBytes is how much randomness a Setup Token carries. 32 bytes is 256
// bits, which is past the point where guessing is the attack anyone would
// choose, and it is the same size the session tokens will be: a token that
// grants ownership of the host must not be the shortest secret dokkup has.
const tokenBytes = 32

// NewToken returns a fresh secret and the hash it is stored as.
//
// Both at once, in one call, because the token and its hash must be the only
// two things that exist: a caller who hashed separately would be free to keep
// the token around, and this is the one credential dokkup shows a human and
// then must forget. The store never sees the token; the operator never sees the
// hash.
//
// The token is base64 with the URL alphabet so that an operator can paste it
// out of a terminal into a browser -- into a query string one day, if the
// installer ever prints a link -- without an encoding step in between turning
// a "+" into a space.
func NewToken() (token, hash string, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("drawing a setup token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken returns the hash a token is stored and matched as: hex sha256.
//
// A plain hash and not argon2id, deliberately, and the difference is the input.
// A password is chosen by a person and has to be made expensive to guess; this
// token is 256 bits of [crypto/rand] output, so an attacker who could try
// enough hashes to find it could also find the argon2 one. What matters here is
// that a stolen database is not a stolen token, and that the lookup is a single
// indexed comparison rather than a scan over rows at 64 MiB apiece.
//
// Hex rather than base64 because the result is a primary key that people read
// in a database shell, and hex has one spelling.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
