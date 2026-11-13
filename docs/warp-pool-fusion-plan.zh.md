# 双池融合代码改造方案（A1 + A2 精确版）

> 仓库：`wbi35772/opencode2api`（`warp` 分支，工作副本 `o2a-warp`）
> 日期：2026-11-07
> 状态：评审稿，未改代码
> 对照文档：`docs/multi-region.zh.md`、`docs/expansion-warp-egress-nodes.zh.md`

---

## 0. 结论

当前代码不是“简单的 if 互斥”，而是三条通道各自带着**同一套熔断器**和不同的冷却语义：

| 事实 | 位置 |
| --- | --- |
| `warp.enabled` 时匿名通道整体走 WARP，静态代理池完全不进入 | `internal/gateway/upstream.go:366-369` |
| 静态代理 429（普通限流）冷却 = `failure_cooldown_seconds` 指数增长，基数 15s | `pool.go:431-446`、`config.go:273/333` |
| 静态代理 429（`FreeUsageLimitError`）**根本不冷却**，只 `closeIdle + quota_hits++` | `upstream.go:495-512` |
| `markBlocked()` 与 `freeUsageBlockTTL()` 已实现但**全仓库零调用**（死代码） | `pool.go:62`、`abuse.go:61` |
| 两个池共用 `anonymousNodes` 的熔断器（同一组 `breakerUntil/consecutive`） | `pool.go:106-121` |
| 静态游标 `Next()` 的兜底轮会返回**仍在冷却**的节点 | `pool.go:344` 与 `:359-381` |
| WARP 通道内部自带 `direct_fallback`（主机 IPv4），发生在外部池之前 | `warp.go` 尾部 |

所以 A1 不是改一个 if，需要三处结构性调整：

1. **调度层**：新增 `doFusedAnonymousUpstream`，两个池在同一请求内顺序尝试；
2. **熔断层**：把熔断器从 `anonymousNodes` 抽出为 `laneBreaker`，WARP 与静态池各一个，否则 WARP 挂掉会连带暂停静态池；
3. **冷却层**：静态池遇到 429 用**硬冷却**（`limitedUntil`），并让游标两轮扫描都跳过它——否则 15 分钟冷却会被“兜底轮”绕过。

---

## 1. 当前调用链（改造前）

```text
doUpstreamTiers (upstream.go:221)
  └─ route.Anonymous
       └─ doAnonymousUpstream (upstream.go:366)
            ├─ usesWARP(model, TierZen, true) ──► doWarpAnonymousUpstream (warp.go:318)
            │        └─ 池空/全冷却 → direct_fallback(主机 IPv4) → 返回
            └─ else ────────────────────────────► 静态 proxyfile 游标（本函数后半段）
```

`doUpstreamTiers` 只用两个信号决定后续：

- `errDeterministicClientRejection` → 立刻把 4xx 返回客户端；
- `errFreeTierIPBlocked` → `blocked=true`，跳过 Zen Key 层，只保留 Go 层（`upstream.go:247-254`、`:277-280`、`:313-321`）。

融合层必须**原样保留**这两个契约。

---

## 2. 任务 A1：双池融合 + 429 跨池 Fallback

### 2.1 新增文件 `internal/gateway/fusion.go`

