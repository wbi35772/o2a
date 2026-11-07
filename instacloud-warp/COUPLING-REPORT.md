# instacloud-warp × opencode2api 耦合报告 v3

> 版本：v3（双基线：旧 `main@58f56676` / 新 `origin/warp-fusion@f7506bd`）
> 生产者：`warp-pool-producer`，分支 `instacloud-warp`（15 个文件，构建/实测通过）
> 网关源码基准：旧 `main@58f56676`；新 `origin/warp-fusion@f7506bd`（含 `docs/warp-fusion-handoff.zh.md`）
> 结论一句话：**旧 main 下必须 `warp.enabled=false` 才能用本池；warp-fusion 下用 `warp.enabled=true + warp.direct_fallback=false + proxies=[] + static_proxy_rate_limit_cooldown_seconds=900` 实现「本地 WARP 池 + 外部静态 SOCKS5 池」双池融合，本池作为外部池并行生效。**

---

## 0. 一页结论（TL;DR）

| 项 | 旧 main@58f56676 | 新 warp-fusion@f7506bd |
| --- | --- | --- |
| 两套 WARP 能否同时生效 | 不能，免费车道二选一 | 能，Fused Mode 双池并行 |
| 用本池的开关 | `warp.enabled=false` + `proxyfile` | `warp.enabled=true` + `warp.direct_fallback=false` + `proxies=[]` + `proxyfile` |
| 内置 WARP 池 | 开 `warp.enabled=true` 时独占免费车道 | 与外部池按 `fusedOrder` 交替/择优 |
| 静态 429 冷却 | `failure_cooldown_seconds` 指数冷却（15/30/60/120s） | 新增 `static_proxy_rate_limit_cooldown_seconds=900`，额度 429 硬冷却 900s（`limitedUntil`） |
| `free_tier_block_seconds` | 旧 main 中零调用死代码 | 不再承担 900s 冷却；填 `0` 即可 |
| 清单里能不能有 `#` | 不能 | 不能 |
| 网关自动重读 proxyfile | 不会（仅 Reload） | 不会（仅 Reload） |
| 换血后要不要重载 | 不需要（端口固定） | 不需要（端口固定） |
| 每请求静态池预算 | `min(16, 8)=8` | 同左，`max_anonymous_exits=0` 用默认 8 |
| 本池定位 | 替代内置 WARP 的外部出口 | 与内置 WARP 并行的外部出口，补足容量、分摊 429 |

---

## 1. 目标与范围

本报告回答四件事：

1. 本池如何接入旧 main 与 warp-fusion 两种网关二进制，**改哪些配置、不改哪些代码**；
2. 两种二进制对静态 SOCKS5 出口的真实处理逻辑（源码级），以及由此决定的行为边界；
3. 双池融合模式（Fused Mode）的配置契约、调度顺序、冷却语义与回滚；
4. 生产者侧「固定端口 + 出口漂移」设计如何消除跨机同步与定时 Reload。

不覆盖：网关业务代码修改（红线）、IPv4 出口（红线）、与存量 SIN/IAD/FRA `/48` 家族重叠（红线）。

---

## 2. 两种二进制，两种接入模式

### 2.1 旧 main@58f56676：互斥模式

- `WarpConfig` 注释写明：启用后，**匿名车道的免费模型流量与免费 Zen-key 回退**改走用户态 WireGuard 隧道，"instead of any configured proxy"（`internal/config/config.go:68-74`）。
- 匿名请求入口先判断 WARP：

  ```go
  // internal/gateway/upstream.go:359-361
  if g.usesWARP(route.ID, config.TierZen, true) {
      return g.doWarpAnonymousUpstream(ctx, route, bodies, ids, attemptOffset)
  }
  ```

- `usesWARP` 的条件是「WARP 池非空 + 免费模型 + 匿名/免费车道」（`internal/gateway/warp.go:36-39`）。
- README 也明确：`warp.enabled` 把免费车道的全部流量（匿名车道、免费模型经 Zen key 的回退、免费模型可用性探测、匿名目录刷新）搬到 WARP 隧道，"Those requests use no proxy"（`README.md:249`）；关掉 WARP 才恢复代理路由（`README.md:255`）。

**结论：旧 main 下 `warp.enabled=true` 时，`proxyfile` 里的 16 个 SOCKS5 出口对免费模型完全不可见。用本池必须 `warp.enabled=false`。**

### 2.2 warp-fusion@f7506bd：双池融合模式

warp-fusion 解除了上述互斥，新增 Fused Mode：

- `fusedActive` = 该模型启用 WARP **且** 匿名池里存在非 `direct` 的真实外部出口（`fusion.go:30` 附近）。
- `fusedOrder` 决定先走哪一池：
  - 一池容量为 0 → 让给另一池；
  - 一池可用容量 ≥ 另一池 2 倍 → 容量大者优先；
  - 否则两池交替（`fusion.go:54` 附近）。
