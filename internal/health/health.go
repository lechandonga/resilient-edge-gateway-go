package health

import (
	"context"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// State 表示实例的健康状态。
type State int

const (
	StateUnknown   State = iota // 初始未知，尚未通过任何探测
	StateHealthy                // 健康，可接流量
	StateUnhealthy              // 不健康，已摘除
)

func (s State) String() string {
	switch s {
	case StateHealthy:
		return "HEALTHY"
	case StateUnhealthy:
		return "UNHEALTHY"
	default:
		return "UNKNOWN"
	}
}

// Instance 记录一个后端实例的探测状态。
//
// 摘除/恢复判定基于"连续计数"：
//   - 健康连续成功 healthyThreshold 次：UNKNOWN/UNHEALTHY -> HEALTHY（恢复接入）
//   - 连续失败 unhealthyThreshold 次：HEALTHY -> UNHEALTHY（摘除）
//
// 单次抖动只会把失败计数 +1，不会立刻摘实例；
// 已挂实例每轮探测都失败，达到阈值后必然被摘除，不会长期留在池中。
type Instance struct {
	Address string

	mu                 sync.RWMutex
	state              State
	consecutiveSuccess int
	consecutiveFailure int
}

// State 返回实例当前状态。
func (i *Instance) State() State {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.state
}

// Info 返回状态与当前连续成功/失败计数，用于日志说明"判定依据"。
func (i *Instance) Info() (state State, succStreak, failStreak int) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.state, i.consecutiveSuccess, i.consecutiveFailure
}

// recordSuccess 记录一次成功，返回是否发生了状态迁移与迁移后的状态。
func (i *Instance) recordSuccess(healthyThreshold int) (changed bool, newState State) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.consecutiveSuccess++
	i.consecutiveFailure = 0

	if i.state != StateHealthy && i.consecutiveSuccess >= healthyThreshold {
		log.Printf("[health] instance %s %s -> HEALTHY（连续成功 %d 次，达到恢复阈值 %d）",
			i.Address, i.state, i.consecutiveSuccess, healthyThreshold)
		i.state = StateHealthy
		return true, StateHealthy
	}
	return false, i.state
}

// recordFailure 记录一次失败，返回是否发生了状态迁移与迁移后的状态。
func (i *Instance) recordFailure(unhealthyThreshold int) (changed bool, newState State) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.consecutiveFailure++
	i.consecutiveSuccess = 0

	if i.state == StateHealthy && i.consecutiveFailure >= unhealthyThreshold {
		log.Printf("[health] instance %s HEALTHY -> UNHEALTHY（连续失败 %d 次，达到摘除阈值 %d）",
			i.Address, i.consecutiveFailure, unhealthyThreshold)
		i.state = StateUnhealthy
		return true, StateUnhealthy
	}
	return false, i.state
}

// sortStrings 对地址切片做稳定字典序排序，
// 让 HealthyInstances 的输出顺序只取决于地址集合，避免轮询偏移。
func sortStrings(s []string) {
	sort.Strings(s)
}

// Checker 周期性探测所有实例，按连续成功/失败阈值做摘除与恢复。
type Checker struct {
	mu        sync.RWMutex
	instances map[string]*Instance

	probePath          string
	probeTimeout       time.Duration
	healthyThreshold   int
	unhealthyThreshold int
	httpClient         *http.Client
}

// NewChecker 创建健康检查器，所有实例初始为 UNKNOWN。
func NewChecker(addresses []string) *Checker {
	c := &Checker{instances: make(map[string]*Instance, len(addresses))}
	for _, addr := range addresses {
		c.instances[addr] = &Instance{Address: addr, state: StateUnknown}
	}
	return c
}

// Register 注册/补充一个实例；已存在则不处理。
func (c *Checker) Register(address string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.instances[address]; !ok {
		c.instances[address] = &Instance{Address: address, state: StateUnknown}
	}
}

// Addresses 返回当前已注册实例地址。
func (c *Checker) Addresses() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	addrs := make([]string, 0, len(c.instances))
	for addr := range c.instances {
		addrs = append(addrs, addr)
	}
	return addrs
}

