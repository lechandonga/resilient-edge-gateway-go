package backend

import (
	"sync/atomic"
)

// State is the health state of a backend instance.
type State int32

const (
	// StateHealthy means the instance may receive traffic.
	StateHealthy State = iota
	// StateUnhealthy means the instance has been ejected from the pool.
	StateUnhealthy
)

func (s State) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// Instance is one backend endpoint with thread-safe health bookkeeping.
type Instance struct {
	Address string
	state   atomic.Int32
}

// NewInstance creates an instance that starts healthy.
func NewInstance(address string) *Instance {
	i := &Instance{Address: address}
	i.state.Store(int32(StateHealthy))
	return i
}

// State returns the current health state.
func (i *Instance) State() State {
	return State(i.state.Load())
}

// Healthy reports whether the instance is currently eligible for traffic.
func (i *Instance) Healthy() bool {
	return i.State() == StateHealthy
}

// markState sets the health state.
func (i *Instance) markState(s State) {
	i.state.Store(int32(s))
}

// String identifies the instance in log lines.
func (i *Instance) String() string {
	return i.Address + "(" + i.State().String() + ")"
}
