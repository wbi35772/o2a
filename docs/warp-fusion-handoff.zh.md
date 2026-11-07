# WARP 双池融合：交付与对接报告（Session A → Session B）

> 分支：`warp-fusion`（基于 `warp-stability@397ca30`，融合提交后合并 `main@beb1731` 的 producer）
> 日期：2026-11-07
> 状态：代码已提交；`gofmt` 干净；`CGO_ENABLED=0 go build ./...` 通过；`go test ./...` 全绿；沙箱双池实测通过；生产未部署
> 设计稿：`docs/warp-pool-fusion-plan.zh.md`
> 对端工程：`instacloud-warp/`（`main@beb1731`，Session B）

---

## 0. 一句话结论

网关侧的双池融合已经收口：WARP 池与外部静态 SOCKS5 池不再互斥，按可用容量轮询，首个池拿不到 2xx 时在同一请求内跨池 Fallback；静态节点遇到 429 进入 15 分钟硬冷却，额度类 429 不采纳上游长达十几小时的 `Retry-After`；跨池切换前必定 drain + close 首个响应。Session B 只需按本文第 5、6 节的契约产出 `egress-ext.txt`，网关即可消费。当前真正的阻断不在网关，而在 producer 工程缺 3 个文件（第 7 节）以及 egress 文件的跨机同步尚未自动化（第 9 节）。

---

## 1. 基线与提交拓扑

| 项 | 值 |
| --- | --- |
| 网关基线 | `warp-stability@397ca30`（2026-11-06T06:46:10Z，远端 tip） |
| 融合提交 | `feat(gateway): fuse WARP and static proxy pools with cross-pool fallback` |
| 对端基线 | `main@beb1731`（2026-11-07T12:27:24Z，producer entrypoint） |
| 合并方式 | `Merge branch 'main' into warp-fusion`，只新增 `instacloud-warp/`，与网关代码无冲突 |
| 分支 | `warp-fusion`（不覆盖 `warp-stability`） |
| 构建 | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o opencode2api ./cmd/opencode2api` |

基线选择理由：`397ca30` 是网关代码最新线，包含代理中心四卡片（`563fb58`）、WARP/节点池分计数（`aeef22f`）、会话钉出口（`14e9b51`）。`main` 的 producer 提交基于旧 `main`（`58f5667`），不含这 31 个网关提交，因此不能拿 `main` 直接替换基线；正确做法是合并，而不是换基。

---

## 2. 本次完善点（必须保留，不得在耦合时丢失）

### 2.1 A1：双池融合 + 429 跨池 Fallback

| 能力 | 代码位置 | 行为 |
| --- | --- | --- |
| 解除 `warp.enabled` 与 `proxyfile` 互斥 | `internal/gateway/upstream.go:349` | `doAnonymousUpstream` 变成路由器：双池可用走融合，否则 WARP 单池，否则静态单池 |
| 融合判定 | `internal/gateway/fusion.go:30` | WARP 对当前模型启用，且匿名池存在非 `direct` 出口 |
| 选池顺序 | `internal/gateway/fusion.go:54` | 一方为 0 让位；一方容量 ≥ 另一方 2 倍优先；否则原子计数器交替 |
| 跨池 Fallback | `internal/gateway/fusion.go:81` | 首池非 2xx → drain+close → 次池；确定性客户端拒绝或 ctx 取消不 fallback；两池都额度耗尽时向上抛 `errFreeTierIPBlocked` |
| 池内走法 | `internal/gateway/fusion.go:120` | WARP 走 `warpAnonymousWalk(..., false)`；静态走 `doStaticAnonymousUpstream` |
| WARP 直连兜底门控 | `internal/gateway/warp.go:326`、`warp.go:473` | 融合时 `allowDirect=false`，宿主 IPv4 排在两池之后；单池 WARP 仍为 `true` |
| 熔断器按池拆分 | `internal/gateway/gateway.go:84`、`gateway.go:152` | `warpLane` 独立于 `anonymous.breaker`；WARP 故障不再暂停静态池，反之亦然 |

请求流：

```text
free-tier request
  └─ doAnonymousUpstream
       ├─ fusedActive? ── yes ── fusedOrder(now)
       │                          ├─ fusedWalk(first)
       │                          │    ├─ WARP: warpAnonymousWalk(allowDirect=false)
       │                          │    └─ static: doStaticAnonymousUpstream
       │                          ├─ 2xx → return
       │                          ├─ deterministic / ctx done → return
       │                          ├─ DrainAndClose(first response)
       │                          └─ fusedWalk(second) → 2xx 或按既有优先级返回错误
       ├─ usesWARP? ── yes ── doWarpAnonymousUpstream(allowDirect=true)
       └─ else ── doStaticAnonymousUpstream
