package server

import (
	"math"
	"sync"
	"time"
)

// tokenBucket is the whole of dokkup's rate limiting: a counter that refills
// with the clock, shared by every caller of the route it guards.
//
// It is written here rather than taken from golang.org/x/time/rate, which does
// exactly this and does it better, for two reasons. The first is that a
// dependency added to a binary that must stay under its size budget has to buy
// more than twenty lines. The second is the clock: x/time/rate reads
// [time.Now] internally, so a test for "the sixth attempt in a minute is
// refused and the seventh a minute later is not" would have to sleep for a
// real minute or assert nothing. The clock here is a field, so that test runs
// in microseconds and asserts the thing that matters.
//
// Global to the process and not per client address, deliberately. dokkup sits
// behind nginx, so the address this process sees is whatever the proxy chose
// to pass on -- a header an attacker sets is not an identity, and the socket
// address is the proxy's. A global bucket is refusable by anyone who wants to
// lock out the operator for a minute, which is a denial of service on a screen
// used once per installation; a per-address bucket is bypassable by anyone
// with a header, which is a denial of the protection itself.
type tokenBucket struct {
	mu sync.Mutex

	// tokens is fractional so that a refill smaller than one attempt is not
	// lost to truncation. Ten requests twelve seconds apart must cost ten
	// tokens and earn ten back, not zero.
	tokens float64

	// burst is both the capacity and what the bucket starts full at: an
	// installation's first redemption must not wait for a refill.
	burst float64

	// refill is how long one token takes to come back. Expressed per token
	// rather than as a rate so that the wait handed to Retry-After is this
	// value scaled, with no second unit to get wrong.
	refill time.Duration

	last time.Time
	now  func() time.Time
}

func newTokenBucket(burst int, refill time.Duration, now func() time.Time) *tokenBucket {
	return &tokenBucket{
		tokens: float64(burst),
		burst:  float64(burst),
		refill: refill,
		last:   now(),
		now:    now,
	}
}

// take spends one token, reporting whether there was one and, if not, how long
// until there is.
//
// The refusal carries the wait because the caller must answer Retry-After with
// it: a client told only "too many attempts" retries immediately, which is how
// a rate limit turns into a busy loop against the thing it was protecting.
func (b *tokenBucket) take() (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Refilled on read rather than by a ticker, so the bucket costs nothing
	// while nobody is attacking it and there is no goroutine to stop.
	now := b.now()
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+float64(elapsed)/float64(b.refill))
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) * float64(b.refill))
}

// retryAfterSeconds turns a wait into the whole number of seconds RFC 9110
// allows in the header, never below one: rounding a 400ms wait down to "0"
// invites the immediate retry the header exists to prevent.
func retryAfterSeconds(wait time.Duration) int {
	seconds := int(math.Ceil(wait.Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}
