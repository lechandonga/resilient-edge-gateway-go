package backend

import "sync"

// UpdateResult explains what happened after feeding one outcome
// (a proxied request or an active probe) into the pool. It is used to
// build the "why" part of gateway logs.
type UpdateResult struct {
	// Changed is true when the instance flipped healthy<->unhealthy.
	Changed bool
	From    State
	To      State
	// Streak is the current consecutive failure (while healthy) or
	// success (while unhealthy) count after applying this outcome.
	Streak int
	// Threshold that must be reached for a state flip.
	Threshold int
}

// Pool groups the instances that back one route/service and keeps
// per-instance consecutive failure/success streaks. A single mutex guards
// streak mutation so passive request feedback and active probe feedback
// can never race each other.
type Pool struct {
	RouteName        string
	FailureThreshold int // consecutive failures required to eject
	SuccessThreshold int // consecutive successes required to re-admit

	mu        sync.RWMutex
	instances []*Instance
	// streak is consecutive failures while healthy, consecutive
	// successes while unhealthy.
	streak map[*Instance]int
}

// NewPool builds a pool from a route's configured backend addresses.
// All instances start healthy.
func NewPool(routeName string, backends []string, failureThreshold, successThreshold int) *Pool {
	p := &Pool{
		RouteName:        routeName,
		FailureThreshold: failureThreshold,
		SuccessThreshold: successThreshold,
		streak:           make(map[*Instance]int),
	}
	for _, addr := range backends {
		p.instances = append(p.instances, NewInstance(addr))
	}
	return p
}

// All returns a copy of every instance in the pool.
func (p *Pool) All() []*Instance {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Instance, len(p.instances))
	copy(out, p.instances)
	return out
}

// Healthy returns a copy of the currently healthy instances.
func (p *Pool) Healthy() []*Instance {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Instance, 0, len(p.instances))
	for _, inst := range p.instances {
		if inst.Healthy() {
			out = append(out, inst)
		}
	}
	return out
}

// RecordResult applies passive feedback from a proxied request.
// Only healthy instances ever receive traffic, so ok=true clears the
// failure streak while ok=false extends it (and may eject the instance).
func (p *Pool) RecordResult(inst *Instance, ok bool) UpdateResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.apply(inst, ok)
}

// ProbeResult applies feedback from an active health probe. A streak of
// successes can re-admit an ejected instance; a streak of failures can
// eject a healthy one.
func (p *Pool) ProbeResult(inst *Instance, ok bool) UpdateResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.apply(inst, ok)
}

func (p *Pool) apply(inst *Instance, ok bool) UpdateResult {
	from := inst.State()
	res := UpdateResult{From: from, To: from}

	switch {
	case ok && from == StateUnhealthy:
		p.streak[inst]++
		res.Streak = p.streak[inst]
		res.Threshold = p.SuccessThreshold
		if res.Streak >= p.SuccessThreshold {
			inst.markState(StateHealthy)
			p.streak[inst] = 0
			res.Changed = true
			res.To = StateHealthy
		}
	case ok: // healthy: one success proves the streak of failures was a jitter
		p.streak[inst] = 0
	case !ok && from == StateHealthy:
		p.streak[inst]++
		res.Streak = p.streak[inst]
		res.Threshold = p.FailureThreshold
		if res.Streak >= p.FailureThreshold {
			inst.markState(StateUnhealthy)
			p.streak[inst] = 0
			res.Changed = true
			res.To = StateUnhealthy
		}
	default: // !ok && already unhealthy: a failed probe breaks the
		// run of consecutive successes, so reset the recovery streak.
		p.streak[inst] = 0
		res.Streak = 0
		res.Threshold = p.SuccessThreshold
	}
	return res
}

// Streak returns the current streak counter for an instance.
func (p *Pool) Streak(inst *Instance) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.streak[inst]
}
