package gateway

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/lechandonga/resilient-edge-gateway-go/internal/balancer"
	"github.com/lechandonga/resilient-edge-gateway-go/internal/config"
	"github.com/lechandonga/resilient-edge-gateway-go/internal/health"
	"github.com/lechandonga/resilient-edge-gateway-go/internal/ratelimit"
)

// 对外返回的错误类型标记，便于客户端区分"限流 / 超时 / 无可用后端"。
const (
	headerError = "X-Gateway-Error"

	errRateLimited = "rate_limited"         // 429：触发限流
	errTimeout     = "upstream_timeout"     // 504：后端超时
	errNoUpstream  = "no_healthy_upstream"  // 503：没有健康实例/重试全部失败
	errUpstream    = "upstream_unreachable" // 503：连接后端失败
)

// Gateway 持有路由表、服务级的健康检查器 / 负载均衡器 / 限流器。
type Gateway struct {
	cfg *config.Config

	routes    []config.RouteConfig
	checkers  map[string]*health.Checker
	balancers map[string]balancer.Balancer
	limiters  map[string]*ratelimit.TokenBucket

	client      *http.Client
	probeCtx    context.Context
	probeCancel context.CancelFunc
}

// New 根据配置构造网关（内部完成组装，不启动探测）。
func New(cfg *config.Config) (*Gateway, error) {
	g := &Gateway{
		cfg:       cfg,
		routes:    append([]config.RouteConfig(nil), cfg.Routes...),
		checkers:  make(map[string]*health.Checker),
		balancers: make(map[string]balancer.Balancer),
		limiters:  make(map[string]*ratelimit.TokenBucket),
	}

	for _, svc := range cfg.Services {
		g.checkers[svc.Name] = health.NewChecker(svc.Instances)
		g.balancers[svc.Name] = balancer.NewRoundRobin()
		if svc.RateLimit != nil {
			g.limiters[svc.Name] = ratelimit.NewTokenBucket(svc.RateLimit.Rate, svc.RateLimit.Burst)
		}
	}

	// 转发客户端：限制连接堆积，避免慢后端把网关卡死或堆积大量空闲连接。
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
	}
	g.client = &http.Client{
		Transport: transport,
		// 不使用 client.Timeout，而是按请求设置 context 超时，
		// 这样能把"后端超时"与其他网络错误区分开。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return g, nil
}

// StartHealthChecks 启动所有服务的后台健康探测。
func (g *Gateway) StartHealthChecks() {
	g.probeCtx, g.probeCancel = context.WithCancel(context.Background())
	hc := g.cfg.HealthCheck
	for name, checker := range g.checkers {
		go checker.Start(
			g.probeCtx,
			hc.Interval.Duration,
			hc.Timeout.Duration,
			hc.Path,
			hc.HealthyThreshold,
			hc.UnhealthyThreshold,
		)
		log.Printf("[gateway] health checker started for service %q interval=%s timeout=%s thresholds(healthy=%d unhealthy=%d)",
			name, hc.Interval.Duration, hc.Timeout.Duration, hc.HealthyThreshold, hc.UnhealthyThreshold)
	}
}

// StopHealthChecks 停止后台健康探测。
func (g *Gateway) StopHealthChecks() {
	if g.probeCancel != nil {
		g.probeCancel()
	}
}

// matchRoute 按最长前缀匹配路由；匹配不到返回空串。
func (g *Gateway) matchRoute(path string) string {
	bestLen := -1
	bestSvc := ""
	for _, r := range g.routes {
		if strings.HasPrefix(path, r.Prefix) && len(r.Prefix) > bestLen {
			bestLen = len(r.Prefix)
			bestSvc = r.Service
		}
	}
	return bestSvc
}

// Handler 返回 http.Handler，供测试与 main 挂载。
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc := g.matchRoute(r.URL.Path)
		if svc == "" {
			log.Printf("[gateway] %s %s -> 404 no route matched", r.Method, r.URL.Path)
			http.Error(w, "no route matched", http.StatusNotFound)
			return
		}

		// 限流：令牌桶取不到令牌直接 429，不建立到后端的连接。
		if limiter, ok := g.limiters[svc]; ok {
			if !limiter.Allow() {
				log.Printf("[gateway] %s %s service=%s -> 429 rate limited（令牌桶无令牌）",
					r.Method, r.URL.Path, svc)
				w.Header().Set(headerError, errRateLimited)
				w.Header().Set("Retry-After", "1")
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
		}

		g.forwardWithFailover(w, r, svc)
	})
}

