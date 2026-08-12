package secret_test

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"

	"github.com/eduardotorresdev/dokkup/internal/secret"
)

func TestAPasswordVerifiesAgainstItsOwnHashAndNothingElse(t *testing.T) {
	t.Parallel()

	const password = "correct horse battery staple"

	encoded, err := secret.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing a password: %v", err)
	}

	// The hash is what goes into the database, so the password must not be
	// anywhere in it.
	if strings.Contains(encoded, password) {
		t.Fatal("the encoded hash carries the password itself")
	}

	matched, err := secret.VerifyPassword(encoded, password)
	if err != nil {
		t.Fatalf("verifying the password that made the hash: %v", err)
	}
	if !matched {
		t.Error("the password that made the hash did not verify against it")
	}

	for _, wrong := range []string{
		"correct horse battery stapl",
		"correct horse battery staple ",
		"Correct horse battery staple",
		"",
	} {
		matched, err := secret.VerifyPassword(encoded, wrong)
		if err != nil {
			t.Errorf("verifying %q: %v", wrong, err)
		}
		if matched {
			t.Errorf("%q verified against a hash it did not make", wrong)
		}
	}
}

func TestTheSamePasswordHashedTwiceGivesTwoDifferentHashes(t *testing.T) {
	t.Parallel()

	const password = "a password two operators both chose"

	first, err := secret.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing a password: %v", err)
	}
	second, err := secret.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing the same password again: %v", err)
	}

	// The salt is the whole point: two operators who chose the same password
	// must have nothing in common in the database, so that a stolen copy cannot
	// be attacked once for every row at a time.
	if first == second {
		t.Error("the same password hashed twice gave the same string, so there is no per-hash salt")
	}

	// Both still verify, which is what says the difference is the salt and not
	// a hash that came out wrong.
	for _, encoded := range []string{first, second} {
		matched, err := secret.VerifyPassword(encoded, password)
		if err != nil {
			t.Fatalf("verifying against %s: %v", encoded, err)
		}
		if !matched {
			t.Error("a freshly made hash did not verify against the password that made it")
		}
	}
}

func TestAHashDokkupDidNotWriteIsAnErrorAndNeverAMatch(t *testing.T) {
	t.Parallel()

	good, err := secret.HashPassword("whatever")
	if err != nil {
		t.Fatalf("hashing a password: %v", err)
	}
	fields := strings.Split(good, "$")

	// A malformed hash is what a bad restore, a truncating column or a
	// hand-edited row leaves behind. Every one of these must fail loudly:
	// answering "no match" would lock the Owner out of their own host with no
	// explanation, and answering "match" would be worse.
	malformed := map[string]string{
		"empty":                "",
		"not PHC at all":       "hash-of-whatever",
		"no leading separator": strings.TrimPrefix(good, "$"),
		"another algorithm":    "$argon2i$" + strings.Join(fields[2:], "$"),
		"another version":      "$" + fields[1] + "$v=16$" + strings.Join(fields[3:], "$"),
		"unreadable cost":      "$" + fields[1] + "$" + fields[2] + "$m=lots,t=3,p=4$" + strings.Join(fields[4:], "$"),
		"zero cost":            "$" + fields[1] + "$" + fields[2] + "$m=0,t=0,p=0$" + strings.Join(fields[4:], "$"),
		"unreadable salt":      "$" + strings.Join(fields[1:4], "$") + "$not base64!!$" + fields[5],
		"unreadable hash":      "$" + strings.Join(fields[1:5], "$") + "$not base64!!",
		"missing hash":         "$" + strings.Join(fields[1:5], "$") + "$",
		"a field too many":     good + "$extra",
		// Sscanf stops where it stops and says nothing about the rest, so a
		// field carrying anything after its number is a field somebody else
		// wrote.
		"junk after the version": "$" + fields[1] + "$v=19junk$" + strings.Join(fields[3:], "$"),
		"junk after the cost":    "$" + strings.Join(fields[1:3], "$") + "$m=65536,t=3,p=4junk$" + strings.Join(fields[4:], "$"),
		// A cost read out of a corrupted column is an allocation dokkup would
		// make on an unauthenticated request. Measured at m=1048576: a
		// gigabyte and 934 ms per attempt.
		"a cost past what dokkup writes": "$" + strings.Join(fields[1:3], "$") + "$m=1048576,t=3,p=4$" +
			strings.Join(fields[4:], "$"),
		"more passes than dokkup writes": "$" + strings.Join(fields[1:3], "$") + "$m=65536,t=99,p=4$" +
			strings.Join(fields[4:], "$"),
		// A length nothing writes, which a corrupted row could still claim:
		// the derivation reproduces the stored length, so it has to be bounded.
		"far too long a hash": "$" + strings.Join(fields[1:5], "$") + "$" +
			base64.RawStdEncoding.EncodeToString(make([]byte, 65)),
	}
	for what, encoded := range malformed {
		matched, err := secret.VerifyPassword(encoded, "whatever")
		if err == nil {
			t.Errorf("verifying against a hash that is %s did not err", what)
		}
		if matched {
			t.Errorf("a hash that is %s verified a password", what)
		}
	}
}

