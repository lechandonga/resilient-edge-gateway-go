# resilient-edge-gateway-go

一个零第三方依赖、可本地直接跑起来的轻量 Go API 网关。核心能力：

- **路由分发**：按请求路径前缀把请求路由到不同后端服务，同服务多实例间**轮询负载均衡**。
- **健康探测与自动摘除/恢复**：后台周期性探测 + 转发被动信号双驱动，基于"连续 N 次成功/失败"阈值判定，抗单次抖动，挂掉的实例不会赖在池子里，恢复后自动接回。
- **限流与超时**：每服务独立令牌桶限流；转发有整体超时；限流、超时、无后端三类错误有明确、可区分的响应。
- **连接保护**：每主机空闲连接数受限，请求级 context 超时，失败自动换实例重试，网关不会被慢后端拖垮或堆积连接。

## 目录结构

```
cmd/gateway/            程序入口（加载配置、启动探测与 HTTP 服务、优雅退出）
configs/config.json     示例配置
internal/config/        配置结构、JSON 加载、默认值与校验
internal/health/        实例状态机、周期健康探测、摘除/恢复阈值判定
internal/balancer/      轮询负载均衡（候选动态变化时也不偏斜）
internal/ratelimit/     惰性补充的令牌桶限流器（无后台 goroutine）
internal/gateway/       路由匹配、限流、转发、超时、故障转移、错误响应
```

## 快速开始

需要 Go 1.25+（标准库实现，无外部依赖）。

```bash
go build ./...
go run ./cmd/gateway -config configs/config.json
# [gateway] listening on :8080
```

网关自身不提供后端，可先用任意静态服务器模拟，例如在三个终端分别起后端：

```bash
for port in 19091 19092 19093; do
  (while true; do
     printf 'HTTP/1.1 200 OK\r\nContent-Length: 3\r\nX-Backend-Port: %s\r\n\r\nhi\n' "$port" | nc -l -p "$port" -q1 >/dev/null
   done) &
done
```

然后请求并多次执行，观察 `X-Upstream-Instance` 在三个实例间轮换：

```bash
curl -s -i http://127.0.0.1:8080/users/1 | grep -i 'X-Upstream-Instance'
# X-Upstream-Instance: 127.0.0.1:19091 / 19092 / 19093 依次轮换
```

## 配置说明（configs/config.json）

```json
{
  "listen": ":8080",
  "forward_timeout": "2s",
  "routes": [
    { "prefix": "/users",  "service": "user-service" },
    { "prefix": "/orders", "service": "order-service" }
  ],
  "services": [
    {
      "name": "user-service",
      "instances": ["127.0.0.1:19091", "127.0.0.1:19092", "127.0.0.1:19093"],
      "rate_limit": { "rate": 5, "burst": 5 }
    },
    {
      "name": "order-service",
      "instances": ["127.0.0.1:20091", "127.0.0.1:20092"]
    }
  ],
  "health_check": {
    "path": "/healthz",
    "interval": "1s",
    "timeout": "300ms",
    "healthy_threshold": 2,
    "unhealthy_threshold": 3
  }
}
```

时长字段既支持字符串（`"500ms"`、`"2s"`），也支持纯数字（按毫秒，`500` = 500ms）。

### 1. 路由规则

- `routes[].prefix` 按**路径前缀**匹配，`prefix` 必须以 `/` 开头；多条同时命中时取**最长前缀**。
- `routes[].service` 指向 `services[].name`；启动时会校验服务必须存在、前缀/服务名不可重复。
- 同服务多个 `instances` 时用轮询均衡。均衡器维护单调递增计数并对**当前健康实例数**取模，因此实例摘除/恢复导致候选集合变化时，流量仍均匀分布，不会总回到第一个实例。
- 请求头原样透传（剔除 `Connection`、`Keep-Alive`、`Transfer-Encoding` 等逐跳头），查询串保留，响应头与状态码（含后端 4xx/5xx）原样回传，并附加 `X-Upstream-Instance` 标识命中实例。

### 2. 健康探测与摘除/恢复

| 字段 | 含义 | 默认 |
| --- | --- | --- |
| `path` | 每个实例上的探测路径，GET 返回 < 400 视为成功 | `/healthz` |
| `interval` | 探测间隔 | `2s` |
| `timeout` | 单次探测超时 | `500ms` |
| `unhealthy_threshold` | 连续失败多少次后摘除健康实例 | `3` |
| `healthy_threshold` | 连续成功多少次后把未知/故障实例接入 | `2` |

判定逻辑（`internal/health`）：

- 实例状态机：`UNKNOWN → HEALTHY → UNHEALTHY → HEALTHY ...`。启动后立即探测一轮，探活成功连续达到 `healthy_threshold` 才进健康池，避免启动即接流量。
- **抗抖动**：健康实例只有**连续失败**达到 `unhealthy_threshold` 才摘除；期间任意一次成功会把失败计数清零。单次网络抖动不会误摘。
- **必摘除**：已挂实例每轮探测都失败（连接拒绝/超时/5xx 都算失败），必然在 `unhealthy_threshold × interval` 内被摘除，不会一直占坑。
- **自动恢复**：摘除后探测继续，连续成功达到 `healthy_threshold` 即重新进入健康池。
- 除主动探测外，真实转发中的连接失败/超时也会作为**被动信号**计入同一套连续计数，摘除更快但同样需要累计才生效。

