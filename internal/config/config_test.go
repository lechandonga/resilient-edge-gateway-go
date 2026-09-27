package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

// TestDurationUnmarshal 验证时长既支持 "500ms" 字符串，也支持数字（毫秒）。
func TestDurationUnmarshal(t *testing.T) {
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"250ms"`)); err != nil || d.Duration != 250*time.Millisecond {
		t.Fatalf("字符串时长解析错误: %v %v", d.Duration, err)
	}
	if err := d.UnmarshalJSON([]byte(`1500`)); err != nil || d.Duration != 1500*time.Millisecond {
		t.Fatalf("数字时长(毫秒)解析错误: %v %v", d.Duration, err)
	}
	if err := d.UnmarshalJSON([]byte(`"not-a-duration"`)); err == nil {
		t.Fatalf("非法时长应报错")
	}
}

// TestLoadDefaultsAndValidate 验证默认值补齐与非法配置被拒绝。
func TestLoadDefaultsAndValidate(t *testing.T) {
	valid := `{
	  "routes": [{"prefix": "/api", "service": "svc"}],
	  "services": [{"name": "svc", "instances": ["127.0.0.1:9001"]}]
	}`
	cfg, err := Load(writeTemp(t, valid))
	if err != nil {
		t.Fatalf("合法配置加载失败: %v", err)
	}
	if cfg.Listen != ":8080" || cfg.ForwardTimeout.Duration != 3*time.Second {
		t.Fatalf("默认值未正确补齐: listen=%s timeout=%s", cfg.Listen, cfg.ForwardTimeout)
	}
	if cfg.HealthCheck.UnhealthyThreshold != 3 || cfg.HealthCheck.HealthyThreshold != 2 {
		t.Fatalf("健康探测默认阈值不正确: %+v", cfg.HealthCheck)
	}
	t.Logf("[test] 默认值补齐: listen=%s forward_timeout=%s 探测阈值 healthy=%d unhealthy=%d",
		cfg.Listen, cfg.ForwardTimeout.Duration, cfg.HealthCheck.HealthyThreshold, cfg.HealthCheck.UnhealthyThreshold)

	badCases := map[string]string{
		"路由指向不存在服务": `{"routes":[{"prefix":"/x","service":"nope"}],"services":[{"name":"svc","instances":["a"]}]}`,
		"服务没有实例":    `{"routes":[{"prefix":"/x","service":"svc"}],"services":[{"name":"svc","instances":[]}]}`,
		"重复服务名":     `{"routes":[{"prefix":"/x","service":"s"}],"services":[{"name":"s","instances":["a"]},{"name":"s","instances":["b"]}]}`,
		"非法限流参数":    `{"routes":[{"prefix":"/x","service":"s"}],"services":[{"name":"s","instances":["a"],"rate_limit":{"rate":0,"burst":0}}]}`,
		"非法前缀":      `{"routes":[{"prefix":"x","service":"s"}],"services":[{"name":"s","instances":["a"]}]}`,
	}
	for name, body := range badCases {
		if _, err := Load(writeTemp(t, body)); err == nil {
			t.Fatalf("非法配置[%s]应被拒绝", name)
		} else {
			t.Logf("[test] 非法配置[%s]被正确拒绝: %v", name, err)
		}
	}
}