- `doFusedAnonymousUpstream` 先走第一池，失败后走第二池；每次失败会 drain + close 响应体；**确定性客户端拒绝或 ctx 取消不触发跨池回退**；两池都额度耗尽 → `errFreeTierIPBlocked`（`fusion.go:81/120` 附近）。
- 外部静态池仍走原来的 `doStaticAnonymousUpstream`；本地 WARP 池走 `doWarpAnonymousUpstream`。两套 breaker 独立：`warpLane` 的熔断不影响 `anonymous.breaker`。
- `HasExternal` 只统计非 `direct` 的真实出口；`AvailableExits` 会排除 `direct`、被免费层标记、不健康、软冷却、硬冷却（`limitedUntil`）的节点；`#pool` 条目始终计入可用数（`pool.go:151-156/154` 附近）。

**结论：warp-fusion 下 `warp.enabled=true` 不再绕过 `proxyfile`；本池作为「外部静态 SOCKS5 池」与网关内置 WARP 池并行。**

### 2.3 模式矩阵

| 模式 | 二进制 | `warp.enabled` | `proxyfile` | 免费车道实际出口 | 适用场景 |
| --- | --- | --- | --- | --- | --- |
| A：纯静态池 | 旧 main / warp-fusion 均可 | `false`（旧 main 必须；fusion 也可） | 16 行 | 本池 16 个境外 `/64` | 旧 main 试点、内置 WARP 不可用 |
| B：纯内置 WARP | 旧 main / warp-fusion 均可 | `true` | 保留但不用 | 网关自建 WARP 池（1–32 个） | 中心主机 UDP 可达、有可路由 `/64` |
| C：双池融合 | **仅 warp-fusion** | `true` + `direct_fallback=false` | 16 行 | 内置 WARP 池 + 本池并行 | 需要更大出口容量、分摊 429、灰度切换 |
| D：旧 main 误配 | 旧 main | `true` | 16 行 | 只有内置 WARP | 本池被静默绕过，禁止 |

### 2.4 推荐路径

- **旧 main 环境**：模式 A，`warp.enabled=false`。
- **warp-fusion 环境**：模式 C，`warp.enabled=true`、`warp.direct_fallback=false`、`proxies=[]`、`proxyfile` 指向 16 行清单、`static_proxy_rate_limit_cooldown_seconds=900`。
- **回滚**：把 `warp.enabled` 改回 `false`（模式 A）或 `true` 且移除 `proxyfile`（模式 B），Reload 即可；两套模式都不需要改代码。

---

## 3. 网关代理模型（源码级）

### 3.1 proxyfile 的解析规则（两版一致）

`internal/config/proxy.go`：

| 规则 | 位置 |
| --- | --- |
| 每行一个代理；空行丢弃；行首 BOM 剥除；单行上限 1 MiB | `proxy.go:68-89` |
| 注释：`#`、`;`、`//` 仅在**行首**或**空格/制表符之后**才生效；URL 内部的 `//` 与 `#pool` 片段保留 | `proxy.go:94-104` |
| 相对路径基于配置文件所在目录解析 | `proxy.go:42-55` |
| 去重：按「清洗后的拨号 URL + 池标记」判重，保留首次出现的拼写 | `proxy.go:106-137` |
| 列表为空时回退为 `direct` | `proxy.go:57-60` |
| 配置内 `proxies` 数组在前，proxyfile 条目在后，合并去重 | `proxy.go:42-61` |

对我们的含义：16 行 `socks5://user:pass@IP:1081..1096` 端口各不相同，全部保留；文件里不要出现任何 `#` 字符（我们的导出脚本已强制校验）。

### 3.2 静态条目 vs `#pool` 动态池（两版一致）

`ParseProxyEntry` 支持 `pool+` 前缀或 `#pool` 片段（`config/proxy.go:15-36`）。两者行为差异是耦合的关键：

| 行为 | 静态条目（我们的清单） | `#pool` 条目 |
| --- | --- | --- |
| 失败冷却 | 有（`MarkFailure` 生效） | **无**，直接 `return`（`gateway/pool.go:395-398`） |
| 连接复用 | 允许 keep-alive / HTTP/2 | 禁用 keep-alive、强制 HTTP/1.1，每次尝试新建 TCP+CONNECT（`pool.go:450-466`） |
| 抽取方式 | 每个请求最多访问一次 | 每次尝试都重新抽取，`retry.max_attempts` 次（`upstream.go:346-349`、`config.go:241-246`） |
| 池内优先级 | 正常轮转 | 一旦池内存在 `#pool`，第一轮之后只循环 `#pool` 条目（`pool.go:313-322`） |
| 健康检查 | 不健康的会被探测恢复（`pool.go:510+`） | 跳过 |

