package gateway

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lechandonga/resilient-edge-gateway-go/internal/config"
)

// fakeBackend 用 httptest 模拟一个后端实例，可动态切换健康状态与延迟。
type fakeBackend struct {
	server *httptest.Server
	addr   string

	mu      sync.Mutex
	healthy bool
	delay   time.Duration
	hits    int64
	name    string
}

func newFakeBackend(name string, healthy bool, delay time.Duration) *fakeBackend {
	b := &fakeBackend{healthy: healthy, delay: delay, name: name}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&b.hits, 1)

		b.mu.Lock()
		healthy, delay := b.healthy, b.delay
		b.mu.Unlock()

		if r.URL.Path == "/healthz" {
			if healthy {
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintln(w, "ok")
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintln(w, "unavailable")
			}
			return
		}

		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("X-Backend-Name", b.name)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "hello from %s\n", b.name)
	}))
	b.addr = strings.TrimPrefix(b.server.URL, "http://")
	return b
}

func (b *fakeBackend) setHealthy(v bool) {
	b.mu.Lock()
	b.healthy = v
	b.mu.Unlock()
}

func (b *fakeBackend) setDelay(d time.Duration) {
	b.mu.Lock()
	b.delay = d
	b.mu.Unlock()
}

func (b *fakeBackend) hitCount() int64 { return atomic.LoadInt64(&b.hits) }

func (b *fakeBackend) close() { b.server.Close() }

// testConfig 构造一个网关配置：健康探测间隔很短，便于测试快速收敛。
func testConfig(backends []*fakeBackend, rate, burst float64, forwardTimeout time.Duration) *config.Config {
	addrs := make([]string, len(backends))
	for i, b := range backends {
		addrs[i] = b.addr
	}
	cfg := &config.Config{
		Listen:         ":0",
		ForwardTimeout: config.Duration{Duration: forwardTimeout},
		Routes:         []config.RouteConfig{{Prefix: "/api", Service: "svc"}},
		Services: []config.ServiceConfig{{
			Name:      "svc",
			Instances: addrs,
		}},
		HealthCheck: config.HealthCheckConfig{
			Path:               "/healthz",
			Interval:           config.Duration{Duration: 100 * time.Millisecond},
			Timeout:            config.Duration{Duration: 200 * time.Millisecond},
			HealthyThreshold:   2,
			UnhealthyThreshold: 3,
		},
	}
	if rate > 0 {
		cfg.Services[0].RateLimit = &config.RateLimitConfig{Rate: rate, Burst: int(burst)}
	}
	return cfg
}

func doRequest(t *testing.T, h http.Handler, path string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, rec.Header().Get(headerError), string(body)
}

// waitForHealthy 轮询直到健康实例数等于 want，超时则失败。
func waitForHealthy(t *testing.T, g *Gateway, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(g.checkers["svc"].HealthyInstances()) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	snap := g.checkers["svc"].Snapshot()
	t.Fatalf("等待 %d 个健康实例超时，当前快照: %v", want, snap)
}

