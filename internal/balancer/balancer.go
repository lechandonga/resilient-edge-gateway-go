package balancer

import (
	"errors"
	"sync/atomic"
)

// ErrNoCandidate 当前没有任何健康实例可选。
var ErrNoCandidate = errors.New("balancer: no healthy candidate available")

// Balancer 在一组健康实例间做负载均衡。
type Balancer interface {
	// Pick 从候选实例中选一个；无可用实例时返回 ErrNoCandidate。
	Pick(candidates []string) (string, error)
}

// RoundRobin 轮询负载均衡器。
//
// 健康实例集合会随摘除/恢复变化（候选列表长度、顺序都可能变），
// 因此不用"下标对 len 取模"（那样在实例摘除后会把流量集中打回第一个），
// 而是维护一个单调递增计数，对当前候选长度取模，保证每个实例都能轮到，
// 不会总打同一个实例。
type RoundRobin struct {
	counter uint64
}

// NewRoundRobin 创建轮询均衡器。
func NewRoundRobin() *RoundRobin {
	return &RoundRobin{}
}

func (b *RoundRobin) Pick(candidates []string) (string, error) {
	if len(candidates) == 0 {
		return "", ErrNoCandidate
	}
	n := atomic.AddUint64(&b.counter, 1) - 1
	return candidates[int(n%uint64(len(candidates)))], nil
}

// 编译期保证实现接口。
var _ Balancer = (*RoundRobin)(nil)