```go
package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
)

// fusedPool names one of the two anonymous egress pools.
type fusedPool int

const (
	fusedWARP fusedPool = iota
	fusedStatic
)

// fusedTie alternates equally sized pools so neither starves the other.
var fusedTie atomic.Uint64

// fusedActive reports whether this request can draw from both pools: the WARP
// pool is enabled for the model and the anonymous list holds at least one exit
// that is not the host's own direct route.
func (g *Gateway) fusedActive(route models.Route) bool {
	if g == nil || g.warp == nil || !g.usesWARP(route.ID, config.TierZen, true) {
		return false
	}
	return g.anonymous.HasExternal()
}

// warpAvailable estimates WARP exits that can serve right now. The pool has no
// Available() accessor; Active minus Cooling is the same number Snapshot shows.
func (g *Gateway) warpAvailable() int {
	if g.warp == nil {
		return 0
	}
	s := g.warp.Snapshot()
	if s.Active <= s.Cooling {
		return 0
	}
	return s.Active - s.Cooling
}

// fusedOrder picks which pool this request tries first. A pool with no usable
// exit yields to the other; a pool with at least twice the warm capacity is
// preferred; otherwise the pools alternate through an atomic counter.
func (g *Gateway) fusedOrder(now time.Time) (fusedPool, fusedPool) {
	warpN := g.warpAvailable()
	staticN := g.anonymous.AvailableExits(now)
	switch {
	case warpN == 0 && staticN == 0:
		return fusedWARP, fusedStatic
	case warpN == 0:
		return fusedStatic, fusedWARP
	case staticN == 0:
		return fusedWARP, fusedStatic
	case warpN >= 2*staticN:
		return fusedWARP, fusedStatic
	case staticN >= 2*warpN:
		return fusedStatic, fusedWARP
	case fusedTie.Add(1)%2 == 1:
		return fusedWARP, fusedStatic
	default:
		return fusedStatic, fusedWARP
	}
}

// doFusedAnonymousUpstream runs the preferred pool, then the other one when the
// first cannot produce a 2xx. Both walks keep their own health accounting; the
// caller only sees one response and one error, with the same error precedence
// the single-pool lanes already use.
func (g *Gateway) doFusedAnonymousUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, error, int) {
	first, second := g.fusedOrder(time.Now())
	resp, err, attempts := g.fusedWalk(first, ctx, route, bodies, ids, attemptOffset)
	if resp != nil && resp.StatusCode/100 == 2 {
		return resp, nil, attempts
	}
	if errors.Is(err, errDeterministicClientRejection) || ctx.Err() != nil {
		return resp, err, attempts
	}
	if resp != nil {
		httpx.DrainAndClose(resp.Body)
	}
	g.logger.Info("anonymous pools fused; trying the other pool",
		"component", "upstream", "event", "fused_pool_fallback",
		"request_id", ids.Request, "from", fusedPoolName(first),
		"to", fusedPoolName(second), "attempts", attempts,
		"error", err)
	resp2, err2, used2 := g.fusedWalk(second, ctx, route, bodies, ids, attemptOffset+attempts)
	attempts += used2
	if resp2 != nil && resp2.StatusCode/100 == 2 {
		return resp2, nil, attempts
	}
	if errors.Is(err2, errDeterministicClientRejection) {
		return resp2, err2, attempts
	}
	// A spent free-tier quota on either pool must stay visible: doUpstreamTiers
	// uses it to skip the Zen key tier, which would answer 429 again.
	if errors.Is(err, errFreeTierIPBlocked) || errors.Is(err2, errFreeTierIPBlocked) {
		return resp2, errFreeTierIPBlocked, attempts
	}
	if resp2 != nil {
		return resp2, err2, attempts
	}
	if err2 != nil {
		return nil, err2, attempts
	}
	return nil, err, attempts
}

func (g *Gateway) fusedWalk(pool fusedPool, ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, offset int) (*http.Response, error, int) {
	if pool == fusedWARP {
		// allowDirect=false: the host's own IPv4 must stay behind the external
		// pool, not in front of it. See 2.5.
		return g.warpAnonymousWalk(ctx, route, bodies, ids, offset, false)
	}
	return g.doStaticAnonymousUpstream(ctx, route, bodies, ids, offset)
}

func fusedPoolName(pool fusedPool) string {
	if pool == fusedWARP {
		return "warp"
	}
	return "static"
}
```

### 2.2 `upstream.go`：只改入口，静态实现改名

原函数 366 行起：