**结论：本池必须使用纯静态条目。** 一旦误加 `#pool`，网关会把它当无限旋转代理：不冷却、每次新建连接、优先循环——看起来「总在换出口」，实际绕过了我们精心设计的 `/64` 去重与 900s 换血语义。

### 3.3 旧 main 的冷却与「死代码」`free_tier_block_seconds`

- `MarkFailure` 触发条件：传输错误，或 401/403/429/5xx；其它响应不惩罚（`pool.go:395-401`）。
- 冷却时长：`failure_cooldown_seconds × 2^(连续失败数-1)`，上限 8×；`Retry-After` 可延长，但同样封顶 8×（`pool.go:402-409`）。默认基数 15s ⇒ 15/30/60/120s。
- 成功一次即清零失败计数与冷却（`pool.go:348-354`）。
- `free_tier_block_seconds` 在旧 main 中**没有调用点**（会话 A 已核实为零调用死代码）。不要依赖它做 900s 额度冷却；旧 main 的 429 只会走上面的指数冷却，且会被超长 `Retry-After` 封顶到 8×基数。

### 3.4 warp-fusion 的 900s 硬冷却与融合调度

新增 `performance.static_proxy_rate_limit_cooldown_seconds`（推荐 `900`）：

| 场景 | 行为 |
| --- | --- |
| 静态代理返回 **额度 429**（免费层配额耗尽） | 写入 `limitedUntil = now + 900s`，**忽略上游超长 `Retry-After`**；该节点在 `AvailableExits` 中被排除，直到硬冷却到期 |
| 静态代理返回 **瞬时 429**（普通限流） | 采用上游较短的 `Retry-After`；不进入 900s 硬冷却 |
| 401/403/5xx/传输错误 | 仍走 `failure_cooldown_seconds` 指数冷却；429 不计入 lane failure/breaker |
| 硬冷却到期 | 节点重新进入 `AvailableExits`；此时生产者 900s 换血通常已给它新 `/64` |

融合调度补充：

- `fusedOrder` 不是固定「先本地后外部」：一池为空则让位；一池容量 ≥ 另一池 2 倍则优先；否则交替。
- 跨池回退只发生在非 2xx/可重试失败；确定性客户端拒绝（如 400）或 ctx 取消不会把请求再打到另一池，避免重复计费/重复生成。
- 两池都额度耗尽才返回 `errFreeTierIPBlocked`；单池耗尽不会直接失败。
- `warp.direct_fallback=false` 必须显式设置：否则 WARP 池失败时可能回退到直连，绕过本池与内置池的出口身份。

### 3.5 匿名请求的尝试预算（两版一致）

```go
// internal/gateway/upstream.go:379-382（旧 main）
limit := min(g.anonymous.Len(), g.maxAnonymousExits())
if g.anonymous.HasPool() && g.cfg.Retry.MaxAttempts > limit {
    limit = g.cfg.Retry.MaxAttempts
}
```

- 静态池：`limit = min(16, 8) = 8`，每个出口最多试一次。
- `retry.max_attempts`（默认 3）**不会**扩大静态池的遍历范围；它只影响 `#pool` 与其他层级。
- 想让单个请求走完 16 个出口，调 `max_anonymous_exits = 16`；代价是全池故障时请求耗时线性增长。
- warp-fusion 的 handoff 推荐 `max_anonymous_exits: 0`（使用内置默认 8），与本池 16 端口形成 2× 冗余。

### 3.6 配置重载（两版一致）

- `Reload()` = 重新读取配置文件（含 proxyfile）并应用（`gateway/runtime.go:428-442`），触发点只有管理端 API（`internal/admin/server.go:268`，WebUI 保存/重载走这里）。
- **没有 fsnotify / 定时重读**。
- 我们的清单 URL 恒定（`NODE_PUBLIC_IP:1081..1096` 不变，换的是端口后面的 WARP 隧道与 `/64`），所以**换血、健康修复都不需要网关重载**；只有节点公网 IP 或端口段变化时才需要 Reload。

### 3.7 键控层级与代理的关系

`transportPool(cfg.RuntimeProxies())` 为每个运行时代理建一对 HTTP 客户端（普通 + Bun 伪装），Zen/Go 键池与匿名池共用这些 transport（`gateway/gateway.go:98-139`、`pool.go:440-498`）。键控层级是否实际绑定到某个代理由键的绑定/轮转策略决定；本报告不展开，但要注意：模式 B 下 `proxyfile` 并非「完全无副作用」，键控层级仍可能用到它。

---

## 4. 生产者侧实现（与网关耦合相关的部分）

### 4.1 流水线

