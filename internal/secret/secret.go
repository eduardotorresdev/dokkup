// Package secret produces and checks the secrets dokkup keeps: an operator's
// password, and the Setup Token.
//
// It is a package of its own, and not a corner of internal/store, because
// choosing how a password is hashed is not a storage decision: the store keeps
// an opaque string and would have to migrate its schema to change algorithm,
// which is precisely what the PHC string written here makes unnecessary. It is
// also what lets the CLI mint a Setup Token without opening the same file the
// server reads.
//
// Nothing here ever puts a password, a token, or a token's hash into an error.
// An error string reaches a log file, and a log file is not where a credential
// stops being one.
package secret

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	// argon2id is the memory-hard winner of the Password Hashing Competition
	// and what OWASP names first for new applications. bcrypt is the obvious
	// alternative and is rejected here for two reasons that matter to dokkup:
	// it silently truncates a password at 72 bytes, and its work factor buys
	// only CPU time, where argon2's memory cost is what makes an attacker's
	// GPU farm stop being cheap. golang.org/x/crypto is already a direct
	// dependency, so this costs no new supply chain.
	"golang.org/x/crypto/argon2"
)

// The argon2id cost dokkup hashes new passwords at.
//
// 64 MiB with one pass over it three times, on four lanes, is the second of the
// two configurations RFC 9106 recommends, and it is chosen over the first
// (2 GiB) because dokkup runs beside Dokku on a host whose memory belongs to
// the applications being deployed: a sign-in form that can be made to allocate
// two gigabytes per attempt is a denial of service with a submit button.
//
// They are constants and also written into every hash, because [VerifyPassword]
// reads the cost back out of the string it is checking. That is what makes
// raising these numbers a change to this file alone: passwords hashed by an
// older dokkup keep verifying at the cost they were made with.
const (
	hashTime    = 3
	hashMemory  = 64 * 1024 // KiB
	hashThreads = 4
	hashKeyLen  = 32
	hashSaltLen = 16
)

// maxKeyLen bounds the hash a PHC string may claim to carry.
//
// It is a sanity limit and not a cost: [VerifyPassword] derives a key of the
// stored length so that a hash written at a different length still verifies,
// and a corrupted row saying it holds four gigabytes would otherwise be an
// allocation request. 64 bytes is twice what dokkup writes and past what any
// argon2 deployment uses.
const maxKeyLen = 64

// The cost a PHC string may claim, for the reason maxKeyLen exists and with
// more at stake: [VerifyPassword] hashes at the cost it reads, so the number in
// a corrupted column is a memory allocation and a stretch of CPU that dokkup
// asked for. Measured, "m=1048576" made one verification allocate a gigabyte
// and take 934 ms, and from the sign-in screen on that is an unauthenticated
// request doing it.
//
// Four times what dokkup writes, in both directions, so that the cost above can
// be raised several times before this has to move, and so that a hash made by
// another tool at a heavier setting still verifies. Beyond that it is not a
// hash this installation can have written.
const (
	maxMemory = 4 * hashMemory
	maxTime   = 4 * hashTime
)

// phcAlgorithm and phcVersion are the two fields of the encoding that this
// package refuses to be flexible about. A hash naming another algorithm, or
// another version of argon2, is not a hash dokkup wrote, and guessing at what
// it meant is how a verifier ends up computing the wrong function and
// answering false to a correct password.
const (
	phcAlgorithm = "argon2id"
	phcVersion   = argon2.Version // 19, the only version x/crypto implements
)

// b64 is base64 without padding, which the PHC string format requires: the "="
// characters would be indistinguishable from the field separators the format
// is parsed by.
var b64 = base64.RawStdEncoding

