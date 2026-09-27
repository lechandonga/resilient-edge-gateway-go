package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Duration 包装 time.Duration，使其支持 JSON 字符串反序列化（如 "500ms"）。
// 若配置成纯数字，则按毫秒解释（如 500 == 500ms）。
type Duration struct {
	time.Duration
}

// UnmarshalJSON 支持 "1s" 这种字符串，也支持 500 这种数字（毫秒）。
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		d.Duration = parsed
		return nil
	}

	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		d.Duration = time.Duration(n) * time.Millisecond
		return nil
	}

	return fmt.Errorf("duration must be a string like \"500ms\" or a number in milliseconds")
}

// MarshalJSON 以字符串形式输出，方便回读。
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// RouteConfig 单条路由规则：按路径前缀匹配到一个后端服务。
type RouteConfig struct {
	Prefix  string `json:"prefix"`
	Service string `json:"service"`
}

// HealthCheckConfig 后端健康探测配置。
//
// 摘除/恢复采用"连续 N 次"判定，避免单次抖动直接摘实例，
// 也避免已挂实例长期留在池中。
type HealthCheckConfig struct {
	Path               string   `json:"path"`
	Interval           Duration `json:"interval"`
	Timeout            Duration `json:"timeout"`
	HealthyThreshold   int      `json:"healthy_threshold"`
	UnhealthyThreshold int      `json:"unhealthy_threshold"`
}

// RateLimitConfig 单服务令牌桶限流配置（每秒补充令牌数 + 桶容量）。
type RateLimitConfig struct {
	Rate  float64 `json:"rate"`
	Burst int     `json:"burst"`
}

// ServiceConfig 一个后端服务及其多个实例地址。
type ServiceConfig struct {
	Name      string           `json:"name"`
	Instances []string         `json:"instances"`
	RateLimit *RateLimitConfig `json:"rate_limit,omitempty"`
}

// Config 网关整体配置。
type Config struct {
	Listen         string            `json:"listen"`
	ForwardTimeout Duration          `json:"forward_timeout"`
	Routes         []RouteConfig     `json:"routes"`
	Services       []ServiceConfig   `json:"services"`
	HealthCheck    HealthCheckConfig `json:"health_check"`
}

// defaults 在配置缺省时填入可用的默认值。
func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.ForwardTimeout.Duration == 0 {
		c.ForwardTimeout.Duration = 3 * time.Second
	}
	if c.HealthCheck.Path == "" {
		c.HealthCheck.Path = "/healthz"
	}
	if c.HealthCheck.Interval.Duration == 0 {
		c.HealthCheck.Interval.Duration = 2 * time.Second
	}
	if c.HealthCheck.Timeout.Duration == 0 {
		c.HealthCheck.Timeout.Duration = 500 * time.Millisecond
	}
	if c.HealthCheck.HealthyThreshold == 0 {
		c.HealthCheck.HealthyThreshold = 2
	}
	if c.HealthCheck.UnhealthyThreshold == 0 {
		c.HealthCheck.UnhealthyThreshold = 3
	}
}

// validate 检查配置自洽性，避免启动后才发现问题。
func (c *Config) validate() error {
	if len(c.Services) == 0 {
		return fmt.Errorf("config: at least one service is required")
	}
	services := make(map[string]int, len(c.Services))
	for i, svc := range c.Services {
		if svc.Name == "" {
			return fmt.Errorf("config: services[%d] has empty name", i)
		}
		if _, dup := services[svc.Name]; dup {
			return fmt.Errorf("config: duplicate service name %q", svc.Name)
		}
		services[svc.Name] = i
		if len(svc.Instances) == 0 {
			return fmt.Errorf("config: service %q has no instances", svc.Name)
		}
		seen := map[string]bool{}
		for j, addr := range svc.Instances {
			if addr == "" {
				return fmt.Errorf("config: service %q instances[%d] is empty", svc.Name, j)
			}
			if seen[addr] {
				return fmt.Errorf("config: service %q has duplicate instance %q", svc.Name, addr)
			}
			seen[addr] = true
		}
		if rl := svc.RateLimit; rl != nil {
			if rl.Rate <= 0 || rl.Burst <= 0 {
				return fmt.Errorf("config: service %q rate_limit requires rate > 0 and burst > 0", svc.Name)
			}
		}
	}

	if len(c.Routes) == 0 {
		return fmt.Errorf("config: at least one route is required")
	}
	prefixes := map[string]bool{}
	for i, r := range c.Routes {
		if r.Prefix == "" || r.Prefix[0] != '/' {
			return fmt.Errorf("config: routes[%d].prefix must start with '/'", i)
		}
		if prefixes[r.Prefix] {
			return fmt.Errorf("config: duplicate route prefix %q", r.Prefix)
		}
		prefixes[r.Prefix] = true
		if _, ok := services[r.Service]; !ok {
			return fmt.Errorf("config: route %q references unknown service %q", r.Prefix, r.Service)
		}
	}

	if c.HealthCheck.Interval.Duration <= 0 || c.HealthCheck.Timeout.Duration <= 0 {
		return fmt.Errorf("config: health_check interval/timeout must be positive")
	}
	if c.HealthCheck.HealthyThreshold <= 0 || c.HealthCheck.UnhealthyThreshold <= 0 {
		return fmt.Errorf("config: health_check thresholds must be positive")
	}
	return nil
}

// Load 从 JSON 文件加载配置，补默认值并校验。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	c := &Config{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}