```
register(n1 直连；n2+ 经上一条 SOCKS5 链式注册)
  → wgcf generate → render wireproxy.conf（纯 IPv6、随机 Endpoint、[Socks5] 账密）
  → wireproxy -n -c 配置自检 → 启动监听 :PORT
  → curl --socks5-hostname https://opencode.ai/cdn-cgi/trace 取 ip= 前 4 段
  → /64 合法性（2a09:）→ 禁区 /48 校验 → 池内去重
  → status.tsv + egress-ext.txt（原子重写）
  → 900s 分批换血（4×4，批间 5s）
  → 300s 健康检查（换 Endpoint → 必要时重注册）
```

### 4.2 关键设计点（为什么这样和网关对齐）

| 设计 | 原因 |
| --- | --- |
| 端口固定、出口漂移 | 网关只在 Reload 时读清单；URL 恒定意味着换血零重载、零跨机同步 |
| 纯 IPv6 `AllowedIPs = ::/0` | 网关看到的是 `2a09:*` `/64`，配额身份按 `/64` 计 |
| `/64` 去重 | 同一 `/64` 在网关侧共享免费额度，去重直接决定池的有效大小 |
| 900s 换血 | 对齐 warp-fusion 的 `static_proxy_rate_limit_cooldown_seconds=900`：硬冷却到期时出口已换新 |
| 4×4 分批 | 每批只动 4 个端口，任意时刻至少 12 个端口在线；网关单请求预算 8 个，仍有足够候选 |
| 链式注册 | Cloudflare 对注册源 IP 限速；链式让每个账号从不同 `/64` 注册 |
| 账号级故障自动重注册 | 网关侧只会看到「某端口长时间不健康」，节点自愈比人工快 |

### 4.3 固定端口 = 零同步（对 handoff 第 8 节的修正）

`docs/warp-fusion-handoff.zh.md` 第 8 节建议「每 5 分钟同步一次 egress 清单」。这对**端口会变化**的生产者成立，但对本生产者不成立：

- 本生产者的公网入口恒为 `NODE_PUBLIC_IP:1081..1096`；900s 换血只替换端口后面的 WARP 会话与 `/64`，**端口不变**。
- 网关的 `proxyfile` 内容是「主机 + 端口」，不含 `/64`；因此换血后网关无需重读文件，也无需定时同步。
- 只有以下两种情况需要重新同步/Reload：
  1. 节点公网 IP 变化；
  2. 端口段变化（例如扩容到 `2081–2096`）。
- 这意味着网关侧不需要 cron、不需要共享存储、不需要 producer→gateway 的推送通道；清单是静态文件，节点侧自治换血。

### 4.4 状态与文件

| 路径 | 内容 |
| --- | --- |
| `/data/egress-ext.txt` | 16 行清单，原子 mv 重写 |
| `/data/status.tsv` | `port pid endpoint prefix64 loc state epoch` |
| `/data/instances/nXX/` | wgcf 账号、profile、wireproxy.conf、日志 |
| 容器日志 | 首轮完成打印可复制清单块；换血/健康/恢复均有结构化日志 |

---

## 5. 接口契约 v3

### 5.1 清单格式（硬性）

```
socks5://warpuser:WarpPass2026@NODE_PUBLIC_IP:1081
socks5://warpuser:WarpPass2026@NODE_PUBLIC_IP:1082
...
socks5://warpuser:WarpPass2026@NODE_PUBLIC_IP:1096
```

- 恰好 `POOL_SIZE` 行（默认 16），UTF-8，无 BOM，LF 或 CRLF 均可。
- **任何位置不得出现 `#` 字符**（包括行尾注释）。
- `socks5://` 与 `socks5h://` 在网关侧等价：Go 的 `http.ProxyURL` 对主机名目标把域名交给代理解析（`pool.go:471-481`），因此 DNS 在节点侧完成——这对纯 IPv6 隧道是必要条件。
- 账号密码可含 URL 保留字符时需百分号编码；当前 `WarpPass2026` 无需编码。

### 5.2 语义

- **URL 恒定**：`NODE_PUBLIC_IP:PORT` 不随换血变化；变化只发生在端口后面的 WARP 会话与 `/64`。
- **可用性抖动**：单端口重启约 1–2s；4×4 分批时每批 4 个端口短暂不可用。
- **原子性**：导出脚本先写临时文件再 `mv`，网关任何时刻读到的都是完整文件。
- **池大小**：默认 16；`POOL_SIZE` 与 `INSTANCE_COUNT` 等价。

### 5.3 文件落点

- 网关配置 `proxyfile` 指向容器内路径（如 `/var/lib/opencode2api/egress-ext.txt`），相对路径基于配置文件目录解析。
- 建议 `0640`，属主为网关运行用户；文件内含 SOCKS 账密，不要进镜像层。
- warp-fusion 的 `proxies` 必须为空数组 `[]`；外部池只通过 `proxyfile` 进入。

---

## 6. 容量与时序

### 6.1 单请求视角