func TestEveryTokenIsNewAndIsStoredAsItsOwnHash(t *testing.T) {
	t.Parallel()

	const draws = 64

	seen := make(map[string]bool, draws)
	for range draws {
		token, hash, err := secret.NewToken()
		if err != nil {
			t.Fatalf("drawing a token: %v", err)
		}
		if token == "" || hash == "" {
			t.Fatalf("drawing a token gave %q and %q", token, hash)
		}

		// The hash is what the store keeps, so the token must not be readable
		// out of it, and the store must be able to recognise the token coming
		// back by hashing it the same way.
		if strings.Contains(hash, token) {
			t.Fatal("the hash carries the token itself")
		}
		if again := secret.HashToken(token); again != hash {
			t.Fatalf("hashing a token again gave %s, want the %s it was issued with", again, hash)
		}

		// Two operators setting two hosts up must not be handed the same
		// credential, and a token that repeats is a token somebody has already
		// seen printed.
		if seen[token] {
			t.Fatalf("a token was drawn twice in %d draws", draws)
		}
		seen[token] = true
	}
}

func TestTwoDifferentTokensHashDifferently(t *testing.T) {
	t.Parallel()

	// The hash is the primary key the token is looked up by, so two tokens
	// that hashed alike would let one be spent as the other.
	if secret.HashToken("one token") == secret.HashToken("another token") {
		t.Error("two different tokens hash to the same string")
	}
	if secret.HashToken("") == secret.HashToken("a token") {
		t.Error("the empty token hashes like a real one")
	}
}

func TestHashesAreWrittenAtTheCostTheContractFroze(t *testing.T) {
	t.Parallel()

	// The cost is not a detail this package is free to lower quietly: it is
	// what makes a stolen copy of the database expensive to attack, and a
	// change to it is a decision somebody has to make on purpose. The prefix
	// is asserted rather than the constants, because the prefix is what other
	// dokkups will read a hash back through.
	const want = "$argon2id$v=19$m=65536,t=3,p=4$"

	encoded, err := secret.HashPassword("a password")
	if err != nil {
		t.Fatalf("hashing a password: %v", err)
	}
	if !strings.HasPrefix(encoded, want) {
		t.Errorf("a fresh hash begins %q, want %q", encoded[:min(len(encoded), len(want))], want)
	}

	// 16 bytes of salt and 32 of hash, both as base64 without padding.
	fields := strings.Split(encoded, "$")
	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil {
		t.Fatalf("decoding the salt: %v", err)
	}
	key, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil {
		t.Fatalf("decoding the hash: %v", err)
	}
	if len(salt) != 16 {
		t.Errorf("the salt is %d bytes, want 16", len(salt))
	}
	if len(key) != 32 {
		t.Errorf("the hash is %d bytes, want 32", len(key))
	}
}

func TestATokenCarriesTheRandomnessTheContractFroze(t *testing.T) {
	t.Parallel()

	token, _, err := secret.NewToken()
	if err != nil {
		t.Fatalf("drawing a token: %v", err)
	}

	// 32 bytes of crypto/rand in base64 without padding, which is 43
	// characters. A token that got shorter would still pass every other case
	// in this file and would be a credential somebody can guess.
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("decoding a token: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("a token carries %d bytes, want 32", len(raw))
	}
}

func TestAPasswordHashedAtAnotherCostStillVerifies(t *testing.T) {
	t.Parallel()

	// This is what reading the cost out of the string buys, and the only case
	// that can prove it: a hash written before the numbers above were last
	// raised -- or by a dokkup that will one day raise them -- is still that
	// operator's password, and locking them out of their own host because the
	// default moved is not an option.
	const (
		password = "a password from an older installation"

		memory  = 8 * 1024
		time    = 1
		threads = 1
		keyLen  = 16
	)

	key := argon2.IDKey([]byte(password), []byte("sixteen bytes!!!"), time, memory, threads, keyLen)
	encoded := fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, time, threads,
		base64.RawStdEncoding.EncodeToString([]byte("sixteen bytes!!!")),
		base64.RawStdEncoding.EncodeToString(key))

	matched, err := secret.VerifyPassword(encoded, password)
	if err != nil {
		t.Fatalf("verifying a hash made at another cost: %v", err)
	}
	if !matched {
		t.Error("a hash made at another cost did not verify against the password that made it")
	}

	matched, err = secret.VerifyPassword(encoded, "another password")
	if err != nil {
		t.Fatalf("verifying the wrong password against a hash made at another cost: %v", err)
	}
	if matched {
		t.Error("the wrong password verified against a hash made at another cost")
	}
}