```

### 2.2 A2：静态代理 429 硬冷却 15 分钟

| 能力 | 代码位置 | 行为 |
| --- | --- | --- |
| 新配置项 | `internal/config/config.go:286-291`、`config.go:429-430` | `performance.static_proxy_rate_limit_cooldown_seconds`；负值拒绝；0 使用内置 900s |
| 默认值 | `internal/gateway/pool.go:151-156` | `defaultStaticRateLimitCooldown = 15 * time.Minute` |
| 节点硬冷却字段 | `internal/gateway/pool.go:154` | `limitedUntil` 与软冷却 `cooldownUntil` 分离 |
| 游标硬跳过 | `internal/gateway/pool.go:389-440` | 第一轮、`#pool` 回退轮、最后兜底轮都不返回 `limitedUntil` 未到期的节点 |
| 429 记罚 | `internal/gateway/pool.go:505` | 额度类 `FreeUsageLimitError`：满窗 900s，忽略 `Retry-After`；瞬态 429：只采纳比 900s 更短的 `Retry-After`；429 不计入 lane 失败、不触发熔断 |
| 额度 429 调用点 | `internal/gateway/upstream.go:514-520` | 静态节点收到额度 429 时 `MarkFailure(node, resp, nil)`；`#pool` 节点自动 no-op |
| 成功/恢复清理 | `internal/gateway/pool.go:456`、`pool.go:467` | `MarkSuccess` 与 `RestoreProxy` 同时清 `limitedUntil` |

### 2.3 边界 1：静态池统计对称暴露

- `HasExternal`（`pool.go:554`）：只认非 `direct` 的真实出口，决定是否进入融合。
- `AvailableExits`（`pool.go:571`）：排除 `direct`、被封、不健康、软冷却、硬冷却；`#pool` 轮换节点始终计入。
- `fusedOrder`（`fusion.go:54`）：静态池全 limited 时返回 0，自动让位 WARP。
- 静态池选不出节点时只返回“无可用出口”，不会误触发静态 `laneBreaker`；429 分支不调用 `NoteFailure`。

### 2.4 边界 2：额度 429 用满冷却窗

- 判定依据：`isFreeUsageLimitBody`（`internal/gateway/abuse.go:101`）匹配 `FreeUsageLimitError` / `free usage limit reached`。
- 额度类：固定 `now + rateLimitCooldown`，不采纳更短 `Retry-After`，也不被指数软冷却缩短。
- 非额度瞬态 429：`hold = min(configured, Retry-After)`；缺失或更长时保持 900s。

### 2.5 边界 3：跨池丢弃响应必须 drain + close

- `fusion.go:90-92`：首池非 2xx 且不是确定性拒绝时，先 `httpx.DrainAndClose(resp.Body)` 再走次池。
- WARP 直连兜底路径原本已有同样的 drain（`warp.go:470` 附近），融合路径复用了该约束。

### 2.6 回归测试

`internal/gateway/pool_test.go`：