- 候选出口：16；单请求最多尝试：8。
- 池健康时：第 1 个出口成功率极高，8 个预算 ≈ 2 倍冗余。
- 单出口被 429：旧 main 进入 15s 指数冷却；warp-fusion 额度 429 进入 900s 硬冷却，瞬时 429 采用较短 `Retry-After`。
- 单出口被免费层标记：旧 main 的 `free_tier_block_seconds` 是死代码；warp-fusion 由 900s 硬冷却承担；900s 后换血已给它新 `/64`。

### 6.2 时间线（典型）

```
t=0        冷启动，n01 直连注册（~5-15s）
t=15-60s   后续实例链式注册 + 探活，池子逐步到 15-16
t≈60-180s  首轮去重完成，日志打印 16/16
t=900s     第一批 4 个换血（每个 1-2s）
t=905s     第二批；此时 12 个端口在线
t=915s     四批完成，全部新出口
t=1200s    健康检查（300s 周期）收敛任何撞车
```

### 6.3 池容量现实

沙箱机房实测可用唯一 `/64` 约 20 个，16 个已接近上限；换血瞬间出现 14–15/16 属正常，5 分钟内健康检查收敛。网关侧应把该窗口视为正常抖动（单请求 8 个预算仍覆盖）。

> 沙箱实测补充：健康检查每 300s 只对每个端口重掷一次；若换 Endpoint 仍返回重复 `/64`（账号粘性），重复可能跨轮持续。此时手工删除该实例账号并重启可换到新前缀池；正式节点若长期 <14/16，建议扩容第二个节点而不是继续压榨单机房。

---

## 7. 故障矩阵

| 现象 | 判定 | 处置 |
| --- | --- | --- |
| 节点容器起不来 | 镜像/权限/端口占用 | `docker logs`；确认 `network_mode: host` 与端口未被占用 |
| n01 注册 429 | 节点 IP 被 Cloudflare 限速 | 等待 10 分钟或换节点 IP；已注册账号会复用 |
| 后续实例注册失败 | 链式代理不可用 | 日志看 `register-failed`；确认上一条端口探活成功；自动回退直连 |
| 某端口一直 `probe-failed` | 隧道握手失败/Endpoint 被墙 | 自动换 Endpoint（4 次）；仍失败则重注册 |
| `/64` 重复 | 机房出口池小 | 换 Endpoint；4 次仍撞车留给下一轮换血；必要时删账号重注册 |
| 中心 `curl` 超时 | 防火墙/安全组 | 节点执行 `firewall-allow-center.sh`；云安全组放行 1081–1096 |
| 中心 `curl` 返回 `socks5: authentication failed` | 账密不符 | 核对 `.env` 与清单 |
| 网关日志 `no_healthy_proxies` | 静态池全不健康 | 先确认模式正确（旧 main `warp.enabled=false`；fusion `warp.enabled=true` + `direct_fallback=false`）；再看节点 `status.tsv` |
| 旧 main 免费车道仍走中心 WARP | `warp.enabled=true` | 改 `false` 并 Reload/重启 |
| warp-fusion 未融合 | `warp.enabled=false` 或 `direct_fallback=true` 或 `proxies` 非空 | 按 2.2 的配置块修正并 Reload |
| warp-fusion 只走本地 WARP | 外部池 `HasExternal=false`（清单为空/全是 `direct`） | 检查 `proxyfile` 路径与 16 行格式 |
| 网关改了节点 IP 后不生效 | proxyfile 未重读 | WebUI 保存/Reload 或重启容器 |
| 出口 `loc` 不对 | 节点路由/机房 | 这是节点属性；换节点国家 |
| 出口是 `104.28.x.x` | 隧道退化为 IPv4 | 检查 `AllowedIPs` 是否被改回 `0.0.0.0/0`；重渲染配置 |

---

## 8. 安全模型

- **双层准入**：SOCKS5 账密（`warpuser` / `WarpPass2026`）+ 节点防火墙源 IP 白名单（SIN `13.229.234.225`、IAD `18.212.22.159`、FRA `3.79.179.114`）。
- 容器 `cap_drop: ALL`、`network_mode: host`（让 INPUT 白名单真正生效），不挂 docker.sock，不 privileged。
- `/data` 含 WireGuard 私钥：备份加密、权限收紧；`docker compose down -v` 会销毁账号池。
- 建议：上线前更换默认 `WarpPass2026`；`.env` 不入库；清单文件 `0640`。
- 临时联调（如 bore 中继）会绕开节点防火墙白名单，只保留 SOCKS5 账密；仅用于测试，不要用于正式流量。

---

## 9. 部署 Runbook

### 9.1 节点

```bash
scp -r warp-pool-producer root@NODE_PUBLIC_IP:/opt/
cd /opt/warp-pool-producer
cp .env.example .env
# 编辑 .env：EXTERNAL_HOST=NODE_PUBLIC_IP
docker compose up -d --build
docker logs -f warp-pool-producer   # 等到 Active Unique /64 Pool Size: 16 / 16
sudo bash scripts/firewall-allow-center.sh
bash scripts/export-egress.sh --host NODE_PUBLIC_IP --verify
```

