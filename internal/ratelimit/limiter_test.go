package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter_BurstThenRejectThenRefill(t *testing.T) {
	base := time.Now()
	l := New(10, 3) // 10 rps, burst 3; bucket starts full

	for i := 1; i <= 3; i++ {
		d := l.TryTake(base)
		if !d.Allowed {
			t.Fatalf("request %d within burst should pass", i)
		}
		t.Logf("take %d allowed, remaining=%.2f (basis: token consumed)", i, d.Tokens)
	}
	if d := l.TryTake(base); d.Allowed {
		t.Fatal("4th immediate request must be rejected")
	} else {
		t.Logf("take 4 rejected, remaining=%.2f (basis: no token available)", d.Tokens)
	}

	// Advance 100ms -> 1 token refilled at 10 rps.
	d := l.TryTake(base.Add(100 * time.Millisecond))
	if !d.Allowed {
		t.Fatal("request after 100ms should pass once a token refilled")
	}
	t.Logf("take after 100ms allowed, remaining=%.2f (basis: 10rps refills 1 token / 100ms)", d.Tokens)

	// Immediately again -> rejected.
	if d := l.TryTake(base.Add(100 * time.Millisecond)); d.Allowed {
		t.Fatal("immediate follow-up must still be rejected")
	}
	t.Log("immediate follow-up rejected (basis: refilled token already spent)")
}

func TestLimiter_DisabledAllowsAll(t *testing.T) {
	l := New(0, 0)
	for i := 0; i < 100; i++ {
		if !l.TryTake(time.Now()).Allowed {
			t.Fatal("disabled limiter must allow every request")
		}
	}
	t.Log("rate<=0: all 100 requests allowed (basis: limiting disabled)")
}

func TestLimiter_BucketCapsAtBurst(t *testing.T) {
	base := time.Now()
	l := New(10, 2)
	// Idle a long time: tokens must not grow beyond burst.
	d := l.TryTake(base.Add(time.Hour))
	if !d.Allowed || d.Tokens > 1.5 {
		t.Fatalf("idle bucket should cap at burst=2, tokens after one take=%.2f", d.Tokens)
	}
	t.Logf("after 1h idle, tokens capped: remaining=%.2f (basis: min(elapsed*rate, burst))", d.Tokens)
}
