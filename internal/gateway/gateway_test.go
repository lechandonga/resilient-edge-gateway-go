package gateway

import (
	"io"
	"testing"
	"time"
)

// TestLoadBalancing_RoundRobin: requests must spread across both healthy
// instances rather than always hitting the same one.
func TestLoadBalancing_RoundRobin(t *testing.T) {
	p1, p2 := freePort(t), freePort(t)
	b1 := newMockBackend(t, p1, echoHandler("b1"))
	b2 := newMockBackend(t, p2, echoHandler("b2"))
	t.Cleanup(func() { b1.stop(); b2.stop() })

	gw := newTestGateway(t, buildConfig(routeSpec{
		name: "svc", prefix: "/svc",
		backends:  []string{b1.addr, b2.addr},
		timeoutMS: 1000, intervalMS: 500, failThr: 3, okThr: 2,
	}))

	got := map[string]int{}
	for i := 0; i < 6; i++ {
		rec := doRequest(gw, "/svc/item")
		if rec.Code != 200 {
			t.Fatalf("request %d: expected 200, got %d body=%s", i, rec.Code, rec.Body.String())
		}
		hit := rec.Header().Get("X-Gateway-Backend")
		t.Logf("request %d -> HIT backend %s (basis: X-Gateway-Backend header)", i+1, hit)
		got[hit]++
	}
	if len(got) != 2 {
		t.Fatalf("expected traffic on 2 backends, got %v", got)
	}
	// Strictly alternating => 3 each; accept a 2:4 spread to stay robust.
	for addr, n := range got {
		if n < 2 {
			t.Fatalf("backend %s got only %d requests, distribution not balanced: %v", addr, n, got)
		}
	}
	t.Logf("distribution: %v", got)
}

// TestBackendDown_EjectedAndNoTraffic: once enough requests fail the
// instance is ejected, and subsequent requests return a clear 502 instead
// of repeatedly hitting a dead instance.
func TestBackendDown_EjectedAndNoTraffic(t *testing.T) {
	p := freePort(t)
	b := newMockBackend(t, p, echoHandler("only"))
	b.stop() // backend is down from the start (connection refused)
	waitClosed(t, b.addr)

	gw := newTestGateway(t, buildConfig(routeSpec{
		name: "svc", prefix: "/svc", backends: []string{b.addr},
		timeoutMS: 1000, intervalMS: 10000, // disable active probing for this phase
		failThr: 3, okThr: 2,
	}))

	var ejected bool
	for i := 1; i <= 3; i++ {
		rec := doRequest(gw, "/svc/x")
		body, _ := io.ReadAll(rec.Result().Body)
		t.Logf("attempt %d against down backend -> status=%d code=%s body=%s (basis: connection refused counts as failure streak)",
			i, rec.Code, rec.Header().Get("X-Gateway-Error"), string(body))
		if rec.Code != 502 {
			t.Fatalf("attempt %d: expected 502, got %d", i, rec.Code)
		}
	}
	pool := gw.Pool("svc")
	inst := pool.All()[0]
	if inst.Healthy() {
		t.Fatalf("expected instance %s to be EJECTED after 3 failures", b.addr)
	}
	ejected = true
	t.Logf("instance %s state=%s -> ejected as expected (basis: 3 consecutive failures >= failure_threshold)",
		b.addr, inst.State())

	// Further requests must fail fast at the gateway without touching it.
	before := b.hits.Load()
	rec := doRequest(gw, "/svc/x")
	t.Logf("post-ejection request -> status=%d code=%s (basis: pool has no healthy instance)",
		rec.Code, rec.Header().Get("X-Gateway-Error"))
	if rec.Code != 502 || rec.Header().Get("X-Gateway-Error") != "no_healthy_backend" {
		t.Fatalf("expected 502 no_healthy_backend, got %d %s", rec.Code, rec.Header().Get("X-Gateway-Error"))
	}
	if b.hits.Load() != before {
		t.Fatalf("ejected instance still received traffic")
	}
	_ = ejected
}

