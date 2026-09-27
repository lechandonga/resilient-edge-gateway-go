package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the gateway configuration.
type Config struct {
	Listen   string        `json:"listen"`
	Routes   []RouteConfig `json:"routes"`
	Defaults Defaults      `json:"defaults"`
}

// RouteConfig describes how requests with a path prefix are handled.
type RouteConfig struct {
	Name        string          `json:"name"`
	Prefix      string          `json:"prefix"`
	Backends    []string        `json:"backends"`
	RateLimit   RateLimitConfig `json:"rate_limit"`
	TimeoutMS   int             `json:"timeout_ms"`
	HealthCheck HCConfig        `json:"health_check"`
}

// RateLimitConfig configures a per-route token bucket. Zero/negative
// RequestsPerSecond disables limiting for the route.
type RateLimitConfig struct {
	RequestsPerSecond float64 `json:"requests_per_second"`
	Burst             int     `json:"burst"`
}

// HCConfig configures active health probing for a route's instances.
type HCConfig struct {
	Path             string `json:"path"`
	IntervalMS       int    `json:"interval_ms"`
	TimeoutMS        int    `json:"timeout_ms"`
	FailureThreshold int    `json:"failure_threshold"`
	SuccessThreshold int    `json:"success_threshold"`
}

// Defaults holds fallback values used when a route omits settings.
type Defaults struct {
	TimeoutMS   int             `json:"timeout_ms"`
	RateLimit   RateLimitConfig `json:"rate_limit"`
	HealthCheck HCConfig        `json:"health_check"`
}

// ErrInvalidConfig is returned when configuration is invalid.
var ErrInvalidConfig = errors.New("invalid config")

// LoadFile reads and validates a JSON configuration file.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cfg, nil
}

// Parse parses JSON configuration bytes, applies defaults and validates.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaults fills unset per-route fields with defaults.
func (c *Config) ApplyDefaults() {
	d := c.Defaults
	if d.TimeoutMS <= 0 {
		d.TimeoutMS = 3000
	}
	if d.HealthCheck.Path == "" {
		d.HealthCheck.Path = "/healthz"
	}
	if d.HealthCheck.IntervalMS <= 0 {
		d.HealthCheck.IntervalMS = 2000
	}
	if d.HealthCheck.TimeoutMS <= 0 {
		d.HealthCheck.TimeoutMS = 500
	}
	if d.HealthCheck.FailureThreshold <= 0 {
		d.HealthCheck.FailureThreshold = 3
	}
	if d.HealthCheck.SuccessThreshold <= 0 {
		d.HealthCheck.SuccessThreshold = 2
	}

	for i := range c.Routes {
		r := &c.Routes[i]
		if r.TimeoutMS <= 0 {
			r.TimeoutMS = d.TimeoutMS
		}
		if r.RateLimit.RequestsPerSecond <= 0 {
			r.RateLimit = d.RateLimit
		} else if r.RateLimit.Burst <= 0 {
			r.RateLimit.Burst = int(r.RateLimit.RequestsPerSecond) + 1
		}
		if r.HealthCheck.Path == "" {
			r.HealthCheck.Path = d.HealthCheck.Path
		}
		if r.HealthCheck.IntervalMS <= 0 {
			r.HealthCheck.IntervalMS = d.HealthCheck.IntervalMS
		}
		if r.HealthCheck.TimeoutMS <= 0 {
			r.HealthCheck.TimeoutMS = d.HealthCheck.TimeoutMS
		}
		if r.HealthCheck.FailureThreshold <= 0 {
			r.HealthCheck.FailureThreshold = d.HealthCheck.FailureThreshold
		}
		if r.HealthCheck.SuccessThreshold <= 0 {
			r.HealthCheck.SuccessThreshold = d.HealthCheck.SuccessThreshold
		}
		for j, addr := range r.Backends {
			r.Backends[j] = normalizeAddr(addr)
		}
	}
	c.Defaults = d
}

// normalizeAddr accepts either "host:port" or "http(s)://host:port[/...]"
// and returns a bare host:port suitable for http.Request.URL.Host.
func normalizeAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		addr = addr[:i]
	}
	return addr
}

// Validate checks the configuration for consistency.
func (c *Config) Validate() error {
	if len(c.Routes) == 0 {
		return fmt.Errorf("%w: at least one route is required", ErrInvalidConfig)
	}
	seenPrefix := make(map[string]string)
	for _, r := range c.Routes {
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("%w: route is missing a name (prefix=%q)", ErrInvalidConfig, r.Prefix)
		}
		if !strings.HasPrefix(r.Prefix, "/") {
			return fmt.Errorf("%w: route %q prefix must start with '/'", ErrInvalidConfig, r.Name)
		}
		if other, ok := seenPrefix[r.Prefix]; ok {
			return fmt.Errorf("%w: prefix %q used by both %q and %q", ErrInvalidConfig, r.Prefix, other, r.Name)
		}
		seenPrefix[r.Prefix] = r.Name

		if len(r.Backends) == 0 {
			return fmt.Errorf("%w: route %q needs at least one backend", ErrInvalidConfig, r.Name)
		}
		seenBackend := make(map[string]struct{}, len(r.Backends))
		for _, b := range r.Backends {
			if b == "" || !strings.Contains(b, ":") {
				return fmt.Errorf("%w: route %q has malformed backend %q (want host:port)", ErrInvalidConfig, r.Name, b)
			}
			if _, dup := seenBackend[b]; dup {
				return fmt.Errorf("%w: route %q lists backend %q more than once", ErrInvalidConfig, r.Name, b)
			}
			seenBackend[b] = struct{}{}
		}
		if r.TimeoutMS <= 0 {
			return fmt.Errorf("%w: route %q timeout_ms must be positive", ErrInvalidConfig, r.Name)
		}
		if r.RateLimit.RequestsPerSecond > 0 && r.RateLimit.Burst < 1 {
			return fmt.Errorf("%w: route %q rate_limit.burst must be >= 1", ErrInvalidConfig, r.Name)
		}
		if r.HealthCheck.TimeoutMS <= 0 || r.HealthCheck.IntervalMS <= 0 {
			return fmt.Errorf("%w: route %q health check interval/timeout must be positive", ErrInvalidConfig, r.Name)
		}
		if r.HealthCheck.TimeoutMS >= r.HealthCheck.IntervalMS {
			return fmt.Errorf("%w: route %q health check timeout (%dms) must be smaller than interval (%dms)",
				ErrInvalidConfig, r.Name, r.HealthCheck.TimeoutMS, r.HealthCheck.IntervalMS)
		}
		if r.HealthCheck.FailureThreshold < 1 || r.HealthCheck.SuccessThreshold < 1 {
			return fmt.Errorf("%w: route %q health check thresholds must be >= 1", ErrInvalidConfig, r.Name)
		}
	}
	_ = time.Now
	return nil
}
