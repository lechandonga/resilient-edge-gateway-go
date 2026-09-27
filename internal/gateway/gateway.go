package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/lechandonga/resilient-edge-gateway-go/internal/backend"
	"github.com/lechandonga/resilient-edge-gateway-go/internal/balancer"
	"github.com/lechandonga/resilient-edge-gateway-go/internal/config"
	"github.com/lechandonga/resilient-edge-gateway-go/internal/health"
	"github.com/lechandonga/resilient-edge-gateway-go/internal/proxy"
	"github.com/lechandonga/resilient-edge-gateway-go/internal/ratelimit"
)

// Logger is the minimal logging surface used by the gateway so test
// output can show which backend was hit and why decisions were made.
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// route wires together everything that belongs to one RouteConfig.
type route struct {
	cfg      config.RouteConfig
	pool     *backend.Pool
	balancer *balancer.RoundRobin
	limiter  *ratelimit.Limiter
	checker  *health.Checker
	timeout  time.Duration
}

// Gateway is the HTTP gateway.
type Gateway struct {
	routes []*route
	px     *proxy.Proxy
	log    Logger
}

// New builds a Gateway from validated configuration and starts the active
// health checkers for every route.
func New(cfg *config.Config, client *http.Client, log Logger) (*Gateway, error) {
	if log == nil {
		log = nopLogger{}
	}
	g := &Gateway{px: proxy.New(client), log: log}

	for _, rc := range cfg.Routes {
		pool := backend.NewPool(rc.Name, rc.Backends, rc.HealthCheck.FailureThreshold, rc.HealthCheck.SuccessThreshold)
		r := &route{
			cfg:      rc,
			pool:     pool,
			balancer: balancer.NewRoundRobin(),
			limiter:  ratelimit.New(rc.RateLimit.RequestsPerSecond, rc.RateLimit.Burst),
			timeout:  time.Duration(rc.TimeoutMS) * time.Millisecond,
		}
		r.checker = health.NewChecker(pool, client, rc.HealthCheck, log)
		r.checker.Start()
		g.routes = append(g.routes, r)
		g.log.Printf("[config] route=%s prefix=%q backends=%v timeout=%s rps=%.1f burst=%d hc=%s@%dms(fail=%d,ok=%d)",
			rc.Name, rc.Prefix, rc.Backends, r.timeout,
			rc.RateLimit.RequestsPerSecond, rc.RateLimit.Burst,
			rc.HealthCheck.Path, rc.HealthCheck.IntervalMS,
			rc.HealthCheck.FailureThreshold, rc.HealthCheck.SuccessThreshold)
	}
	return g, nil
}

// Handler returns the http.Handler that performs routing.
func (g *Gateway) Handler() http.Handler { return g }

// ServeHTTP implements http.Handler.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt := g.matchRoute(r.URL.Path)
	if rt == nil {
		writeError(w, http.StatusNotFound, "no_route",
			fmt.Sprintf("no route matches path %q", r.URL.Path))
		return
	}

	// Rate limiting happens before selecting an instance, so a rejected
	// request never reaches or ties up a backend connection.
	dec := rt.limiter.TryTake(time.Now())
	if !dec.Allowed {
		g.log.Printf("[ratelimit] route=%s REJECT path=%s remaining-tokens=%.2f rate=%.1f burst=%d",
			rt.cfg.Name, r.URL.Path, dec.Tokens, dec.Rate, dec.Burst)
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"request rejected by gateway rate limiter")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), rt.timeout)
	defer cancel()
	r = r.WithContext(ctx)

	g.forwardWithRetry(rt, w, r)
}