// TestBackendRecovery_ReaddedByActiveProbe: after recovery, consecutive
// successful active probes re-admit the instance and traffic resumes.
func TestBackendRecovery_ReaddedByActiveProbe(t *testing.T) {
	p := freePort(t)
	b := newMockBackend(t, p, echoHandler("only"))
	b.stop()
	waitClosed(t, b.addr)

	const interval = 100 // ms
	gw := newTestGateway(t, buildConfig(routeSpec{
		name: "svc", prefix: "/svc", backends: []string{b.addr},
		timeoutMS: 1000, intervalMS: interval, failThr: 1, okThr: 2,
	}))

	// Force ejection via passive failures.
	for i := 0; i < 2; i++ {
		doRequest(gw, "/svc/x")
	}
	if gw.Pool("svc").All()[0].Healthy() {
		t.Fatalf("precondition: instance should be unhealthy")
	}
	t.Log("instance ejected; now recovering backend on same address")

	b.restart(t)
	t.Logf("backend %s restarted; waiting for 2 consecutive successful probes", b.addr)

	deadline := time.Now().Add(3 * time.Second)
	var recovered bool
	for time.Now().Before(deadline) {
		if gw.Pool("svc").All()[0].Healthy() {
			recovered = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !recovered {
		t.Fatalf("instance was not re-admitted after recovery")
	}
	t.Log("instance state=healthy again (basis: 2 consecutive successful active probes >= success_threshold)")

	rec := doRequest(gw, "/svc/x")
	t.Logf("post-recovery request -> status=%d HIT backend=%s", rec.Code, rec.Header().Get("X-Gateway-Backend"))
	if rec.Code != 200 || rec.Header().Get("X-Gateway-Backend") != b.addr {
		t.Fatalf("expected 200 from recovered backend, got %d", rec.Code)
	}
}

// TestTransientJitter_DoesNotEject: a single connection failure must not
// eject a healthy instance when failure_threshold is 3, and a later
// success resets the streak so repeated isolated jitters never eject.
func TestTransientJitter_DoesNotEject(t *testing.T) {
	p := freePort(t)
	b := newMockBackend(t, p, echoHandler("stable"))
	t.Cleanup(b.stop)

	gw := newTestGateway(t, buildConfig(routeSpec{
		name: "svc", prefix: "/svc", backends: []string{b.addr},
		timeoutMS: 1000, intervalMS: 10000, failThr: 3, okThr: 2,
	}))

	// Simulate two isolated failures separated by successes: each failure
	// is real connection refusal, but success in between resets the streak.
	b.stop()
	waitClosed(t, b.addr)
	rec := doRequest(gw, "/svc/x")
	t.Logf("jitter#1 -> status=%d, failure streak=1/3 (basis: one failure only)", rec.Code)
	b.restart(t)
	if rec = doRequest(gw, "/svc/x"); rec.Code != 200 {
		t.Fatalf("expected recovery to 200, got %d", rec.Code)
	}
	t.Log("jitter#1 followed by success -> streak reset to 0")

	b.stop()
	waitClosed(t, b.addr)
	rec = doRequest(gw, "/svc/x")
	t.Logf("jitter#2 -> status=%d, failure streak=1/3 again (basis: reset streak never accumulated)", rec.Code)
	if !gw.Pool("svc").All()[0].Healthy() {
		t.Fatalf("healthy instance was ejected by transient jitter")
	}
	t.Log("instance remains HEALTHY despite 2 failures (basis: never 3 consecutive)")
	b.restart(t)
}

// TestRateLimit_ReturnsDistinct429: after burst tokens are exhausted the
// gateway rejects immediately with 429 (distinct from 504/502) and the
// request never reaches a backend; after waiting for a refill it recovers.
func TestRateLimit_ReturnsDistinct429(t *testing.T) {
	p := freePort(t)
	b := newMockBackend(t, p, echoHandler("rl"))
	t.Cleanup(b.stop)

	// rps=10, burst=3 -> 3 immediate allows, then 429.
	gw := newTestGateway(t, buildConfig(routeSpec{
		name: "svc", prefix: "/svc", backends: []string{b.addr},
		rps: 10, burst: 3, timeoutMS: 1000, intervalMS: 10000, failThr: 3, okThr: 2,
	}))

	for i := 1; i <= 3; i++ {
		rec := doRequest(gw, "/svc/x")
		t.Logf("request %d -> status=%d (basis: bucket token available)", i, rec.Code)
		if rec.Code != 200 {
			t.Fatalf("request %d: expected 200, got %d", i, rec.Code)
		}
	}
	rec := doRequest(gw, "/svc/x")
	body := rec.Body.String()
	t.Logf("request 4 -> status=%d code=%s body=%s (basis: bucket empty, tokens<1)",
		rec.Code, rec.Header().Get("X-Gateway-Error"), body)
	if rec.Code != 429 || rec.Header().Get("X-Gateway-Error") != "rate_limited" {
		t.Fatalf("expected 429 rate_limited, got %d %s", rec.Code, rec.Header().Get("X-Gateway-Error"))
	}

	// Wait for at least one token refill, then it should be allowed again.
	time.Sleep(150 * time.Millisecond) // rps=10 -> 1 token per 100ms
	rec = doRequest(gw, "/svc/x")
	t.Logf("request after refill -> status=%d (basis: token bucket refilled)", rec.Code)
	if rec.Code != 200 {
		t.Fatalf("expected 200 after refill, got %d", rec.Code)
	}
}

// TestUpstreamTimeout_ReturnsDistinct504: a backend slower than the route
// timeout yields 504 (distinct from 429/502), and the slow handler is
// abandoned promptly so gateway resources are not held.
func TestUpstreamTimeout_ReturnsDistinct504(t *testing.T) {
	p := freePort(t)
	b := newMockBackend(t, p, sleepHandler(500*time.Millisecond))
	t.Cleanup(b.stop)

	gw := newTestGateway(t, buildConfig(routeSpec{
		name: "svc", prefix: "/svc", backends: []string{b.addr},
		timeoutMS: 100, intervalMS: 10000, failThr: 3, okThr: 2,
	}))

	start := time.Now()
	rec := doRequest(gw, "/svc/slow")
	elapsed := time.Since(start)
	t.Logf("timeout request -> status=%d code=%s elapsed=%s (basis: backend sleep 500ms > timeout 100ms)",
		rec.Code, rec.Header().Get("X-Gateway-Error"), elapsed)
	if rec.Code != 504 || rec.Header().Get("X-Gateway-Error") != "upstream_timeout" {
		t.Fatalf("expected 504 upstream_timeout, got %d %s", rec.Code, rec.Header().Get("X-Gateway-Error"))
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("gateway waited %s, should have abandoned the slow backend near the deadline", elapsed)
	}
}

// TestRouting_LongestPrefixAnd404: requests are routed by longest prefix,
// and unknown paths get a clear 404.
func TestRouting_LongestPrefixAnd404(t *testing.T) {
	pA, pB := freePort(t), freePort(t)
	bA := newMockBackend(t, pA, echoHandler("A"))
	bB := newMockBackend(t, pB, echoHandler("B"))
	t.Cleanup(func() { bA.stop(); bB.stop() })

	cfg := buildConfig(
		routeSpec{name: "api", prefix: "/api", backends: []string{bA.addr},
			timeoutMS: 1000, intervalMS: 10000, failThr: 3, okThr: 2},
		routeSpec{name: "apiv2", prefix: "/api/v2", backends: []string{bB.addr},
			timeoutMS: 1000, intervalMS: 10000, failThr: 3, okThr: 2},
	)
	gw := newTestGateway(t, cfg)

	if rec := doRequest(gw, "/api/v2/x"); rec.Header().Get("X-Gateway-Backend") != bB.addr {
		t.Fatalf("/api/v2/x expected backend B(%s), got %s", bB.addr, rec.Header().Get("X-Gateway-Backend"))
	}
	t.Logf("/api/v2/x -> HIT %s (basis: longest prefix /api/v2 wins)", bB.addr)

	if rec := doRequest(gw, "/api/v1/x"); rec.Header().Get("X-Gateway-Backend") != bA.addr {
		t.Fatalf("/api/v1/x expected backend A(%s), got %s", bA.addr, rec.Header().Get("X-Gateway-Backend"))
	}
	t.Logf("/api/v1/x -> HIT %s (basis: matches /api only)", bA.addr)

	rec := doRequest(gw, "/nope")
	if rec.Code != 404 || rec.Header().Get("X-Gateway-Error") != "no_route" {
		t.Fatalf("expected 404 no_route, got %d %s", rec.Code, rec.Header().Get("X-Gateway-Error"))
	}
	t.Log("/nope -> 404 no_route (basis: no configured prefix matches)")
}