无公网入站时（PaaS/NAT），跳过 `firewall-allow-center.sh`，用 `scripts/bore-supervisor.sh` 把 `127.0.0.1:1081..1096` 映射到 `BORE_SERVER:BORE_PORT_START..`，并把清单换成中继地址；详见 README「无公网入站：bore 中继」。

### 9.2 旧 main（模式 A）

1. 把 16 行清单写入中心容器 `proxyfile` 指向的路径（无 `#`）。
2. 网关配置：

   ```json
   {
     "warp": { "enabled": false },
     "proxyfile": "/var/lib/opencode2api/egress-ext.txt",
     "performance": {
       "max_anonymous_exits": 8,
       "failure_cooldown_seconds": 15
     }
   }
   ```

3. 保存/Reload（管理端 API 或重启容器）。
4. 验证：中心容器内 `curl --socks5-hostname` 打 `cdn-cgi/trace`，`ip=` 应为 `2a09:*`。

### 9.3 warp-fusion（模式 C：双池融合）

1. 把 16 行清单写入 `proxyfile` 指向的路径（无 `#`）。
2. 网关配置：

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

   - `proxies: []`：外部池只从 `proxyfile` 进入。
   - `warp.pool_size`：按机器规格；沙箱实测 4 足够，中心机可调 8–16。
   - `max_anonymous_exits: 0`：使用内置默认 8；本池 16 端口 = 2× 冗余。
   - `free_tier_block_seconds: 0`：旧键，fusion 不再依赖；填 0 避免误解。
3. 保存/Reload。
4. 验证融合生效：
   - `/healthz` 的 `warp` 块应显示本地 WARP 活跃；
   - 网关日志应出现 fused 选路/回退记录；
   - 抽样请求的出口应同时出现 `2a09:*`（本池）与本地 WARP 的出口段。

### 9.4 回滚

- 切回旧 main 模式 A：`warp.enabled = false` → Reload。
- 切回纯内置 WARP（模式 B）：`warp.enabled = true`、移除 `proxyfile` 引用 → Reload。
- 完全移除本池：删 `proxyfile` 引用 → Reload；节点容器可停。
- 节点侧：`docker compose down`（保留 `/data` 卷）；恢复时 `up -d`。

---

## 10. 验收与证据

### 10.1 沙箱实测（已通过）

| 检查 | 结果 |
| --- | --- |
| 镜像构建 | 60.6 MB；wgcf 补丁编译通过；wireproxy 1.0.8-dev |
| 冷启动 | 15/16，健康检查后 16/16 |
| 900s 满池换血 | 4×4 分批；换血后 14–15/16，5 分钟内收敛 |
| 账号级故障恢复 | 破坏私钥后自动重注册，~30s 恢复 |
| 导出验收 | `16/16 unique IPv6 /64 prefixes, 0 failure(s)`，exit 0 |
| 清单格式 | 16 行、0 个 `#`、账号行数 = POOL_SIZE |
| 禁区 `/48` | 全程 0 重叠 |

### 10.2 中心侧验收命令

```bash
# 逐端口抽样
for p in 1081 1088 1096; do
  curl -sS -m 10 --socks5-hostname "warpuser:WarpPass2026@NODE_PUBLIC_IP:$p" \
    https://opencode.ai/cdn-cgi/trace | grep -E '^(ip|loc)='
done
```

期望：`ip=2a09:bac...`（IPv6），`loc=JP`（或目标国家）。任何 `ip=104.28...` 视为失败。

### 10.3 本次沙箱联调入口（临时）

沙箱 `35.91.188.19` 是共享 NAT，1081–1096 不能从公网直连；本次用 bore 固定端口映射：

| 本地端口 | 公网入口 | 实测 `/64` |
| --- | --- | --- |
| 1081 | bore.pub:40001 | `2a09:bac1:76a0:de10` |
| 1082 | bore.pub:40002 | `2a09:bac5:b1bc:4aa` |
| 1083 | bore.pub:40003 | `2a09:bac5:b1bb:26be` |
| 1084 | bore.pub:40004 | `2a09:bac5:b1bf:4aa` |
| 1085 | bore.pub:40005 | `2a09:bac5:b1bf:26be` |
| 1086 | bore.pub:40006 | `2a09:bac5:b1b9:26be` |
| 1087 | bore.pub:40007 | `2a09:bac5:b1bc:26be` |
| 1088 | bore.pub:40008 | `2a09:bac1:76c0:de10` |
| 1089 | bore.pub:40009 | `2a09:bac5:b1bb:4aa` |
| 1090 | bore.pub:40010 | `2a09:bac5:b1bd:4aa` |
| 1091 | bore.pub:40011 | `2a09:bac5:b1ba:26be` |
| 1092 | bore.pub:40012 | `2a09:bac1:76e0:de10` |
| 1093 | bore.pub:40013 | `2a09:bac1:7680:de10` |
| 1094 | bore.pub:40014 | `2a09:bac5:b1be:26be` |
| 1095 | bore.pub:40015 | `2a09:bac5:b1b9:4aa` |
| 1096 | bore.pub:40016 | `2a09:bac5:b1b8:26be` |