| 测试 | 覆盖 |
| --- | --- |
| `TestQuota429HoldsFullWindowAndIgnoresRetryAfter` | 额度 429 满窗 900s、忽略长 `Retry-After`、不计 lane 失败、不触发 breaker、`AvailableExits=0` |
| `TestTransient429AdoptsShorterRetryAfter` | 瞬态 429 采纳 20s 短窗 |
| `TestAvailableExitsCountsDrawableStaticNodes` | 只计 live 与 `#pool`，排除 `direct`/unhealthy/limited/软冷却；`HasExternal` 对 direct-only 为 false |

验证回显：`ok opencode2api/internal/gateway`、`ok opencode2api/internal/config`；`gofmt -l` 无输出。

---

## 3. 配置契约（网关侧）

融合容器推荐配置：

```json
{
  "anonymous": true,
  "proxies": [],
  "proxyfile": "/var/lib/opencode2api/egress-ext.txt",
  "warp": {
    "enabled": true,
    "pool_size": 4,
    "stack": "ipv6",
    "direct_fallback": false
  },
  "performance": {
    "failure_cooldown_seconds": 15,
    "max_anonymous_exits": 0,
    "free_tier_block_seconds": 0,
    "static_proxy_rate_limit_cooldown_seconds": 900
  }
}
```

要点：

- `proxies: []`：静态池只留 `egress-ext.txt`，避免旧代理条目混入同一静态池。
- `direct_fallback: false`：融合时宿主 IPv4 必须排在两池之后；单池 WARP 才考虑开启。
- `max_anonymous_exits: 0`：使用内置默认 8（`upstream.go:335`），不是“无限”。16 节点全试会拉长单请求时间；需要全量探测时显式设为 16。
- `static_proxy_rate_limit_cooldown_seconds: 900`：与 producer 的 `REROLL_INTERVAL=900` 对齐；0 也等于 900。
- `warp.pool_size` 按机器规格设置；沙箱实测 4 足够，生产沿用现值。

---

## 4. 外部池数据契约（网关消费侧）

### 4.1 文件

| 项 | 要求 |
| --- | --- |
| 容器内路径 | `/var/lib/opencode2api/egress-ext.txt`（与 `proxyfile` 一致） |
| 编码 | UTF-8，LF 换行，一行一个出口 |
| 空行 | 忽略；`#` 注释行忽略 |
| 权限 | `0644` 足够；文件内是明文凭据，不要进镜像、不要提交仓库 |

### 4.2 行格式

```text
socks5://warpuser:WarpPass2026@JP_NODE_IP:1081
socks5://warpuser:WarpPass2026@JP_NODE_IP:1082
socks5://warpuser:WarpPass2026@JP_NODE_IP:1083
socks5://warpuser:WarpPass2026@JP_NODE_IP:1084
socks5://warpuser:WarpPass2026@JP_NODE_IP:1085
socks5://warpuser:WarpPass2026@JP_NODE_IP:1086
socks5://warpuser:WarpPass2026@JP_NODE_IP:1087
socks5://warpuser:WarpPass2026@JP_NODE_IP:1088
socks5://warpuser:WarpPass2026@JP_NODE_IP:1089
socks5://warpuser:WarpPass2026@JP_NODE_IP:1090
socks5://warpuser:WarpPass2026@JP_NODE_IP:1091
socks5://warpuser:WarpPass2026@JP_NODE_IP:1092
socks5://warpuser:WarpPass2026@JP_NODE_IP:1093
socks5://warpuser:WarpPass2026@JP_NODE_IP:1094
socks5://warpuser:WarpPass2026@JP_NODE_IP:1095
socks5://warpuser:WarpPass2026@JP_NODE_IP:1096
```

### 4.3 硬性约束