// IsHealthy 查询实例是否健康可接流量。
func (c *Checker) IsHealthy(address string) bool {
	c.mu.RLock()
	inst, ok := c.instances[address]
	c.mu.RUnlock()
	if !ok {
		return false
	}
	return inst.State() == StateHealthy
}

// HealthyInstances 返回当前健康实例地址列表，顺序稳定（按注册顺序的地址排序）。
func (c *Checker) HealthyInstances() []string {
	c.mu.RLock()
	addrs := make([]string, 0, len(c.instances))
	for addr, inst := range c.instances {
		if inst.State() == StateHealthy {
			addrs = append(addrs, addr)
		}
	}
	c.mu.RUnlock()
	sortStrings(addrs)
	return addrs
}

// Snapshot 返回 实例地址 -> 状态 的快照。
func (c *Checker) Snapshot() map[string]State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	snap := make(map[string]State, len(c.instances))
	for addr, inst := range c.instances {
		snap[addr] = inst.State()
	}
	return snap
}

// Info 返回指定实例的状态与连续成功/失败计数；实例不存在时返回 Unknown。
func (c *Checker) Info(address string) (state State, succStreak, failStreak int) {
	c.mu.RLock()
	inst, ok := c.instances[address]
	c.mu.RUnlock()
	if !ok {
		return StateUnknown, 0, 0
	}
	return inst.Info()
}

// RecordForwardResult 把真实转发结果也计入连续计数（被动信号），
// 与主动探测共用同一套阈值，避免偶发抖动误摘。
func (c *Checker) RecordForwardResult(address string, success bool) {
	c.mu.RLock()
	inst, ok := c.instances[address]
	c.mu.RUnlock()
	if !ok {
		return
	}
	if success {
		inst.recordSuccess(c.healthyThreshold)
	} else {
		inst.recordFailure(c.unhealthyThreshold)
	}
}

// probeOnce 对单个实例发一次 HTTP GET 探测；2xx/3xx 视为成功。
func (c *Checker) probeOnce(ctx context.Context, inst *Instance) {
	reqCtx, cancel := context.WithTimeout(ctx, c.probeTimeout)
	defer cancel()

	url := "http://" + inst.Address + c.probePath
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		inst.recordFailure(c.unhealthyThreshold)
		return
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		inst.recordFailure(c.unhealthyThreshold)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 400 {
		inst.recordSuccess(c.healthyThreshold)
	} else {
		inst.recordFailure(c.unhealthyThreshold)
	}
}

// probeTimeout 保存探测超时（由 Start 注入）。
// 放在字段里以便 probeOnce 使用，默认 500ms。
//
// Start 启动后台探测循环，直到 ctx 取消。
// 启动后会立即探测一轮，让实例尽快进入 HEALTHY。
func (c *Checker) Start(ctx context.Context, interval, timeout time.Duration, probePath string, healthyThreshold, unhealthyThreshold int) {
	c.probePath = probePath
	c.healthyThreshold = healthyThreshold
	c.unhealthyThreshold = unhealthyThreshold
	c.probeTimeout = timeout
	c.httpClient = &http.Client{Timeout: timeout}

	probeAll := func() {
		c.mu.RLock()
		instances := make([]*Instance, 0, len(c.instances))
		for _, inst := range c.instances {
			instances = append(instances, inst)
		}
		c.mu.RUnlock()

		var wg sync.WaitGroup
		for _, inst := range instances {
			wg.Add(1)
			go func(inst *Instance) {
				defer wg.Done()
				c.probeOnce(ctx, inst)
			}(inst)
		}
		wg.Wait()
	}

	// 立即探测一轮，避免启动初期所有实例都是 UNKNOWN。
	probeAll()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[health] checker stopped")
			return
		case <-ticker.C:
			probeAll()
		}
	}
}

// Stop 是占位方法；生命周期实际由传给 Start 的 ctx 控制。
func (c *Checker) Stop() {}