```go
func (g *Gateway) doAnonymousUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, error, int) {
	if g.fusedActive(route) {
		return g.doFusedAnonymousUpstream(ctx, route, bodies, ids, attemptOffset)
	}
	if g.usesWARP(route.ID, config.TierZen, true) {
		return g.doWarpAnonymousUpstream(ctx, route, bodies, ids, attemptOffset)
	}
	return g.doStaticAnonymousUpstream(ctx, route, bodies, ids, attemptOffset)
}

// doStaticAnonymousUpstream walks the session's anonymous proxy cursor. ...
func (g *Gateway) doStaticAnonymousUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, error, int) {
	// 函数体保持原样，只删掉开头这三行：
	//   if g.usesWARP(...) { return g.doWarpAnonymousUpstream(...) }
	...
}
```

> 行为保持：`warp.enabled=true` 且 `proxyfile` 为空时，`RuntimeProxies()` 返回 `["direct"]`，`HasExternal()==false`，仍然走纯 WARP；`warp.enabled=false` 时仍然走纯静态池。

### 2.3 `pool.go`：新增双池探测

```go
// HasExternal reports whether the anonymous list holds at least one exit that
// is not the host's own direct route. A list that only carries "direct" is the
// host egress, not a second pool.
func (p *anonymousNodes) HasExternal() bool {
	if p == nil {
		return false
	}
	for _, node := range p.nodes {
		if node.proxy != nil && node.proxy.name != "direct" {
			return true
		}
	}
	return false
}

// AvailableExits counts exits that can serve a request now: healthy, not under
// an IP block, and not inside a 429 hard cooldown. Rotating #pool entries are
// always counted because a new connection draws a new egress.
func (p *anonymousNodes) AvailableExits(now time.Time) int {
	if p == nil {
		return 0
	}
	available := 0
	for _, node := range p.nodes {
		if node.proxy == nil || node.proxy.name == "direct" {
			continue
		}
		if node.proxy.blocked(now) {
			continue
		}
		if !node.proxy.healthy.Load() {
			continue
		}
		if !node.proxy.pool && node.limitedUntil.Load() > now.UnixNano() {
			continue
		}
		available++
	}
	return available
}
```

### 2.4 `pool.go`：熔断器按池拆分（必要，不是可选）

WARP 通道当前用 `g.anonymous.AllowAttempt/ReleaseProbe/NoteSuccess/NoteFailure`（`warp.go:319/322/396/449/510`）。共享熔断器的后果：WARP 隧道连续失败 → `NoteFailure` 触发 breaker → 静态池 `AllowAttempt()` 直接返回 `errAnonymousLanePaused`，Fallback 在“WARP 挂了”这个最需要它的场景失效。

改法：

1. 在 `pool.go` 新增：

```go
// laneBreaker is the per-lane circuit breaker that used to live inside
// anonymousNodes. WARP and the static proxy list each own one, so an outage of
// one lane can never pause the other.
type laneBreaker struct {
	backoff        anonymousBackoff
	breakerUntil   atomic.Int64
	breakerStrikes atomic.Int32
	consecutive    atomic.Int32
	probing        atomic.Bool
}

func newLaneBreaker(backoff anonymousBackoff) laneBreaker {
	return laneBreaker{backoff: backoff}
}
```

2. 把 `AllowAttempt/Open/ReleaseProbe/NoteSuccess/NoteNeutral/NoteFailure/ForceOpen/trip` 的实现原样搬到 `*laneBreaker` 上（只把接收者换掉）。

3. `anonymousNodes` 结构改为：

```go
type anonymousNodes struct {
	nodes    []*anonymousNode
	next     atomic.Uint64
	cooldown time.Duration
	rateLimitCooldown time.Duration
	hasPool  bool
	breaker  laneBreaker
}
```

并保留同名委托方法，`upstream.go` 现有调用零改动：

