package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lechandonga/resilient-edge-gateway-go/internal/config"
)

func testHTTPClient() *http.Client {
	tr := &http.Transport{
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       2 * time.Second,
		ResponseHeaderTimeout: 0,
	}
	return &http.Client{Transport: tr}
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// routeSpec describes one route in a gateway under test.
type routeSpec struct {
	name       string
	prefix     string
	backends   []string
	rps        float64
	burst      int
	timeoutMS  int
	intervalMS int
	failThr    int
	okThr      int
}

// buildConfig converts route specs into a validated config with small,
// test-friendly health-check and timeout values.
func buildConfig(routes ...routeSpec) *config.Config {
	cfg := &config.Config{Listen: "127.0.0.1:0"}
	for _, rs := range routes {
		hc := config.HCConfig{
			Path:             "/healthz",
			IntervalMS:       rs.intervalMS,
			TimeoutMS:        rs.intervalMS / 2,
			FailureThreshold: rs.failThr,
			SuccessThreshold: rs.okThr,
		}
		if hc.TimeoutMS < 50 {
			hc.TimeoutMS = 50
		}
		cfg.Routes = append(cfg.Routes, config.RouteConfig{
			Name:        rs.name,
			Prefix:      rs.prefix,
			Backends:    rs.backends,
			RateLimit:   config.RateLimitConfig{RequestsPerSecond: rs.rps, Burst: rs.burst},
			TimeoutMS:   rs.timeoutMS,
			HealthCheck: hc,
		})
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	return cfg
}

// newTestGateway constructs a gateway without starting a network listener.
func newTestGateway(t *testing.T, cfg *config.Config) *Gateway {
	t.Helper()
	client := testHTTPClient()
	gw, err := New(cfg, client, testLogger{t})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := contextWithTimeout(time.Second)
		defer cancel()
		_ = gw.Shutdown(ctx)
	})
	return gw
}

// doRequest runs one request against the gateway and returns the response.
func doRequest(gw *Gateway, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	return rec
}