// forwardWithRetry forwards to a healthy instance, retrying once on a
// different healthy instance only when the backend is unreachable.
// Timeouts are not retried (the deadline is already spent), and 5xx
// responses are not retried (they are valid backend responses to relay).
func (g *Gateway) forwardWithRetry(rt *route, w http.ResponseWriter, r *http.Request) {
	for attempt := 0; attempt < 2; attempt++ {
		healthy := rt.pool.Healthy()
		inst := rt.balancer.Pick(healthy)
		if inst == nil {
			g.log.Printf("[route] route=%s no healthy instance (pool size=%d) -> 502",
				rt.cfg.Name, len(rt.pool.All()))
			writeError(w, http.StatusBadGateway, "no_healthy_backend",
				"no healthy backend instance available")
			return
		}

		g.log.Printf("[forward] route=%s attempt=%d picked=%s reason=round-robin over %d healthy",
			rt.cfg.Name, attempt+1, inst.Address, len(healthy))

		resp, err := g.px.Do(r.Context(), r, inst.Address)
		if err == nil {
			rt.pool.RecordResult(inst, true)
			w.Header().Set("X-Gateway-Backend", inst.Address)
			if werr := proxy.WriteResponse(w, resp); werr != nil {
				resp.Body.Close()
				g.log.Printf("[forward] route=%s backend=%s response relay failed: %v",
					rt.cfg.Name, inst.Address, werr)
				return
			}
			resp.Body.Close()
			g.log.Printf("[forward] route=%s backend=%s responded status=%d -> success",
				rt.cfg.Name, inst.Address, resp.StatusCode)
			return
		}

		perr := err.(*proxy.Error)
		g.applyFailure(rt, inst, perr)

		switch perr.Kind {
		case proxy.KindTimeout:
			g.log.Printf("[forward] route=%s backend=%s TIMEOUT after %s -> 504",
				rt.cfg.Name, inst.Address, rt.timeout)
			writeError(w, http.StatusGatewayTimeout, "upstream_timeout",
				fmt.Sprintf("backend %s did not respond within %s", inst.Address, rt.timeout))
			return
		case proxy.KindUnreachable:
			// Retry once on a *different* still-healthy instance.
			if attempt == 0 {
				if other := g.otherHealthy(rt, inst); other != nil {
					g.log.Printf("[forward] route=%s backend=%s unreachable, retrying on %s",
						rt.cfg.Name, inst.Address, other.Address)
					continue
				}
			}
			writeError(w, http.StatusBadGateway, "upstream_unreachable",
				fmt.Sprintf("backend %s is unreachable", inst.Address))
			return
		default:
			writeError(w, http.StatusBadGateway, "bad_gateway_response", perr.Error())
			return
		}
	}
}

// applyFailure feeds a failed forwarding attempt back into the pool and
// logs the ejection evidence.
func (g *Gateway) applyFailure(rt *route, inst *backend.Instance, perr *proxy.Error) {
	res := rt.pool.RecordResult(inst, false)
	if res.Changed {
		g.log.Printf("[health-passive] route=%s instance=%s EJECTED reason=%s failures=%d threshold=%d",
			rt.cfg.Name, inst.Address, perr.Kind, res.Streak, res.Threshold)
	} else {
		g.log.Printf("[health-passive] route=%s instance=%s failure reason=%s streak=%d/%d (kept in pool)",
			rt.cfg.Name, inst.Address, perr.Kind, res.Streak, res.Threshold)
	}
}

// otherHealthy returns a healthy instance different from failed, or nil.
func (g *Gateway) otherHealthy(rt *route, failed *backend.Instance) *backend.Instance {
	for _, inst := range rt.pool.Healthy() {
		if inst.Address != failed.Address {
			return inst
		}
	}
	return nil
}

// matchRoute returns the route with the longest matching prefix.
func (g *Gateway) matchRoute(path string) *route {
	var best *route
	bestLen := -1
	for _, rt := range g.routes {
		if path == rt.cfg.Prefix || (len(rt.cfg.Prefix) > 1 && startsWithPrefix(path, rt.cfg.Prefix)) {
			if len(rt.cfg.Prefix) > bestLen {
				best = rt
				bestLen = len(rt.cfg.Prefix)
			}
		}
	}
	return best
}

func startsWithPrefix(path, prefix string) bool {
	return len(path) >= len(prefix) && path[:len(prefix)] == prefix
}

// Shutdown stops all background health checkers.
func (g *Gateway) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		for _, rt := range g.routes {
			rt.checker.Stop()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pool exposes a route's instance pool, mainly for tests and diagnostics.
func (g *Gateway) Pool(routeName string) *backend.Pool {
	for _, rt := range g.routes {
		if rt.cfg.Name == routeName {
			return rt.pool
		}
	}
	return nil
}

// errorBody is the JSON shape of every gateway-generated error.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeError emits a distinct, machine-readable JSON error.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Gateway-Error", code)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: code, Message: msg})
}