1. **严禁 `#pool` 后缀**。带 `#pool` 的条目会被当作轮换传输：每次新连接、绕过硬冷却、`MarkFailure` 直接 return。producer 的端口是固定出口，必须按静态节点处理。
2. **纯 IPv6 出口**。producer 已在 `render_conf` 中剥离 `172.16.0.2/32`、IPv4 resolver 与默认路由；网关不校验出口族，IPv4 泄漏由 producer 侧保证。
3. **不得与现有 `/48` 重叠**。producer 的 `DENY_48_PREFIXES` 默认已包含三台生产的 `2a09:bac5:55`、`2a09:bac1:65`、`2a09:bac5:c82`、`2a09:bac1:27b`、`2a09:bac1:1e`。新增节点必须保留该列表。
4. **只写健康出口**。producer 的 `write_egress` 只输出 `state == ok` 的行，因此文件可能少于 16 行；网关按实际行数建池，`AvailableExits` 只计健康节点。
5. **认证**：普通 HTTP 客户端走 `http.Transport` 的 SOCKS5 User；Bun 伪装客户端走自研 `socks5Connect`，发 RFC1929 用户名/密码（`internal/httpx/bun_dial.go`）。两边都支持 `socks5://user:pass@host:port`。
6. **日志脱敏**：`config.RedactURL` 会把密码显示为 `***`；文件本身不要挂进 WebUI 可下载目录。

---

## 5. Producer 对接矩阵（`instacloud-warp/`，`main@beb1731`）

| Producer 侧 | 值/行为 | 网关侧消费 |
| --- | --- | --- |
| `INSTANCE_COUNT` / `POOL_SIZE` | 16 | 预期 16 个静态节点；文件少于 16 行属正常 |
| `START_PORT` | 1081 | 端口 1081–1096 |
| `SOCKS_USER` / `SOCKS_PASS` | `warpuser` / `WarpPass2026` | URL userinfo；与网关契约一致 |
| `EXTERNAL_HOST` | 空则自动探测 | 写入 `egress-ext.txt` 的 `HOST`；跨机访问必须是可达的公网地址 |
| `STATE_DIR` | `/data`（命名卷 `warp-pool-data`） | 文件位置 `/data/egress-ext.txt`，需同步到三台网关 |
| `REROLL_INTERVAL` | 900s | 与 900s 硬冷却对齐；每 15 分钟换一批 `/64` |
| `REROLL_BATCH` | 4 | 每批 4 个，16 个约 4 批完成；冷却与轮换错峰 |
| `HEALTH_INTERVAL` | 300s | 健康复查；失败会触发 reroll |
| `DEDUP_RETRIES` | 4 | 重复 `/64` 或非法前缀重试 |
| `REGISTER_RETRIES` | 3 | 注册失败重试 |
| `WGCF_KEEPALIVE` | 25 | WireGuard 保活 |
| `PROBE_URL` | `https://opencode.ai/cdn-cgi/trace` | 验证 `ip`/`loc`；只有合法 IPv6 且不在 deny 列表才 `ok` |
| `DENY_48_PREFIXES` | 三台生产家族 | 防重叠；新增节点必须保留 |
| `network_mode: host` | 端口落在节点 INPUT | 需要防火墙放行中心容器源 IP 到 1081–1096 |
| `EXPOSE 1081-1096` | 仅声明 | 实际以 host 网络为准 |
| `egress-ext.txt` | `socks5://user:pass@host:port` | 网关 `proxyfile` 直接读取 |

Producer 的 `write_egress` 行为（`instacloud-warp/entrypoint.sh`）：

```bash
# 只输出 state=ok 的实例；文件为空时只告警，不退出
printf 'socks5://%s:%s@%s:%s\n' "$SOCKS_USER" "$SOCKS_PASS" "$PUBLIC_HOST" "$port" >>"$out"
```

`PUBLIC_HOST` 来源：`EXTERNAL_HOST` 优先；否则 `detect_host()` 自动探测。跨机部署必须显式设置 `EXTERNAL_HOST`，不要依赖自动探测。

---

## 6. 网关侧运行与可观测性

### 6.1 healthz

```bash
curl -sS http://127.0.0.1:8081/healthz
```

关键字段：

