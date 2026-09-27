package backend

import "testing"

func TestPool_EjectsAfterConsecutiveFailures(t *testing.T) {
	p := NewPool("svc", []string{"h1:1", "h2:2"}, 3, 2)
	h1 := p.All()[0]
	if !h1.Healthy() {
		t.Fatal("instances start healthy")
	}

	// Two failures stay in pool.
	for i := 1; i <= 2; i++ {
		res := p.RecordResult(h1, false)
		if res.Changed || !h1.Healthy() {
			t.Fatalf("failure %d must not eject (streak=%d threshold=%d)", i, res.Streak, res.Threshold)
		}
		t.Logf("failure %d: streak=%d/%d state=%s (basis: below threshold)", i, res.Streak, res.Threshold, h1.State())
	}
	// Third failure ejects.
	res := p.RecordResult(h1, false)
	if !res.Changed || res.To != StateUnhealthy || h1.Healthy() {
		t.Fatalf("third failure should eject, got changed=%v to=%s", res.Changed, res.To)
	}
	t.Logf("failure 3: EJECTED (basis: streak=3 >= failure_threshold=3)")

	if healthy := p.Healthy(); len(healthy) != 1 || healthy[0].Address != "h2:2" {
		t.Fatalf("only h2 should remain healthy, got %v", healthy)
	}
}

func TestPool_SuccessResetsFailureStreak(t *testing.T) {
	p := NewPool("svc", []string{"h1:1"}, 3, 2)
	h1 := p.All()[0]

	p.RecordResult(h1, false)
	p.RecordResult(h1, false)
	if res := p.RecordResult(h1, true); res.Streak != 0 {
		t.Fatalf("success should reset streak to 0, got %d", res.Streak)
	}
	t.Log("two failures then success -> streak reset to 0 (basis: jitter tolerated)")

	p.RecordResult(h1, false)
	if !h1.Healthy() {
		t.Fatal("isolated failure after a success must not eject")
	}
	t.Log("one failure after reset: still HEALTHY (basis: streak restarted at 1)")
}

func TestPool_RecoversAfterConsecutiveSuccesses(t *testing.T) {
	p := NewPool("svc", []string{"h1:1"}, 1, 2)
	h1 := p.All()[0]

	p.RecordResult(h1, false) // ejected immediately (threshold 1)
	if h1.Healthy() {
		t.Fatal("should be ejected")
	}

	// First probe success: streak 1/2, still unhealthy.
	if res := p.ProbeResult(h1, true); res.Changed || h1.Healthy() {
		t.Fatalf("first success must not recover, streak=%d", res.Streak)
	}
	t.Log("probe ok#1: streak=1/2 state=unhealthy")
	// A failed probe in between resets the recovery streak.
	p.ProbeResult(h1, false)
	t.Log("probe fail in between -> recovery streak reset")
	p.ProbeResult(h1, true)
	if h1.Healthy() {
		t.Fatal("one success after an interrupting failure must not recover")
	}
	t.Log("probe ok#1 (after reset): streak=1/2 still unhealthy (basis: successes must be consecutive)")
	if res := p.ProbeResult(h1, true); !res.Changed || !h1.Healthy() {
		t.Fatalf("second consecutive success should recover, got changed=%v", res.Changed)
	}
	t.Log("probe ok#2: RECOVERED (basis: streak=2 >= success_threshold=2)")
}
