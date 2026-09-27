package balancer

import (
	"testing"

	"github.com/lechandonga/resilient-edge-gateway-go/internal/backend"
)

func TestRoundRobin_SpreadsEvenly(t *testing.T) {
	p := backend.NewPool("svc", []string{"h1:1", "h2:2", "h3:3"}, 3, 2)
	b := NewRoundRobin()
	healthy := p.Healthy()

	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		inst := b.Pick(healthy)
		if inst == nil {
			t.Fatal("picked nil with 3 healthy instances")
		}
		counts[inst.Address]++
	}
	for _, h := range healthy {
		if counts[h.Address] != 3 {
			t.Fatalf("expected 3 each, got %v", counts)
		}
	}
	t.Logf("9 picks over 3 instances: %v (basis: counter mod len(healthy))", counts)
}

func TestRoundRobin_EmptyPoolReturnsNil(t *testing.T) {
	b := NewRoundRobin()
	if got := b.Pick(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	t.Log("empty healthy set -> nil (basis: gateway then returns no_healthy_backend)")
}
