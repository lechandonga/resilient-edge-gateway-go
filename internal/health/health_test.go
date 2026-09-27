package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newProbeServer 起一个可切换健康状态的探测服务器。
func newProbeServer(healthy *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
}

func addr(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://")
}

// TestThresholdTransitions 直接验证连续计数阈值：
//   - 连续失败达到阈值才摘除，少一次都不行；
//   - 摘除后连续成功达到阈值才恢复。
func TestThresholdTransitions(t *testing.T) {
	c := NewChecker([]string{"a"})
	inst := c.instances["a"]
	inst.state = StateHealthy

	// 连续失败 2 次（阈值 3）不应摘除。
	inst.recordFailure(3)
	inst.recordFailure(3)
	if inst.State() != StateHealthy {
		t.Fatalf("连续失败 2 次不应摘除")
	}
	// 一次成功清零失败计数 —— 抗抖动。
	inst.recordSuccess(2)
	_, _, failStreak := inst.Info()
	if failStreak != 0 {
		t.Fatalf("一次成功后失败计数应清零，实际 %d", failStreak)
	}
	// 再来 3 次连续失败才摘除。
	inst.recordFailure(3)
	inst.recordFailure(3)
	changed, s := inst.recordFailure(3)
	if !changed || s != StateUnhealthy {
		t.Fatalf("连续失败 3 次应迁移到 UNHEALTHY")
	}
	t.Logf("[test] 摘除判定: 前 2 次失败被一次成功清零，再次连续失败满 3 次才摘除 判定依据: 连续失败计数 >= unhealthy_threshold")

	// 恢复需要连续成功 2 次。
	inst.recordSuccess(2)
	if inst.State() != StateUnhealthy {
		t.Fatalf("仅 1 次成功不应恢复")
	}
	changed, s = inst.recordSuccess(2)
	if !changed || s != StateHealthy {
		t.Fatalf("连续成功 2 次应恢复 HEALTHY")
	}
	t.Logf("[test] 恢复判定: 连续成功满 2 次才从 UNHEALTHY 回到 HEALTHY 判定依据: 连续成功计数 >= healthy_threshold")
}

// TestCheckerActiveProbe 启动真实 HTTP 探测循环，验证：
// 初始 UNKNOWN 的实例被探活后进入健康池；挂掉后被摘除；恢复后重新入池。
func TestCheckerActiveProbe(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := newProbeServer(&healthy)
	defer srv.Close()

	c := NewChecker([]string{addr(srv)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.Start(ctx, 50*time.Millisecond, 200*time.Millisecond, "/healthz", 2, 3)

	waitUntil(t, func() bool { return c.IsHealthy(addr(srv)) }, 2*time.Second,
		"实例探活成功后应为 HEALTHY")
	t.Logf("[test] 主动探测: 实例连续 2 次 /healthz 200 -> HEALTHY，进入健康池")

	healthy.Store(false)
	waitUntil(t, func() bool { return !c.IsHealthy(addr(srv)) }, 2*time.Second,
		"连续探测失败达到阈值后应被摘除")
	t.Logf("[test] 主动探测: /healthz 连续 3 次 503 -> UNHEALTHY，自动摘除 判定依据: unhealthy_threshold=3")

	healthy.Store(true)
	waitUntil(t, func() bool { return c.IsHealthy(addr(srv)) }, 2*time.Second,
		"实例恢复后应自动重新接入")
	t.Logf("[test] 主动探测: 恢复后连续 2 次 200 -> HEALTHY，自动恢复接入 判定依据: healthy_threshold=2")
}

// TestCheckerDeadConnection 验证实例直接关监听（连接被拒）也会被摘除，
// 不会一直留在池子里。
func TestCheckerDeadConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	a := addr(srv)

	c := NewChecker([]string{a})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Start(ctx, 30*time.Millisecond, 200*time.Millisecond, "/healthz", 1, 1)

	waitUntil(t, func() bool { return c.IsHealthy(a) }, 2*time.Second, "应先探活成功")
	srv.Close()
	waitUntil(t, func() bool { return !c.IsHealthy(a) }, 2*time.Second,
		"连接被拒/超时达到阈值后应摘除")
	if len(c.HealthyInstances()) != 0 {
		t.Fatalf("已挂实例不应留在健康池中，实际 %v", c.HealthyInstances())
	}
	t.Logf("[test] 主动探测: 实例关监听导致连接失败 -> UNHEALTHY，不留池 判定依据: 网络错误同样计入连续失败")
}

func waitUntil(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}