| 字段 | 含义 |
| --- | --- |
| `proxies.total` | 静态池节点数（= egress 文件行数） |
| `proxies.healthy` / `unhealthy` / `blocked` | 静态池健康状态 |
| `proxies.quota_hits` | 累计额度 429 次数 |
| `warp.size` / `active` / `standby` / `cooling` | WARP 池状态；`active - cooling` 即 `warpAvailable()` |

### 6.2 日志事件

| 事件 | 含义 | 成功信号 |
| --- | --- | --- |
| `fused_pool_fallback` | 首池失败，切次池 | `from=static to=warp` 或反向，带 `attempts`、`error` |
| `free_usage_limit_reached` | 上游额度 429 | 带 `Retry-After`；节点进 900s 硬冷却 |
| `anonymous_attempt_succeeded` | 匿名出口成功 | `status=2xx`、`proxy`、`attempt` |
| `anonymous_breaker_open` | 某池熔断 | 只影响该池；另一池继续 |

### 6.3 热加载

配置改动走磁盘 + reload，不重启 app：

```bash
# 容器内 admin 端口（8080 仅容器内）
curl -sS -X POST 'http://127.0.0.1:8080/api/config/reload' \
  -H 'Authorization: Bearer TOKEN'
```

`Reload` 会重新 `config.Load`，包括重新解析 `proxyfile`，重建 runtime 并原子切换（`internal/gateway/runtime.go:441`）。只有 `listen`、`webui.listen`、`webui.enabled` 变更要求重启（`runtime.go:400-409`）。也就是说：**换 egress 文件不需要重启，reload 即可；换二进制才需要重启 app 进程。**

---

## 7. Session B 交付缺口（当前阻断 producer 构建）

`main@beb1731` 的 `instacloud-warp/` 只有 4 个文件：

```text
instacloud-warp/Dockerfile
instacloud-warp/docker-compose.yml
instacloud-warp/endpoints.txt
instacloud-warp/entrypoint.sh
```

但 `Dockerfile` 还引用：

| 缺失文件 | 引用位置 | 后果 |
| --- | --- | --- |
| `instacloud-warp/patches/wgcf-socks5-proxy.patch` | `COPY patches/wgcf-socks5-proxy.patch` | 镜像构建在 COPY 处失败 |
| `instacloud-warp/healthcheck.sh` | `COPY healthcheck.sh`、`HEALTHCHECK CMD` | 镜像构建在 COPY 处失败 |
| `scripts/firewall-allow-center.sh` | `docker-compose.yml` 注释 | 防火墙放行流程无脚本可执行 |

构建验证命令（未执行，需在 producer 节点或本地 Docker 环境）：

```bash
cd instacloud-warp
docker build -t warp-pool-producer:1.0.0 .
# 预期当前失败：COPY failed: file not found ... patches/wgcf-socks5-proxy.patch
```

耦合前必须补齐这 3 个文件，或在 compose 中改为外部挂载。网关侧不依赖它们，但 producer 起不来就没有 `egress-ext.txt`。

---

## 8. egress 文件跨机同步（尚未自动化）

producer 与三台网关不在同一容器/主机。`egress-ext.txt` 在 producer 的 `/data` 命名卷里，当前没有 HTTP 拉取接口。推荐两条路径：

### 8.1 编排脚本拉取（推荐，最小改动）

```bash
# 1) producer 节点：从容器取出文件并 base64（insta CLI 无 upload，argv 传 base64）
docker exec warp-pool-producer base64 -w0 /data/egress-ext.txt > egress.b64

# 2) 网关容器：写入目标文件
B64="$(cat egress.b64)"
insta compute exec --timeout 30 GATEWAY_SERVICE -- \
  sh -c "printf %s '$B64' | base64 -d > /var/lib/opencode2api/egress-ext.txt && wc -l /var/lib/opencode2api/egress-ext.txt"

# 3) reload
insta compute exec --timeout 30 GATEWAY_SERVICE -- \
  sh -c "curl -sS -X POST http://127.0.0.1:8080/api/config/reload -H 'Authorization: Bearer TOKEN'"
```