// HashPassword returns an argon2id PHC string:
//
//	$argon2id$v=19$m=<KiB>,t=<time>,p=<lanes>$<b64 salt>$<b64 hash>
//
// The salt is fresh per call and stored beside the hash rather than kept
// somewhere central, so two operators who choose the same password have
// nothing in common in the database, and a stolen copy cannot be attacked once
// for every row at a time.
//
// The whole cost is written into the string rather than being assumed by the
// verifier. See the constants for why that is what makes the cost raisable.
func HashPassword(password string) (string, error) {
	salt := make([]byte, hashSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("drawing a salt to hash a password with: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, hashTime, hashMemory, hashThreads, hashKeyLen)

	return fmt.Sprintf("$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		phcAlgorithm, phcVersion, hashMemory, hashTime, hashThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether password produced encoded.
//
// The comparison is constant time, so that the number of leading bytes an
// attacker got right cannot be read off the clock. A malformed encoded string
// is an error and never a false match: a row whose hash was truncated by a bad
// restore must fail loudly rather than quietly rejecting the operator who owns
// the host, and an empty stored hash must never be something an empty password
// verifies against.
//
// The cost comes out of encoded rather than from the constants above, because
// a password hashed before those numbers were last raised is still that
// operator's password.
func VerifyPassword(encoded, password string) (bool, error) {
	settings, salt, want, err := parsePHC(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt,
		settings.time, settings.memory, settings.threads, settings.keyLen)

	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// cost is the part of a PHC string that says how expensive the hash was to
// make, in the units [argon2.IDKey] takes them in.
type cost struct {
	memory  uint32
	time    uint32
	threads uint8

	// keyLen is how long the stored hash is, carried here so that the caller
	// reproduces it at the length it was written at rather than at today's.
	keyLen uint32
}

// parsePHC pulls a hash apart into what it takes to reproduce it.
//
// Every field is checked, including the ones dokkup itself always writes the
// same way. This function's input is a column read back from a database file
// that a restore, a botched migration or an operator with sqlite3 may have
// touched, so "we wrote it, it must be well formed" is an assumption rather
// than a fact.
func parsePHC(encoded string) (cost, []byte, []byte, error) {
	// A leading "$" makes the first field empty; that is the format, not a
	// mistake.
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" {
		return cost{}, nil, nil, errors.New("reading a password hash: it is not a PHC string")
	}
	if fields[1] != phcAlgorithm {
		return cost{}, nil, nil, fmt.Errorf("reading a password hash: it names %q, not %s", fields[1], phcAlgorithm)
	}

	// Scanned and then written back out to be compared with what was read.
	// Sscanf stops at the first character it cannot use and reports no error
	// for the ones after it, so "v=19junk" scans as 19; a field that does not
	// reproduce itself is a field this package did not write.
	var version int
	if _, err := fmt.Sscanf(fields[2], "v=%d", &version); err != nil {
		return cost{}, nil, nil, fmt.Errorf("reading a password hash: its version field is unreadable: %w", err)
	}
	if fmt.Sprintf("v=%d", version) != fields[2] {
		return cost{}, nil, nil, fmt.Errorf("reading a password hash: its version field reads %q", fields[2])
	}
	if version != phcVersion {
		return cost{}, nil, nil, fmt.Errorf("reading a password hash: it is argon2 version %d, not %d",
			version, phcVersion)
	}

	var settings cost
	if _, err := fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d",
		&settings.memory, &settings.time, &settings.threads); err != nil {
		return cost{}, nil, nil, fmt.Errorf("reading a password hash: its cost is unreadable: %w", err)
	}
	if fmt.Sprintf("m=%d,t=%d,p=%d", settings.memory, settings.time, settings.threads) != fields[3] {
		return cost{}, nil, nil, fmt.Errorf("reading a password hash: its cost reads %q", fields[3])
	}
	// argon2 panics rather than erring on a cost of zero, and a zero cost is
	// exactly what a corrupted or hand-written field decodes to.
	if settings.memory == 0 || settings.time == 0 || settings.threads == 0 {
		return cost{}, nil, nil, errors.New("reading a password hash: its cost is zero")
	}
	if settings.memory > maxMemory || settings.time > maxTime {
		return cost{}, nil, nil, fmt.Errorf(
			"reading a password hash: it asks for %d KiB and %d passes, past the %d and %d allowed",
			settings.memory, settings.time, maxMemory, maxTime)
	}

	salt, err := b64.DecodeString(fields[4])
	if err != nil {
		return cost{}, nil, nil, fmt.Errorf("reading a password hash: its salt is unreadable: %w", err)
	}
	key, err := b64.DecodeString(fields[5])
	if err != nil {
		return cost{}, nil, nil, fmt.Errorf("reading a password hash: it is unreadable: %w", err)
	}
	if len(salt) == 0 || len(key) == 0 {
		return cost{}, nil, nil, errors.New("reading a password hash: it carries no salt or no hash")
	}
	if len(key) > maxKeyLen {
		return cost{}, nil, nil, fmt.Errorf("reading a password hash: it is %d bytes long, past the %d allowed",
			len(key), maxKeyLen)
	}
	// The bound above is what makes the conversion safe, which the linter
	// cannot see from here.
	settings.keyLen = uint32(len(key)) //nolint:gosec // G115: len(key) is checked against maxKeyLen on the line before
	return settings, salt, key, nil
}