```go
func (p *anonymousNodes) AllowAttempt() bool {
	if p == nil {
		return true
	}
	return p.breaker.AllowAttempt()
}

func (p *anonymousNodes) Open() bool {
	if p == nil {
		return false
	}
	return p.breaker.Open()
}

func (p *anonymousNodes) ReleaseProbe()  { if p != nil { p.breaker.ReleaseProbe() } }
func (p *anonymousNodes) NoteSuccess()   { if p != nil { p.breaker.NoteSuccess() } }
func (p *anonymousNodes) NoteNeutral()   { if p != nil { p.breaker.NoteNeutral() } }
func (p *anonymousNodes) ForceOpen() (time.Duration, bool) {
	if p == nil { return 0, false }
	return p.breaker.ForceOpen()
}
func (p *anonymousNodes) NoteFailure() (time.Duration, bool) {
	if p == nil { return 0, false }
	return p.breaker.NoteFailure()
}
```

4. `gateway.go` 的 `Gateway` 结构新增 `warpLane laneBreaker`，`New()` 里：

```go
backoff := anonymousBackoffFromConfig(cfg.Models.AnonymousBackoff)
...
anonymous: anonymousPool(cfg.Anonymous, transports, cooldown, rateLimitCooldown, backoff),
warpLane:  newLaneBreaker(backoff),
```

5. `warp.go` 的 5 处调用改为 `g.warpLane.*`。

### 2.5 `warp.go`：把 `direct_fallback` 放到两池之后

WARP 通道末尾会在 `blocked` 或 `errWARPUnavailable` 时用 `g.directTransport` 直连主机 IPv4。融合时这个动作发生在外部池**之前**，会把外部池挡掉。改法：

```go
func (g *Gateway) doWarpAnonymousUpstream(...) (*http.Response, error, int) {
	return g.warpAnonymousWalk(ctx, route, bodies, ids, attemptOffset, true)
}

// warpAnonymousWalk is doWarpAnonymousUpstream without the single-pool
// wrapper. allowDirect is false when another pool still has to be tried: the
// host's own IPv4 is the last resort, not a bypass of the fused order.
func (g *Gateway) warpAnonymousWalk(..., allowDirect bool) (...) {
	...
}
```

尾部条件：

```go
if allowDirect && g.cfg.Warp.DirectFallback && (blocked || errors.Is(lastErr, errWARPUnavailable)) && ctx.Err() == nil {
```

> 融合容器建议同时把 `warp.direct_fallback` 设为 `false`：主机 IPv4 在 2026-11-07 的实测里四台全部 `429 FreeUsageLimitError`，留着只会多绕两轮。

### 2.6 尝试预算

| 阶段 | 上限 |
| --- | --- |
| WARP 池 | `retry.max_attempts`（计划 6） |
| 静态池 | `min(len(exits), max_anonymous_exits)`（默认 8；列表无 `#pool` 时不会被 `max_attempts` 抬高） |
| 最坏总次数 | 14 |

两池共享父 `ctx`，总时长仍受 `first_event_timeout_seconds` / `attempt_timeout_seconds` 和 HTTP 请求超时约束，不会无限叠加。

> 契约里“`max_attempts` 提到 6 保证充足重试”对静态池不完全成立：6 只作用于 WARP 池与 `#pool` 列表。静态池上限仍是 `max_anonymous_exits=8`。

### 2.7 日志与遥测

- 新增 `event=fused_pool_fallback`、`from=warp|static`、`to=...`、`error`。
- 每次尝试仍走 `recordUpstreamAttempt`：WARP 记录 `warpProxy(egress)`，静态记录 `node.proxy.name`（已脱敏）。
- 成功事件沿用两条通道原有的 `anonymous_attempt_succeeded`。

---

## 3. 任务 A2：静态代理 429 冷却 15s → 15m

### 3.1 先修一个事实错误

契约说“静态代理 429 冷却 15 秒”。准确情况分两种：

| 429 类型 | 当前行为 |
| --- | --- |
| `FreeUsageLimitError`（额度耗尽） | **完全不冷却**。`upstream.go:495-512` 只 `closeIdle()` + `noteQuotaHit()`，节点仍在游标里，下一个请求立刻再抽到它 |
| 普通 429（会话/限流） | 第一次 429 只旋转 session；第二次 429 才 `MarkFailure` → 15s×2ⁿ，最高 120s |

