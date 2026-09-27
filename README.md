# resilient-edge-gateway-go

一个只用 Go 标准库实现的轻量 API 网关，可在本地直接跑起来。它把进入的 HTTP
请求按 **路径前缀** 分发到多个后端实例，并在后端不稳定时保持行为可预期：

- **路由 + 负载均衡**：按最长前缀匹配路由，同服务多实例用无锁轮询（round-robin）
  均摊流量，不会一直打同一个实例。
- **健康探测与自动摘除 / 恢复**：
  - 被动健康：转发时连接失败 / 超时计入“连续失败”，达到 `failure_threshold`
    才摘除，因此**单次抖动不会误摘**；一次成功立即把失败计数清零。
  - 主动健康：后台按固定间隔探测每个实例的健康检查路径，摘除后的实例在
    **连续** `success_threshold` 次探测成功后自动重新接入；探测失败会中断
    连续成功计数，因此**已经挂掉的实例不会赖在池子里**。
- **限流与超时**：
  - 每路由一个令牌桶（`requests_per_second` / `burst`），超限立即返回
    **429 `rate_limited`**，请求根本不会去占用后端连接。
  - 每路由独立超时，超时立即返回 **504 `upstream_timeout`**，用的是每请求
    context 截止时间，到期即放弃上游，连接不会堆积。
  - 后端连不上返回 **502 `upstream_unreachable`**，并在还有其他健康实例时
    **换一个实例重试一次**；所有实例都不健康时快速返回 502 `no_healthy_backend`。
- 三类失败（限流 / 超时 / 不可达）有不同的 HTTP 状态码和
  `X-Gateway-Error` 响应头，错误体是统一的 JSON：
  `{"error":"<code>","message":"<详情>"}`。

## 目录结构

```
cmd/gateway/                 网关入口（加载配置、启动、优雅退出）
cmd/mockbackend/             手工验证用的模拟后端（支持 ?delay= 模拟慢响应）
internal/config/             JSON 配置解析、默认值、校验、后端地址归一化
internal/backend/            实例池：健康状态 + 连续成败计数（摘除/恢复阈值）
internal/balancer/           原子计数器实现的轮询选择器
internal/ratelimit/          惰性补充的令牌桶（无后台 goroutine）
internal/health/             主动健康探测器（定时 GET 健康检查路径）
internal/proxy/              请求转发、hop-by-hop 头清理、错误分类
internal/gateway/            组装：路由匹配 → 限流 → 选实例 → 超时转发/重试
configs/config.json          示例配置
```

## 构建与测试

```bash
go build ./...
go test ./... -count=1 -v        # -v 可看到每次命中的后端与判定依据
go vet ./...
```

运行网关：

```bash
go run ./cmd/gateway -config configs/config.json
```

## 配置说明

配置是一个 JSON 文件（见 `configs/config.json`）：

```jsonc
{
  "listen": ":8080",
  "defaults": {                       // 路由缺省时使用的默认值
    "timeout_ms": 3000,
    "rate_limit": { "requests_per_second": 0, "burst": 0 },   // 0 = 不限流
    "health_check": {
      "path": "/healthz",
      "interval_ms": 2000,            // 探测间隔
      "timeout_ms": 500,             // 单次探测超时（必须 < interval）
      "failure_threshold": 3,        // 连续失败多少次摘除
      "success_threshold": 2         // 摘除后连续成功多少次恢复
    }
  },
  "routes": [
    {
      "name": "users",
      "prefix": "/users",            // 路径前缀；多条命中时取最长前缀
      "backends": ["127.0.0.1:9001", "127.0.0.1:9002"], // host:port，可带 http://
      "timeout_ms": 800,
      "rate_limit": { "requests_per_second": 5, "burst": 5 },
      "health_check": { "path": "/healthz", "interval_ms": 500,
                        "timeout_ms": 200,
                        "failure_threshold": 3, "success_threshold": 2 }
    }
  ]
}
```

要点：