// TestBackendDownAndRecover 验证：
//  1. 三个健康实例时，轮询能命中不同实例，不总打同一个；
//  2. 某个实例宕掉（健康探测连续失败达到阈值）后，被自动摘除，请求不再落到它身上；
//  3. 实例恢复（连续成功达到阈值）后，自动重新接入并再次被命中。
func TestBackendDownAndRecover(t *testing.T) {
	b1 := newFakeBackend("backend-1", true, 0)
	b2 := newFakeBackend("backend-2", true, 0)
	b3 := newFakeBackend("backend-3", true, 0)
	defer b1.close()
	defer b2.close()
	defer b3.close()

	cfg := testConfig([]*fakeBackend{b1, b2, b3}, 0, 0, 2*time.Second)
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	gw.StartHealthChecks()
	defer gw.StopHealthChecks()

	h := gw.Handler()
	waitForHealthy(t, gw, 3, 2*time.Second)

	// 阶段一：发 6 个请求，期望三个实例都被命中（轮询均衡）。
	hit := map[string]int{}
	for i := 0; i < 6; i++ {
		status, _, body := doRequest(t, h, "/api/test")
		if status != http.StatusOK {
			t.Fatalf("请求 %d 期望 200，实际 %d", i+1, status)
		}
		switch {
		case strings.Contains(body, "backend-1"):
			hit["backend-1"]++
		case strings.Contains(body, "backend-2"):
			hit["backend-2"]++
		case strings.Contains(body, "backend-3"):
			hit["backend-3"]++
		}
	}
	log.Printf("[test] 阶段一(全健康) 命中分布: %v (b1=%d b2=%d b3=%d) 判定依据: 三个实例均 HEALTHY，轮询计数器依次取模",
		hit, b1.hitCount(), b2.hitCount(), b3.hitCount())
	if len(hit) != 3 {
		t.Fatalf("轮询应命中全部 3 个实例，实际只命中: %v", hit)
	}

	// 阶段二：backend-2 宕机，等待被健康探测摘除。
	b2.setHealthy(false)
	waitForHealthy(t, gw, 2, 2*time.Second)
	log.Printf("[test] 阶段二 backend-2 已宕机并被摘除，健康快照: %v 判定依据: 连续失败达到 unhealthy_threshold=3",
		gw.checkers["svc"].Snapshot())

	// 再发请求，backend-2 不应再被命中（被动转发计数也可能影响，但这里它已不在健康池）。
	hitsBefore := b2.hitCount()
	for i := 0; i < 6; i++ {
		status, _, _ := doRequest(t, h, "/api/test")
		if status != http.StatusOK {
			t.Fatalf("摘除后请求 %d 期望 200，实际 %d", i+1, status)
		}
	}
	if b2.hitCount() != hitsBefore {
		t.Fatalf("backend-2 被摘除后不应再收到请求，命中数却从 %d 变为 %d", hitsBefore, b2.hitCount())
	}
	log.Printf("[test] 阶段二 摘除后 backend-2 命中数保持 %d，请求只落在存活实例上 判定依据: 健康池只含 HEALTHY 实例",
		b2.hitCount())

	// 阶段三：backend-2 恢复，等待自动重新接入。
	b2.setHealthy(true)
	waitForHealthy(t, gw, 3, 2*time.Second)
	log.Printf("[test] 阶段三 backend-2 恢复并重新接入，健康快照: %v 判定依据: 连续成功达到 healthy_threshold=2",
		gw.checkers["svc"].Snapshot())

	// 再发若干请求，backend-2 应重新被命中。
	hitsBefore = b2.hitCount()
	for i := 0; i < 9; i++ {
		if status, _, _ := doRequest(t, h, "/api/test"); status != http.StatusOK {
			t.Fatalf("恢复后请求 %d 期望 200，实际 %d", i+1, status)
		}
	}
	if b2.hitCount() <= hitsBefore {
		t.Fatalf("backend-2 恢复后应重新被命中，命中数未增加(before=%d after=%d)", hitsBefore, b2.hitCount())
	}
	log.Printf("[test] 阶段三 恢复后 backend-2 新增命中 %d 次 判定依据: 恢复后回到健康池参与轮询",
		b2.hitCount()-hitsBefore)
}

// TestRateLimit 验证：令牌桶初始有 burst 个令牌，突发请求超过 burst 的部分
// 应返回 429 且带 X-Gateway-Error: rate_limited；等待令牌补充后又可放行。
func TestRateLimit(t *testing.T) {
	b1 := newFakeBackend("backend-1", true, 0)
	defer b1.close()

	// 2 令牌/秒，桶容量 3。
	cfg := testConfig([]*fakeBackend{b1}, 2, 3, 2*time.Second)
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	gw.StartHealthChecks()
	defer gw.StopHealthChecks()
	h := gw.Handler()
	waitForHealthy(t, gw, 1, 2*time.Second)

	allowed, limited := 0, 0
	for i := 0; i < 8; i++ {
		status, kind, _ := doRequest(t, h, "/api/rl")
		switch status {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
			if kind != errRateLimited {
				t.Fatalf("429 响应期望 X-Gateway-Error=%s，实际 %s", errRateLimited, kind)
			}
			limited++
		default:
			t.Fatalf("请求 %d 期望 200/429，实际 %d", i+1, status)
		}
	}
	log.Printf("[test] 限流场景 突发 8 请求: 放行=%d 限流=%d 判定依据: 桶容量 burst=3，超出无令牌即 429",
		allowed, limited)
	if allowed != 3 {
		t.Fatalf("初始满桶应恰好放行 3 个，实际放行 %d", allowed)
	}
	if limited != 5 {
		t.Fatalf("其余 5 个应被限流，实际限流 %d", limited)
	}

	// 等待约 1 秒，按 rate=2/s 应补充约 2 个令牌。
	time.Sleep(1100 * time.Millisecond)
	status, kind, _ := doRequest(t, h, "/api/rl")
	log.Printf("[test] 限流后等待 1.1s 再请求 status=%d kind=%s 判定依据: 约补充 2 个令牌，应再次放行",
		status, kind)
	if status != http.StatusOK {
		t.Fatalf("令牌补充后应放行，实际 status=%d kind=%s", status, kind)
	}
}

