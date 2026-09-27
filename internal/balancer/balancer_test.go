package balancer

import (
	"errors"
	"testing"
)

// TestRoundRobinDistributes 验证候选稳定时严格轮询，不总打同一个实例。
func TestRoundRobinDistributes(t *testing.T) {
	b := NewRoundRobin()
	candidates := []string{"a", "b", "c"}

	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		got, err := b.Pick(candidates)
		if err != nil {
			t.Fatalf("Pick %d: %v", i, err)
		}
		counts[got]++
	}
	if counts["a"] != 3 || counts["b"] != 3 || counts["c"] != 3 {
		t.Fatalf("9 次选择应均匀分布 3/3/3，实际 %v", counts)
	}
	t.Logf("[test] 轮询命中分布: %v，判定依据: 单调计数器对候选数取模", counts)
}

// TestRoundRobinChangingCandidates 验证摘除/恢复导致候选变化时
// 仍然只在当前候选间轮转，且无候选时返回 ErrNoCandidate。
func TestRoundRobinChangingCandidates(t *testing.T) {
	b := NewRoundRobin()

	if _, err := b.Pick(nil); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("无候选应返回 ErrNoCandidate，实际 %v", err)
	}

	// 只剩 a、c（模拟 b 被摘除），二者都应被选到。
	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		got, err := b.Pick([]string{"a", "c"})
		if err != nil {
			t.Fatalf("Pick %d: %v", i, err)
		}
		counts[got]++
	}
	if counts["a"] != 2 || counts["c"] != 2 {
		t.Fatalf("摘除 b 后应在 a/c 间均分，实际 %v", counts)
	}
	t.Logf("[test] 候选变化后命中分布: %v，判定依据: 每次只对当前健康候选取模", counts)
}