- 认证：`warpuser` / `WarpPass2026`。
- 全部 16 个入口已实测返回 `2a09:*`、`loc=US`，且 `/64` 两两不同。
- **临时性**：bore.pub 是公共中继，沙箱销毁或 bore 进程退出即失效；正式节点请用 `NODE_PUBLIC_IP:1081..1096` + 防火墙白名单。

---

## 11. 监控与告警建议

| 信号 | 来源 | 建议阈值 |
| --- | --- | --- |
| 池大小 | 节点日志 `Active Unique /64 Pool Size: N / 16` | <14 持续 10 分钟告警 |
| 端口状态 | `/data/status.tsv` 的 `state` | 非 `ok` 数量 ≥3 告警 |
| 出口国家 | 抽样 `loc=` | 与节点国家不符告警 |
| 网关代理健康 | WebUI Resources / `ResourceSnapshot.Proxies` | 不健康 ≥4 告警 |
| 免费层 429 比例 | 网关日志 | 持续升高说明 `/64` 池被过度使用 |
| 硬冷却数量（fusion） | `limitedUntil` 未到期节点数 | ≥4 且持续 5 分钟告警 |
| WARP 状态（模式 B/C） | `/healthz` 的 `warp` 块 | `active=0` 且 enabled ⇒ 503，立即处理 |

---

## 12. 对接决策清单（逐条给推荐值）

| # | 决策 | 旧 main | warp-fusion | 理由 |
| --- | --- | --- | --- | --- |
| 1 | `warp.enabled` | `false` | `true` | 旧 main 开 WARP 会绕过本池；fusion 才支持双池 |
| 2 | `warp.direct_fallback` | 不适用 | `false` | 防止 WARP 失败回退直连 |
| 3 | `proxies` | 不适用 | `[]` | 外部池只从 proxyfile 进入 |
| 4 | `proxyfile` | 16 行 | 16 行 | 静态条目，无 `#` |
| 5 | `max_anonymous_exits` | `8` | `0`（=8） | 16 端口 = 2× 冗余 |
| 6 | `failure_cooldown_seconds` | `15` | `15` | 瞬时失败指数冷却 |
| 7 | `free_tier_block_seconds` | 忽略（死代码） | `0` | fusion 用新键做 900s 冷却 |
| 8 | `static_proxy_rate_limit_cooldown_seconds` | 不适用 | `900` | 额度 429 硬冷却，与换血对齐 |
| 9 | 重载流程 | 仅节点 IP/端口变化时 Reload | 同左 | URL 恒定，换血无需重载 |
| 10 | 节点数量 | ≥2（不同国家） | ≥2 | 单节点故障时池不归零 |
| 11 | 第二节点端口段 | `2081–2096` | 同左 | 清单可直接合并，网关零改动 |
| 12 | 监控 | 池大小 + 端口状态 + `loc` 抽样 | 加硬冷却计数 | 见第 11 节 |

---

## 13. 附录

### 13.1 环境变量（生产者）

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `EXTERNAL_HOST` | 自动探测 | 写入清单的公网 IPv4，建议显式设置 |
| `START_PORT` | `1081` | 首个 SOCKS5 端口 |
| `POOL_SIZE` / `INSTANCE_COUNT` | `16` | 实例数 |
| `SOCKS_USER` / `SOCKS_PASS` | `warpuser` / `WarpPass2026` | SOCKS5 账密 |
| `REROLL_INTERVAL` | `900` | 全量换血周期（秒） |
| `REROLL_BATCH` | `4` | 每批换血实例数 |
| `DEDUP_RETRIES` | `4` | 单端口去重最大重试 |
| `REGISTER_RETRIES` | `3` | 单账号注册最大尝试 |
| `WGCF_KEEPALIVE` | `25` | PersistentKeepalive |
| `PROBE_URL` | `https://opencode.ai/cdn-cgi/trace` | 探活地址 |
| `PROBE_TIMEOUT` | `6` | 探活超时（秒） |
| `HEALTH_INTERVAL` | `300` | 健康检查周期（秒） |
| `DENY_48_PREFIXES` | 见 `.env.example` | 禁区 `/48` 家族 |

### 13.2 Endpoint 池

8 个 Anycast 地址 × 4 端口（2408/500/1701/4500）= 32 组合：

`162.159.192.1/2`、`162.159.195.1/2`、`188.114.96.1`–`188.114.99.1`。

`162.159.193.1` 已移除（同账号全端口握手失败，其余 8 个地址通过）。