注意：`insta compute exec` 的 stdin 不可用，文件只能走 base64 argv；Windows 端需 `$env:NODE_OPTIONS='--no-network-family-autoselection'`。

### 8.2 Producer 暴露只读 HTTP（后续增强）

producer 增加一个最小 HTTP 端点（例如 `:18080/egress-ext.txt`，仅允许中心 IP），网关侧用 sidecar/定时任务拉取。当前未实现，列为 Session B 可选增强。

### 8.3 同步频率

- producer 每 900s reroll 一批（4 个），健康检查每 300s 可能触发 reroll。
- 网关的硬冷却也是 900s，因此建议**每 5 分钟同步一次**，最迟不超过 15 分钟；否则网关会继续使用已被 producer 换掉的旧端口（旧端口仍监听时无害，但新 `/64` 无法及时进入池）。
- 同步后必须 reload；reload 会重建静态池并保留 WARP 池。

---

## 9. 部署与回滚

### 9.1 部署顺序（单台灰度）

1. 构建静态二进制：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o opencode2api ./cmd/opencode2api`。
2. 备份旧二进制与 `config.json`。
3. 替换 app 二进制。**替换后必须重启 app 进程**；`compute restart` 被禁止，使用容器内 stop/start 或既有 wrapper 的进程管理方式（按现场流程执行）。
4. 写入 `egress-ext.txt`（第 8 节）。
5. 改 `config.json`：`proxyfile`、`proxies: []`、`direct_fallback: false`、`static_proxy_rate_limit_cooldown_seconds: 900`。
6. `POST /api/config/reload`。
7. 验收：`healthz` 的 `proxies.total` 等于文件行数、`healthy` 全绿；发一条免费模型请求，日志出现 `anonymous_attempt_succeeded`；人为制造额度 429，确认 `free_usage_limit_reached` + 节点 900s 冷却；静态全冷却时出现 `fused_pool_fallback`。
8. 三台重复；每台独立 `egress-ext.txt`（同一份 producer 输出可以复用）。

### 9.2 回滚

1. 恢复旧二进制并重启 app。
2. 恢复 `config.json` 备份，或把 `proxyfile` 置空、`proxies` 恢复原值。
3. `POST /api/config/reload`。
4. 确认 `healthz` 回到部署前状态；WARP 单池仍可用。

---

## 10. 联调验收清单（按顺序执行）

| 步骤 | 命令/动作 | 通过标准 |
| --- | --- | --- |
| 1 | 沙箱 `opencode2api-2` 写入真实 `egress-ext.txt`，reload | `proxies.total=16`、`healthy=16`（或文件实际行数） |
| 2 | 发一条免费模型请求 | HTTP 200；日志 `anonymous_attempt_succeeded` |
| 3 | 连续请求直到额度 429 | `free_usage_limit_reached`；该节点 900s 内不再被选中 |
| 4 | 16 节点全部额度 429 后再请求 | `fused_pool_fallback` static→warp；WARP 返回 200 |
| 5 | 检查 `healthz` | `breaker_open=0`；`warp.active > warp.cooling` |
| 6 | 等 900s 后再次请求 | 静态节点重新入池（`limitedUntil` 到期） |
| 7 | 灰度一台生产 | 同上；确认三台生产源 IP 能连到 producer 端口 |
| 8 | 三台全量 | 三台各自 `fused_pool_fallback` 与 WARP 计数正常 |

沙箱已完成的实测（2026-11-07，`opencode2api-2:8082`）：

- 40/40 HTTP 200；
- 2 次 `fused_pool_fallback` static→warp（`attempts=8`，`error=free tier blocked the egress IP`）；
- 16 次 `free_usage_limit_reached`（`Retry-After` 50029–51214s）；
- 随后 38 次请求 WARP `attempt=1` 成功（`2a09:bac5:c82a:2828::/64`）；
- `breaker_open=0`；`healthz`：`quota_hits=16`、warp 4 active / 3 cooling。

---

## 11. 风险与未验证项

| 项 | 状态 | 说明 |
| --- | --- | --- |
| 真实带认证 SOCKS5 | 未验证 | 沙箱用明文 socks5，未连过 `warpuser:WarpPass2026` 的真实 producer |
| Producer 构建 | 当前失败 | 缺 `patches/wgcf-socks5-proxy.patch`、`healthcheck.sh`、防火墙脚本 |
| egress 文件同步 | 未自动化 | 需编排脚本或 producer HTTP 端点 |
| JP 节点放行三台生产源 IP | 未验证 | 需在 producer 防火墙放行 SIN/IAD/FRA 出口 |
| 生产二进制 commit | 未固定 | 生产镜像 sha 已记录，但 app commit 未注入；替换前需比对 |
| 15m 冷却 vs 900s reroll | 已对齐 | 但 reroll 是分批（4/批），实际换池有错峰；网关按节点独立计时 |
| WARP 容量 2x 规则 | 保留 | WARP 池容量 ≥ 静态池 2 倍时优先 WARP；如希望静态池更多参与，调 `warp.pool_size` 或改 `fusedOrder` |
| IPv4 出口 | 网关不校验 | 由 producer 的纯 IPv6 渲染保证；新增 producer 必须保留剥离逻辑 |
| `#pool` 误用 | 已防 | 契约明确禁止；网关对 `#pool` 不做硬冷却，误用会导致反复 429 |

