package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/lechandonga/resilient-edge-gateway-go/internal/backend"
	"github.com/lechandonga/resilient-edge-gateway-go/internal/config"
)

// Logger is the minimal logging surface (compatible with the standard
// log package) used to explain probe decisions.
type Logger interface {
	Printf(format string, args ...any)
}

// noopLogger is used when none is supplied.
type noopLogger struct{}

func (noopLogger) Printf(string, ...any) {}

// Checker actively probes every instance in a pool on a fixed interval.
// Consecutive failures eject an instance and consecutive successes
// re-admit it; the streak bookkeeping lives in backend.Pool so passive
// and active feedback share one threshold implementation.
type Checker struct {
	pool   *backend.Pool
	client *http.Client
	cfg    config.HCConfig
	log    Logger

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// NewChecker builds a checker for a pool.
func NewChecker(pool *backend.Pool, client *http.Client, cfg config.HCConfig, log Logger) *Checker {
	if log == nil {
		log = noopLogger{}
	}
	return &Checker{pool: pool, client: client, cfg: cfg, log: log, done: make(chan struct{})}
}

// Start launches the background probing loop.
func (c *Checker) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go c.run(ctx)
}

// Stop halts the probing loop and waits for it to exit.
func (c *Checker) Stop() {
	c.once.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
	})
	<-c.done
}

// run is the ticker loop.
func (c *Checker) run(ctx context.Context) {
	defer close(c.done)
	interval := time.Duration(c.cfg.IntervalMS) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, inst := range c.pool.All() {
				if ctx.Err() != nil {
					return
				}
				c.probeOnce(ctx, inst)
			}
		}
	}
}

// probeOnce performs one probe of an instance and feeds the outcome to
// the pool, logging the evidence behind any state transition.
func (c *Checker) probeOnce(parent context.Context, inst *backend.Instance) bool {
	timeout := time.Duration(c.cfg.TimeoutMS) * time.Millisecond
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	url := "http://" + inst.Address + c.cfg.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.feed(inst, false, "build request failed: %v", err)
		return false
	}

	resp, err := c.client.Do(req)
	if err != nil {
		reason := "error"
		if ctx.Err() == context.DeadlineExceeded {
			reason = "timeout"
		}
		c.feed(inst, false, "probe %s %s after %s: %v", c.cfg.Path, reason, timeout, err)
		return false
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		c.feed(inst, false, "probe %s returned status %d", c.cfg.Path, resp.StatusCode)
		return false
	}
	c.feed(inst, true, "probe %s returned status %d", c.cfg.Path, resp.StatusCode)
	return true
}

// feed applies the outcome and logs the streak / threshold evidence.
func (c *Checker) feed(inst *backend.Instance, ok bool, detail string, args ...any) {
	res := c.pool.ProbeResult(inst, ok)
	verdict := "no state change"
	if res.Changed {
		verdict = "STATE CHANGE " + res.From.String() + "->" + res.To.String()
	}
	c.log.Printf("[health] route=%s instance=%s ok=%t streak=%d threshold=%d %s | %s",
		c.pool.RouteName, inst.Address, ok, res.Streak, res.Threshold, verdict,
		sprintf(detail, args...))
}

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