### 3. 限流与超时

**限流（按服务配置，缺省不限流）**：

- `rate_limit.rate`：每秒补充的令牌数；`rate_limit.burst`：桶容量。
- 令牌桶初始满桶，允许 `burst` 个突发请求；令牌按经过时间惰性补充、上限为 `burst`。
- 取不到令牌时**立即**拒绝，不与后端建立连接：返回 `429 Too Many Requests`，并带：
  - `X-Gateway-Error: rate_limited`
  - `Retry-After: 1`

**超时**：

- `forward_timeout` 是单次转发的整体时限（建连 + 写请求 + 等响应头）。
- 后端超时返回 `504 Gateway Timeout`，并带 `X-Gateway-Error: upstream_timeout`；网关在时限到达即关闭连接，不会挂起堆积。
- 健康池为空 / 尝试过所有健康实例仍失败时返回 `503 Service Unavailable`，并带：
  - `X-Gateway-Error: no_healthy_upstream`（从未选出实例）
  - 或 `X-Gateway-Error: upstream_unreachable`（连接错误，换实例重试后仍失败）

错误对照：

| 场景 | HTTP 状态码 | `X-Gateway-Error` |
| --- | --- | --- |
| 超过限流 | 429 | `rate_limited` |
| 后端处理超时 | 504 | `upstream_timeout` |
| 无健康实例 / 连接失败重试耗尽 | 503 | `no_healthy_upstream` / `upstream_unreachable` |
| 无路由匹配 | 404 | （无） |

## 验证方法

### 自动化测试

```bash
go test ./... -count=1 -v
# 带竞态检测：
CGO_ENABLED=1 go test ./... -race -count=1
```

测试覆盖场景及预期（测试日志会打印每次命中的实例与判定依据）：

| 测试 | 场景 | 预期 |
| --- | --- | --- |
| `TestBackendDownAndRecover` | 3 实例轮询 → 1 个宕机 → 恢复 | 宕机前 3 个实例都被命中；摘除后请求不再落到它；恢复后重新被命中 |
| `TestAllInstancesDown` | 唯一实例宕机后恢复 | 健康池空时返回 `503 no_healthy_upstream`；恢复后自动回到 200 |
| `TestSingleFailureDoesNotEject` | 注入 1 次偶发失败 | 失败计数 1 < 阈值 3，实例保持 HEALTHY；一次成功即清零计数 |
| `TestCheckerActiveProbe` / `TestCheckerDeadConnection` | 真实 HTTP 探测循环 | 连续成功接入、连续失败摘除、关监听也会被摘除 |
| `TestRateLimit` | burst=3、rate=2/s 下突发 8 请求 | 恰好 3 个 200、5 个 `429 rate_limited`；等约 1s 补令牌后恢复 200 |
| `TestForwardTimeout` | 后端睡 1s、超时 200ms | 返回 `504 upstream_timeout`，整体耗时约 200ms 而非 1s |
| `TestRoundRobin*` | 候选稳定/动态变化 | 均匀分布；无候选返回 `ErrNoCandidate` |
| `TestTokenBucketBurstAndRefill` | 令牌桶突发、补充、封顶 | 满桶取 burst 个；按 elapsed×rate 补充；长期补充不超过 burst |
| `TestLoadDefaultsAndValidate` | 配置默认值/非法配置 | 默认值补齐；路由悬空、无实例、重名、限流参数非法等均启动报错 |

### 手工验证要点

```bash
# 1) 观察摘除日志：停掉某个后端实例后
#    [health] instance 127.0.0.1:19092 HEALTHY -> UNHEALTHY（连续失败 3 次，达到摘除阈值 3）
# 2) 观察恢复日志：重新拉起该实例后
#    [health] instance 127.0.0.1:19092 UNHEALTHY -> HEALTHY（连续成功 2 次，达到恢复阈值 2）
# 3) 每个请求的网关节点日志都会打印命中实例与判定依据：
#    [gateway] GET /users/1 -> service=user-service instance=127.0.0.1:19092 attempt=1 \
#      判定依据: 实例状态=HEALTHY 连续成功=5 连续失败=0 本轮健康候选=3

# 4) 限流：user-service rate=5/s burst=5，连打 8 次应有 429
for i in $(seq 1 8); do
  curl -s -o /dev/null -w '%{http_code} %header{x-gateway-error}\n' http://127.0.0.1:8080/users/1
done

# 5) 超时：把某后端业务接口改成 sleep 5s（> forward_timeout=2s）
curl -s -i http://127.0.0.1:8080/users/1 | grep -iE 'HTTP/|X-Gateway-Error'
# HTTP/1.1 504 Gateway Timeout / X-Gateway-Error: upstream_timeout
```

## 设计取舍备注

- 健康状态按实例加锁，读多写少场景开销很小；健康实例列表返回排序后的快照，保证均衡输入稳定。
- 转发只对"连不上/超时"做同请求内的实例级故障转移，避免对非幂等写请求盲目重试；后端明确返回的 5xx 被视作有效响应透传，由调用方决定语义。
- 令牌桶用惰性计算而非后台定时器，无额外 goroutine，吞吐只受锁竞争限制。