// TestForwardTimeout 验证：后端处理时间超过网关 forward_timeout 时，
// 网关返回 504 且 X-Gateway-Error: upstream_timeout，不会一直挂住连接。
func TestForwardTimeout(t *testing.T) {
	// 慢后端：业务接口睡 1s，但 /healthz 仍然正常，所以它处于健康池。
	slow := newFakeBackend("slow", true, time.Second)
	defer slow.close()

	cfg := testConfig([]*fakeBackend{slow}, 0, 0, 200*time.Millisecond)
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	gw.StartHealthChecks()
	defer gw.StopHealthChecks()
	h := gw.Handler()
	waitForHealthy(t, gw, 1, 2*time.Second)

	start := time.Now()
	status, kind, _ := doRequest(t, h, "/api/slow")
	elapsed := time.Since(start)

	log.Printf("[test] 超时场景 后端业务耗时=1s 网关 forward_timeout=200ms，实际 status=%d kind=%s 耗时=%s 判定依据: context deadline exceeded 归类为 upstream_timeout",
		status, kind, elapsed)
	if status != http.StatusGatewayTimeout {
		t.Fatalf("超时期望 504，实际 %d", status)
	}
	if kind != errTimeout {
		t.Fatalf("期望 X-Gateway-Error=%s，实际 %s", errTimeout, kind)
	}
	if elapsed > 900*time.Millisecond {
		t.Fatalf("网关应在超时附近快速返回，实际耗时 %s", elapsed)
	}
}

// TestAllInstancesDown 验证：健康池为空时返回 503 no_healthy_upstream，
// 且后端恢复后网关自动恢复服务。
func TestAllInstancesDown(t *testing.T) {
	b1 := newFakeBackend("backend-1", true, 0)
	defer b1.close()

	cfg := testConfig([]*fakeBackend{b1}, 0, 0, time.Second)
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	gw.StartHealthChecks()
	defer gw.StopHealthChecks()
	h := gw.Handler()
	waitForHealthy(t, gw, 1, 2*time.Second)

	b1.setHealthy(false)
	waitForHealthy(t, gw, 0, 2*time.Second)

	status, kind, _ := doRequest(t, h, "/api/x")
	log.Printf("[test] 全部宕机场景 status=%d kind=%s 判定依据: 健康池为空，无实例可供负载均衡器选择", status, kind)
	if status != http.StatusServiceUnavailable || kind != errNoUpstream {
		t.Fatalf("期望 503/%s，实际 %d/%s", errNoUpstream, status, kind)
	}

	b1.setHealthy(true)
	waitForHealthy(t, gw, 1, 2*time.Second)
	if status, _, _ := doRequest(t, h, "/api/x"); status != http.StatusOK {
		t.Fatalf("后端恢复后期望 200，实际 %d", status)
	}
	log.Printf("[test] 全部宕机后恢复: 请求重新返回 200 判定依据: 实例重新 HEALTHY 并回到健康池")
}

// TestSingleFailureDoesNotEject 验证：偶发一次失败不会把健康实例摘掉，
// 必须连续失败达到阈值才摘除（扛抖动）。
func TestSingleFailureDoesNotEject(t *testing.T) {
	// 健康探测路径始终 200；这里通过 RecordForwardResult 注入 1 次偶发失败。
	b1 := newFakeBackend("backend-1", true, 0)
	defer b1.close()

	cfg := testConfig([]*fakeBackend{b1}, 0, 0, time.Second)
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	gw.StartHealthChecks()
	defer gw.StopHealthChecks()
	checker := gw.checkers["svc"]
	waitForHealthy(t, gw, 1, 2*time.Second)

	// 注入 1 次转发失败（小于 unhealthy_threshold=3）。
	checker.RecordForwardResult(b1.addr, false)
	state, succ, fail := checker.Info(b1.addr)
	log.Printf("[test] 偶发失败场景 注入 1 次失败后: state=%s 连续成功=%d 连续失败=%d 判定依据: 失败计数 %d < 摘除阈值 3",
		state, succ, fail, fail)
	if !checker.IsHealthy(b1.addr) {
		t.Fatalf("单次偶发失败不应摘除实例")
	}

	// 一次成功即清零失败计数。
	checker.RecordForwardResult(b1.addr, true)
	if !checker.IsHealthy(b1.addr) {
		t.Fatalf("一次成功后实例应仍保持健康")
	}
	log.Printf("[test] 偶发失败后一次成功，失败计数清零，实例保持 HEALTHY 判定依据: 任意成功重置连续失败计数")
}