也就是说，真正“反复抽到已耗尽 `/64`”的原因是**额度 429 没有冷却**，不是 15 秒太短。A2 要同时补上这两条路径。

### 3.2 配置

`internal/config/config.go` 的 `PerformanceConfig`：

```go
	FailureCooldownSeconds   int `json:"failure_cooldown_seconds"`
	// StaticProxyRateLimitCooldownSeconds keeps a static anonymous exit out of
	// rotation after HTTP 429. A static exit cannot change its egress address
	// between attempts, so the short transport cooldown only re-draws a spent
	// quota. Zero uses the built-in 15 minutes, aligned with an external
	// producer that re-rolls each exit's /64 about every 20 minutes.
	StaticProxyRateLimitCooldownSeconds int `json:"static_proxy_rate_limit_cooldown_seconds"`
```

校验（放在 `max_anonymous_exits` 校验后面）：

```go
	if cfg.Performance.StaticProxyRateLimitCooldownSeconds < 0 {
		return Config{}, errors.New("performance.static_proxy_rate_limit_cooldown_seconds must not be negative (0 uses the built-in 15 minutes)")
	}
```

`config.example.json` 的 `performance` 增加一行：

```json
    "static_proxy_rate_limit_cooldown_seconds": 900
```

`gateway.go` 的 `New()`：

```go
	rateLimitCooldown := time.Duration(cfg.Performance.StaticProxyRateLimitCooldownSeconds) * time.Second
	if rateLimitCooldown <= 0 {
		rateLimitCooldown = defaultStaticRateLimitCooldown
	}
```

### 3.3 `pool.go`：硬冷却字段

```go
// defaultStaticRateLimitCooldown is how long a static anonymous exit stays out
// of rotation after HTTP 429 when performance.static_proxy_rate_limit_cooldown_seconds
// is unset.
const defaultStaticRateLimitCooldown = 15 * time.Minute

type anonymousNode struct {
	proxy         *proxyTransport
	failures      atomic.Uint32
	cooldownUntil atomic.Int64 // soft: transport failure, can be a last resort
	limitedUntil  atomic.Int64 // hard: HTTP 429, never drawn until it expires
}
```

`MarkSuccess` / `RestoreProxy` 同时清 `limitedUntil`。

### 3.4 `pool.go`：`MarkFailure` 按状态分流

```go
func (p *anonymousNodes) MarkFailure(node *anonymousNode, resp *http.Response, err error) {
	if node == nil || (node.proxy != nil && node.proxy.pool) {
		return
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if err == nil && resp != nil && status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusTooManyRequests && status < 500 {
		return
	}
	// A 429 is metered to the egress address, and a static exit keeps that
	// address until the external producer re-rolls it. Use the hard window
	// instead of the exponential transport cooldown.
	if status == http.StatusTooManyRequests {
		delay := p.rateLimitCooldown
		if delay <= 0 {
			delay = defaultStaticRateLimitCooldown
		}
		// A genuinely short Retry-After (a real rate limit) may shorten it;
		// a long one (free-tier quota, ~17h) must not extend it past the
		// producer's re-roll cycle.
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > 0 && retryAfter < delay {
			delay = retryAfter
		}
		node.limitedUntil.Store(time.Now().Add(delay).UnixNano())
		return
	}
	failures := node.failures.Add(1)
	delay := p.cooldown * time.Duration(1<<min(failures-1, 3))
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = min(retryAfter, p.cooldown*maxAnonymousRetryAfterMultiplier)
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}
```

### 3.5 `upstream.go`：额度 429 立刻落冷却

`doStaticAnonymousUpstream` 的 `isFreeUsageLimitBody` 分支，在 `node.proxy.noteQuotaHit()` 后加一行：

```go
				node.proxy.closeIdle()
				node.proxy.noteQuotaHit()
				// A static exit keeps the spent egress: cool it for the
				// rate-limit window instead of drawing it again next request.
				g.anonymous.MarkFailure(node, resp, nil)
				lastErr = errFreeTierIPBlocked
				blocked = true
				deferred = true
```