// forwardWithFailover 选取健康实例转发；连接失败/超时会换实例重试，
// 最多尝试当前所有健康实例。后端返回的 HTTP 状态码原样透传（含 5xx）。
func (g *Gateway) forwardWithFailover(w http.ResponseWriter, r *http.Request, svc string) {
	checker := g.checkers[svc]
	lb := g.balancers[svc]

	tried := make(map[string]bool)
	lastErrKind := errNoUpstream

	for attempt := 0; ; attempt++ {
		var fresh []string
		for _, addr := range checker.HealthyInstances() {
			if !tried[addr] {
				fresh = append(fresh, addr)
			}
		}

		if len(fresh) == 0 {
			kind := lastErrKind
			status := http.StatusServiceUnavailable
			if kind == errTimeout {
				status = http.StatusGatewayTimeout
			}
			if attempt == 0 {
				log.Printf("[gateway] %s %s service=%s -> %d no healthy instance（健康池为空）",
					r.Method, r.URL.Path, svc, status)
			} else {
				log.Printf("[gateway] %s %s service=%s -> %d all attempts failed, last=%s",
					r.Method, r.URL.Path, svc, status, kind)
			}
			w.Header().Set(headerError, kind)
			http.Error(w, http.StatusText(status), status)
			return
		}

		target, err := lb.Pick(fresh)
		if err != nil {
			w.Header().Set(headerError, errNoUpstream)
			http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
			return
		}
		tried[target] = true

		state, succStreak, failStreak := checker.Info(target)
		log.Printf("[gateway] %s %s -> service=%s instance=%s attempt=%d 判定依据: 实例状态=%s 连续成功=%d 连续失败=%d 本轮健康候选=%d",
			r.Method, r.URL.Path, svc, target, attempt+1, state, succStreak, failStreak, len(fresh))

		status, kind, ferr := g.forwardOnce(w, r, target)
		if ferr == nil {
			// 明确收到后端 HTTP 响应（含 5xx）即算转发成功。
			checker.RecordForwardResult(target, true)
			log.Printf("[gateway] hit backend %s status=%d", target, status)
			return
		}

		// 连接失败 / 超时：被动信号计入健康判定，换另一个健康实例重试。
		checker.RecordForwardResult(target, false)
		lastErrKind = kind
		log.Printf("[gateway] instance %s forward failed kind=%s err=%v，失败信号已记录，切换实例重试",
			target, kind, ferr)
	}
}

// forwardOnce 向单个实例发起一次转发。
// 返回 (HTTP状态码, 错误类别, 错误)；ferr 为 nil 表示已完整写回响应。
func (g *Gateway) forwardOnce(w http.ResponseWriter, r *http.Request, target string) (int, string, error) {
	ctx, cancel := context.WithTimeout(r.Context(), g.cfg.ForwardTimeout.Duration)
	defer cancel()

	forwardURL := "http://" + target + r.URL.Path
	if r.URL.RawQuery != "" {
		forwardURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, forwardURL, r.Body)
	if err != nil {
		return 0, errUpstream, err
	}
	copyHeaders(req.Header, r.Header)

	resp, err := g.client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || isTimeoutErr(err) {
			return 0, errTimeout, err
		}
		return 0, errUpstream, err
	}
	defer resp.Body.Close()

	return writeResponse(w, resp, target)
}

// writeResponse 把后端响应头、状态码、Body 透传给客户端。
func writeResponse(rw http.ResponseWriter, resp *http.Response, target string) (int, string, error) {
	if rw == nil {
		return resp.StatusCode, "", nil
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			rw.Header().Add(k, v)
		}
	}
	rw.Header().Set("X-Upstream-Instance", target)
	rw.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(rw, resp.Body); err != nil {
		return resp.StatusCode, errUpstream, err
	}
	return resp.StatusCode, "", nil
}

// copyHeaders 复制请求头，跳过逐跳（hop-by-hop）头与连接管理头。
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding",
			"Upgrade", "Te", "Trailer":
			continue
		default:
			for _, v := range vs {
				dst.Add(k, v)
			}
		}
	}
}

// isTimeoutErr 判断是否为网络层超时错误。
func isTimeoutErr(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}
