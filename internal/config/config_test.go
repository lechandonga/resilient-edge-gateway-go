package config

import (
	"errors"
	"testing"
)

func TestParse_AppliesDefaultsAndNormalizes(t *testing.T) {
	data := []byte(`{
		"routes": [{
			"name": "api",
			"prefix": "/api",
			"backends": ["http://127.0.0.1:9001/", "127.0.0.1:9002"],
			"rate_limit": {"requests_per_second": 5}
		}]
	}`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Fatalf("default listen = %q", cfg.Listen)
	}
	r := cfg.Routes[0]
	if r.Backends[0] != "127.0.0.1:9001" || r.Backends[1] != "127.0.0.1:9002" {
		t.Fatalf("backend normalization wrong: %v", r.Backends)
	}
	if r.TimeoutMS != 3000 {
		t.Fatalf("default timeout = %d", r.TimeoutMS)
	}
	if r.RateLimit.Burst != 6 {
		t.Fatalf("default burst should be rps+1=6, got %d", r.RateLimit.Burst)
	}
	if r.HealthCheck.FailureThreshold != 3 || r.HealthCheck.SuccessThreshold != 2 {
		t.Fatalf("default HC thresholds wrong: %+v", r.HealthCheck)
	}
	t.Logf("parsed route backends=%v timeout=%dms burst=%d hc=%+v",
		r.Backends, r.TimeoutMS, r.RateLimit.Burst, r.HealthCheck)
}

func TestParse_RejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"no routes":         `{"routes": []}`,
		"bad prefix":        `{"routes":[{"name":"a","prefix":"api","backends":["h:1"]}]}`,
		"no backends":       `{"routes":[{"name":"a","prefix":"/a","backends":[]}]}`,
		"malformed backend": `{"routes":[{"name":"a","prefix":"/a","backends":["no-port"]}]}`,
		"duplicate backend": `{"routes":[{"name":"a","prefix":"/a","backends":["h:1","h:1"]}]}`,
		"duplicate prefix":  `{"routes":[{"name":"a","prefix":"/a","backends":["h:1"]},{"name":"b","prefix":"/a","backends":["h:2"]}]}`,
	}
	for name, jsonData := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(jsonData))
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("expected ErrInvalidConfig, got %v", err)
			}
			t.Logf("rejected as expected: %v", err)
		})
	}
}