注意保持 `deferred=true`：底部不会再走 `observeAnonymousResult`，也就不会把额度 429 计进 lane 熔断器。

### 3.6 `pool.go`：游标两轮都跳过硬冷却

`anonymousCursor.Next()` 第一次扫描（`pool.go:337`）：

```go
		if isBlocked(node) || isLimited(node) {
			c.used[idx] = true
			continue
		}
```

兜底轮（`pool.go:359-376`）也要跳过，否则“最后一个节点”会绕过 15 分钟：

```go
	for step := range n {
		idx := (c.start + step) % n
		if c.used[idx] || isLimited(c.pool.nodes[idx]) {
			continue
		}
		...
	}
```

函数顶部：

```go
	isLimited := func(node *anonymousNode) bool {
		return node != nil && node.limitedUntil.Load() > now
	}
```

> `cooldownUntil`（15–120s 传输冷却）保持“软冷却”，仍可作为兜底最后尝试；只有 429 的 `limitedUntil` 是硬跳过。

### 3.7 可观测性（可选）

`/healthz` 的 `proxies` 增加 `rate_limited` 计数（`limitedUntil > now` 的节点数），用于验收“429 后 15 分钟内不再被调度”。

---

## 4. 部署与配置（A3）

```json
{
  "warp": { "enabled": true, "direct_fallback": false },
  "proxies": [],
  "proxyfile": "/var/lib/opencode2api/egress-ext.txt",
  "performance": { "static_proxy_rate_limit_cooldown_seconds": 900 },
  "retry": { "max_attempts": 6 }
}
```

- 备份 `config.json.bak-<timestamp>`；
- 写入 `egress-ext.txt`（**不要加 `#pool` 后缀**，否则 `MarkFailure` 会直接 return，A2 失效）；
- `POST /api/config/reload` 确认 `{"applied":true,"restart_required":false}`；
- 严禁 `compute restart`。

---

## 5. 验收

1. `go build ./... && go test ./...` 全绿。
2. 单元：
   - 双池存在时 `fusedOrder` 在等量时交替；单池无可用时顺序反转；
   - 429 后 `limitedUntil ≈ now+15m`；`Retry-After: 30` 时 `≈ now+30s`；
   - 游标两轮都不返回 `limitedUntil` 未到期的节点。
3. 联调（一台容器）：
   - 连续 20 次 `/v1/chat/completions`，日志同时出现 `warp` 与 `socks5://...:108x` 两类出口；
   - 人为让一个静态端口返回 429，15 分钟内该端口不再出现在任何请求里；
   - 全静态池 429 时，请求自动回到 WARP 池并 200。
4. 回滚：恢复备份 → reload。

---

## 6. 给会话 B 的接口修正

1. **不要 `#pool` 后缀**：带 `#pool` 的条目被当作“每条连接换出口”，`MarkFailure` 直接跳过，429 冷却不会生效。
2. 15 分钟冷却 + 20 分钟换 `/64` 的组合是自洽的：冷却结束时端口还剩约 5 分钟，随后被重摇。
3. 若某国家节点的 16 个端口全部额度耗尽，静态池会整体进入 15 分钟硬冷却；这期间流量全部回落本地 WARP 池。这是设计预期，不是故障。

---

## 7. 风险

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 首 token 变慢 | 两池串行尝试，最坏 14 次 | 监控 `fused_pool_fallback` 比例；必要时把 `max_anonymous_exits` 降到 4 |
| 熔断器语义变化 | 拆分后 WARP 与静态互不影响 | 需要为 WARP 单独调 `models.anonymous_backoff` 时再评估 |
| 静态池全冷却 | 15 分钟内无静态出口 | 本地 WARP 池兜底；`direct_fallback=false` 时不碰主机 IPv4 |
| 游标亲和 | 同一会话固定落在一个静态出口，可能连续撞同一块 `/64` | 429 后该出口硬冷却，下一请求自然换出口 |