---

## 12. 变更文件清单

| 文件 | 类型 | 说明 |
| --- | --- | --- |
| `internal/gateway/fusion.go` | 新增 | 双池调度、跨池 fallback、drain+close |
| `internal/gateway/pool_test.go` | 新增 | 3 个边界回归测试 |
| `internal/gateway/pool.go` | 修改 | `laneBreaker` 拆分、`limitedUntil`、`MarkFailure`、`AvailableExits`、`HasExternal` |
| `internal/gateway/gateway.go` | 修改 | `warpLane` 字段与初始化、`rateLimitCooldown` |
| `internal/gateway/upstream.go` | 修改 | 匿名路由、静态走法改名、额度 429 记罚 |
| `internal/gateway/warp.go` | 修改 | `warpAnonymousWalk(..., allowDirect)`、5 处 breaker 换 `warpLane` |
| `internal/config/config.go` | 修改 | `StaticProxyRateLimitCooldownSeconds` 与校验 |
| `config.example.json` | 修改 | 新增 `static_proxy_rate_limit_cooldown_seconds: 900` |
| `docs/warp-pool-fusion-plan.zh.md` | 新增 | 设计稿 |
| `docs/warp-fusion-handoff.zh.md` | 新增 | 本报告 |

`git diff --stat`（融合代码）：6 个已跟踪文件 284 insertions / 98 deletions，另加 2 个新文件。

---

## 13. 给 Session B 的接口边界

| 归属 | 责任 |
| --- | --- |
| Session A（网关） | 双池调度、跨池 fallback、静态 429 冷却、WARP/静态熔断隔离、配置 reload、healthz/日志 |
| Session B（producer） | WARP 注册、wireproxy SOCKS5、端口 1081–1096、`egress-ext.txt`、去重/重注册、健康检查、防火墙 |
| 共享契约 | 文件行格式、认证、纯 IPv6、`/48` 不重叠、无 `#pool`、只写健康节点、900s reroll 与 900s 冷却对齐 |
| 待补 | producer 3 个缺失文件；egress 文件同步；中心 IP 放行；真实认证 SOCKS5 联调 |

耦合时只要守住第 4 节的 6 条硬性约束，网关侧无需再改代码；若 producer 改为动态端口或 `#pool` 语义，必须先改网关的 `AvailableExits`/`MarkFailure`，否则硬冷却与统计会失真。
