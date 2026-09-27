package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a goroutine-safe token bucket with lazy refill: tokens are
// recomputed from elapsed time only when TryTake is called, so no
// background goroutine or timer is needed (nothing to leak).
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens added per second
	burst  float64 // bucket capacity
	tokens float64 // tokens currently available
	last   time.Time
}

// Decision records whether a request was allowed and the bucket level
// after the attempt, which is the evidence used in log lines.
type Decision struct {
	Allowed bool
	Tokens  float64
	Rate    float64
	Burst   int
}

// New builds a token-bucket limiter starting full. A rate <= 0 means
// limiting is disabled (every request is allowed).
func New(requestsPerSecond float64, burst int) *Limiter {
	l := &Limiter{
		rate:  requestsPerSecond,
		burst: float64(burst),
		last:  time.Now(),
	}
	l.tokens = l.burst
	return l
}

// TryTake attempts to consume one token at time now.
func (l *Limiter) TryTake(now time.Time) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.rate <= 0 {
		return Decision{Allowed: true, Tokens: 0, Rate: 0, Burst: 0}
	}

	elapsed := now.Sub(l.last).Seconds()
	if elapsed > 0 {
		l.tokens += elapsed * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
	}
	// Always advance the clock anchor (even on the first call or a
	// slightly-earlier synthetic timestamp) so later refills are exact.
	l.last = now

	d := Decision{Allowed: false, Tokens: l.tokens, Rate: l.rate, Burst: int(l.burst)}
	if l.tokens >= 1 {
		l.tokens--
		d.Allowed = true
		d.Tokens = l.tokens
	}
	return d
}
