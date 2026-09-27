package ratelimit

import (
	"testing"
	"time"
)

// fakeClock 可控时钟，避免测试依赖真实 sleep。
type fakeClock struct{ t time.Time }

func (f *fakeClock) nowFunc() time.Time { return f.t }

// TestTokenBucketBurstAndRefill 验证：
//   - 初始满桶可放行 burst 个；
//   - 桶空后立即请求被拒；
//   - 按经过时间补充令牌后恢复放行；
//   - 补充量不超过桶容量。
func TestTokenBucketBurstAndRefill(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	b := &TokenBucket{
		rate:   2, // 2 个/秒
		burst:  3,
		tokens: 3,
		last:   clk.t,
		now:    clk.nowFunc,
	}

	allowed := 0
	for i := 0; i < 3; i++ {
		if b.Allow() {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("满桶应放行 3 个，实际 %d", allowed)
	}
	if b.Allow() {
		t.Fatalf("桶空后应拒绝")
	}
	t.Logf("[test] 满桶放行 3 个后拒绝，判定依据: tokens 耗尽")

	// 前进 0.5s，应补充 1 个令牌。
	clk.t = clk.t.Add(500 * time.Millisecond)
	if !b.Allow() {
		t.Fatalf("补充 1 个令牌后应放行")
	}
	if b.Allow() {
		t.Fatalf("仅剩 0 个令牌应拒绝")
	}
	t.Logf("[test] 0.5s 后按 rate=2/s 补充 1 个令牌并放行，判定依据: elapsed*rate")

	// 前进很久也只补到桶容量 3。
	clk.t = clk.t.Add(10 * time.Second)
	n := 0
	for b.Allow() {
		n++
	}
	if n != 3 {
		t.Fatalf("长时间后令牌数应被截断为 burst=3，实际取到 %d", n)
	}
	t.Logf("[test] 10s 后最多只取到 3 个令牌，判定依据: 令牌数被 min(tokens, burst) 截断")
}