### 13.3 旧 main 源码索引

| 主题 | 位置 |
| --- | --- |
| WARP 配置与校验 | `internal/config/config.go:68-117` |
| 默认值（冷却 15s、尝试 3 次） | `internal/config/config.go:292-302` |
| proxyfile 解析 | `internal/config/proxy.go:15-137` |
| 匿名池调度 | `internal/gateway/pool.go:276-346` |
| 冷却/屏蔽 | `internal/gateway/pool.go:57-64, 390-410` |
| transport 构建 | `internal/gateway/pool.go:440-498` |
| 匿名请求预算 | `internal/gateway/upstream.go:331-382` |
| WARP 分支 | `internal/gateway/upstream.go:359-361`、`internal/gateway/warp.go:36-39` |
| Reload | `internal/gateway/runtime.go:428-442`、`internal/admin/server.go:268` |
| 行为文档 | `README.md:233, 249-255, 305-308, 385-396` |

### 13.4 warp-fusion 源码索引（来自 handoff）

| 主题 | 位置 |
| --- | --- |
| Fused 激活/顺序/执行 | `internal/gateway/fusion.go:30, 54, 81, 120` |
| 外部池可用数/硬冷却 | `internal/gateway/pool.go:151-156, 154, 389-440, 505` |
| 429 与 limitedUntil | `internal/gateway/pool.go:456, 467, 554, 571` |
| 新配置键与校验 | `internal/config/config.go:286-291, 429-430` |
| 融合请求入口 | `internal/gateway/upstream.go:349, 514-520` |
| WARP 池状态 | `internal/gateway/warp.go:326, 473` |
| 网关装配 | `internal/gateway/gateway.go:84, 152` |
| 滥用/配额信号 | `internal/gateway/abuse.go:101` |

### 13.5 版本锚点

- 生产者：`instacloud-warp` 分支，15 个文件；本报告提交后以该分支 HEAD 为准
- wgcf `v2.3.1` + `patches/wgcf-socks5-proxy.patch`（纯标准库 SOCKS5）
- wireproxy `github.com/windtf/wireproxy@35cce290db2f32d5e4316bd54102186f486d0af4`
- 旧网关源码：`main@58f56676`
- 新网关源码：`origin/warp-fusion@f7506bd`

---

## 14. 与 handoff 文档的差异与修正

`docs/warp-fusion-handoff.zh.md` 是 warp-fusion 的权威交接文档；以下三点需要对齐：

1. **生产者文件数**：handoff 说 `main@beb1731` 的 `instacloud-warp/` 只有 4 个文件（Dockerfile、compose、endpoints、entrypoint），缺 patch/healthcheck/firewall。实际 `instacloud-warp` 分支已含全部 15 个文件，镜像构建与 16/16 实测通过。以本分支为准。
2. **egress 同步**：handoff 第 8 节建议每 5 分钟同步清单。本生产者端口固定，换血只换端口后的 `/64`，网关无需重读 `proxyfile`；只有节点 IP/端口段变化时才需要同步/Reload。见 4.3。
3. **`free_tier_block_seconds`**：旧 main 中零调用；warp-fusion 用 `static_proxy_rate_limit_cooldown_seconds=900` 承担额度 429 硬冷却。配置里把旧键填 `0`，不要指望它。

---

## 15. 联调清单（会话 A 直接照做）

1. 取本报告 10.3 的 16 行（或节点 `egress-ext.txt`），写入 `proxyfile` 指向的文件；确认无 `#`、16 行、每行 `socks5://`。
2. 确认网关是 `warp-fusion@f7506bd`（或更新），不是旧 main。
3. 按 9.3 写配置：`anonymous=true`、`proxies=[]`、`warp.enabled=true`、`warp.direct_fallback=false`、`warp.stack="ipv6"`、`static_proxy_rate_limit_cooldown_seconds=900`、`max_anonymous_exits=0`、`free_tier_block_seconds=0`。
4. Reload 后检查 `/healthz` 的 `warp` 块与网关日志的 fused 选路记录。
5. 从中心容器抽样 3 个端口：

   ```bash
   for p in 1081 1088 1096; do
     curl -sS -m 10 --socks5-hostname "warpuser:WarpPass2026@NODE_PUBLIC_IP:$p" \
       https://opencode.ai/cdn-cgi/trace | grep -E '^(ip|loc)='
   done
   ```

   期望 `ip=2a09:*`、`loc=JP`（正式节点）或 `loc=US`（当前沙箱）。
6. 若只看到本地 WARP、看不到本池：检查 `proxyfile` 路径、`proxies` 是否为空、`warp.direct_fallback` 是否为 `false`、`HasExternal` 是否为真。
7. 若本池节点大量 429：观察 `limitedUntil` 硬冷却计数；≥4 且持续 5 分钟，扩容第二节点或提高 `pool_size`。