- **路由规则**：`prefix` 必须以 `/` 开头且不能重复；`backends` 至少一个且
  不能重复，支持 `host:port` 或 `http(s)://host:port[/...]`（会被归一化为
  `host:port`）。请求路径与某前缀前缀匹配即路由到该服务；多个匹配选最长前缀。
- **健康探测**：`failure_threshold` 控制抗抖动（建议 3），`success_threshold`
  控制恢复的谨慎程度（建议 2）。探测超时必须小于探测间隔。被动（真实请求）
  与主动（定时探测）两路反馈共用同一份连续计数逻辑，判定口径一致。
- **限流**：`requests_per_second` 为每秒补充的令牌数，`burst` 为桶容量
  （突发上限）；桶初始是满的。设为 0 表示该路由不限流。
- **超时**：`timeout_ms` 是网关等待后端的上限，到点即放弃。

校验规则（`internal/config`）覆盖：无路由、非法前缀、空/重复后端、
`burst<1`、探测超时 ≥ 间隔、阈值 < 1 等，启动即报错而不是带病运行。

## 手工验证方法

用仓库自带的模拟后端：

```bash
# 终端 1/2/3：起后端
go run ./cmd/mockbackend -port 9001 -name users-1
go run ./cmd/mockbackend -port 9002 -name users-2
go run ./cmd/mockbackend -port 9101 -name orders-1
# 终端 4：起网关
go run ./cmd/gateway -config configs/config.json
```

1. **负载均衡**：连打 4 次，`X-Gateway-Backend` 在 9001/9002 间交替。
   ```bash
   for i in 1 2 3 4; do
     curl -s -D - -o /dev/null http://127.0.0.1:8080/users/$i | grep -i X-Gateway-Backend
   done
   ```
2. **宕机摘除与恢复**：`kill` 掉 9001，继续请求会看到流量全部落在 9002
   （前几个请求里网关会在 9001 不可达时自动重试到 9002），网关日志出现
   `EJECTED` / 主动探测 `STATE CHANGE healthy->unhealthy`；重新启动 9001，
   约 `interval_ms × success_threshold` 后日志出现
   `STATE CHANGE unhealthy->healthy`，流量重新均摊。
3. **限流**：orders 路由 `rps=2 burst=2`，连发 5 个：前 2 个 200，后 3 个
   429 且 `X-Gateway-Error: rate_limited`。
   ```bash
   for i in 1 2 3 4 5; do
     curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8080/orders/$i
   done
   ```
4. **超时**：users 路由超时 800ms，让后端睡 2000ms，约 0.8s 返回
   504 且 `X-Gateway-Error: upstream_timeout`。
   ```bash
   curl -i "http://127.0.0.1:8080/users/slow?delay=2000"
   ```
5. **无匹配路由**：`curl -i http://127.0.0.1:8080/nope` → 404
   `X-Gateway-Error: no_route`。

## 行为对照表

| 场景 | HTTP 状态 | `X-Gateway-Error` | 说明 |
|---|---|---|---|
| 正常转发 | 后端原样状态（含 5xx） | 无；成功有 `X-Gateway-Backend` | 5xx 也视为“后端可达”，照原样透传 |
| 超过限流 | 429 | `rate_limited` | 在选实例之前拒绝，不碰后端 |
| 后端超时 | 504 | `upstream_timeout` | 到点即放弃，不重试 |
| 后端连不上 | 502 | `upstream_unreachable` | 换另一个健康实例重试一次 |
| 无健康实例 | 502 | `no_healthy_backend` | 快速失败，不再尝试连接 |
| 无匹配路由 | 404 | `no_route` | 最长前缀匹配失败 |

## 设计取舍

- 只用标准库，无第三方依赖，便于离线构建与代码审查。
- 轮询用原子计数器，请求路径上不加互斥锁；实例池用 RWMutex 保护健康状态
  与计数，读写都很快。
- 令牌桶惰性补充，没有后台定时器，不泄漏 goroutine。
- 健康状态用原子量读取、判定写入集中在 `backend.Pool` 一处，被动/主动反馈
  不会互相竞争出不一致结论。
