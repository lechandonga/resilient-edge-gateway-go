package ratelimit

import (
	"sync"
	"time"
)

// Limiter 限流器接口：Allow 判断当前请求是否放行。
type Limiter interface {
	Allow() bool
}

// TokenBucket 令牌桶：按固定速率补充令牌，桶满时多余令牌丢弃。
//
// 初始令牌数为满桶（burst），允许短时突发；之后每秒补充 rate 个。
// 令牌计算是惰性的：只在 Allow 被调用时根据距上次调用经过的时间补充，
// 不需要后台 goroutine，因此网关自身不会被定时器/goroutine 拖垮。
type TokenBucket struct {
	mu sync.Mutex

	rate   float64 // 每秒生成令牌数
	burst  float64 // 桶容量
	tokens float64 // 当前令牌数
	last   time.Time
	now    func() time.Time
}

// NewTokenBucket 创建令牌桶限流器。rate 为每秒令牌数，burst 为桶容量。
func NewTokenBucket(rate float64, burst int) *TokenBucket {
	b := &TokenBucket{
		rate:  rate,
		burst: float64(burst),
		now:   time.Now,
	}
	// 初始满桶：服务刚启动时允许一次突发。
	b.tokens = b.burst
	b.last = b.now()
	return b
}

// Allow 尝试取 1 个令牌；取不到返回 false（调用方应拒绝请求）。
func (b *TokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now

	// 按经过时间补充令牌，但不超过桶容量。
	b.tokens += elapsed * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// 编译期保证实现接口。
var _ Limiter = (*TokenBucket)(nil)
