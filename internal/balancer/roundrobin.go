package balancer

import (
	"sync/atomic"

	"github.com/lechandonga/resilient-edge-gateway-go/internal/backend"
)

// RoundRobin distributes requests across a pool's healthy instances
// using an atomically incremented counter. No per-request lock is taken,
// and each healthy instance is chosen roughly equally often (counter mod
// number of healthy instances), so traffic is never pinned to one node.
type RoundRobin struct {
	counter atomic.Uint64
}

// NewRoundRobin creates a round-robin balancer.
func NewRoundRobin() *RoundRobin {
	return &RoundRobin{}
}

// Pick returns the next healthy instance. It returns nil when no healthy
// instances are available. The caller must not pass an empty slice.
func (b *RoundRobin) Pick(healthy []*backend.Instance) *backend.Instance {
	n := uint64(len(healthy))
	if n == 0 {
		return nil
	}
	idx := b.counter.Add(1) - 1
	return healthy[idx%n]
}
