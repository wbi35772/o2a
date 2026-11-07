# instacloud-warp × opencode2api 耦合报告 v2

> 版本：v2（源码级耦合分析，取代 v1 的对接章节）
> 生产者：`warp-pool-producer`，分支 `instacloud-warp`（commit `8f304ab`）
> 网关源码基准：`wbi35772/opencode2api` `main@58f56676`
> 结论一句话：**网关开 `warp.enabled` 时，免费模型流量全部走内置 WARP，`proxyfile` 静态池被完全绕过；要用本池，必须 `warp.enabled = false`。**

---

## 0. 一页结论（TL;DR）

| 项 | 结论 |
| --- | --- |
| 两套 WARP 能否同时生效 | **不能**。同一时刻免费匿名车道只走一条路：内置 WARP 或静态代理池 |
| 用本池的开关 | `warp.enabled = false` + `proxyfile` 指向 16 行清单 |
| 用内置 WARP 的开关 | `warp.enabled = true`（`pool_size` 1–32，默认 2） |
| 本池的定位 | 境外节点 16 条独立 `2a09:*` `/64` 出口；补足或替代中心机房的 WARP 出口 |
| 清单里能不能有 `#` | **不能**。`#pool` 会被识别为动态池：不冷却、每次新建连接、池内循环抽取 |
| 网关会不会自动重读 proxyfile | **不会**。只在配置加载/管理端 Reload 时读取；无文件监视 |
| 换血后要不要重载 | **不需要**。清单 URL 恒定，出口在端口后面漂移；仅节点 IP/端口变化时重载 |
| 每请求最多试几个出口 | `min(池大小, max_anonymous_exits)`，默认 `min(16, 8) = 8` |
| 失败冷却 | 15s 起，逐次翻倍，上限 8×（15/30/60/120s）；`Retry-After` 可延长到 120s |
| 免费层 IP 屏蔽 | 默认 900s（`free_tier_block_seconds`），命中后该出口被完全跳过 |
| 与 900s 换血的配合 | 屏蔽到期时，端口后面的 `/64` 已换新，配额身份随之更新 |

---

## 1. 目标与范围

本报告回答三件事：

1. 本池如何接入 opencode2api，**改哪些配置、不改哪些代码**；
2. 网关内部对静态 SOCKS5 出口的真实处理逻辑（源码级），以及由此决定的行为边界；
3. 两套 WARP 出口方案（网关内置 vs 本池）的取舍、切换与回滚。

不覆盖：网关业务代码修改（红线）、IPv4 出口（红线）、与存量 SIN/IAD/FRA `/48` 家族重叠（红线）。

---

## 2. 核心结论：两套 WARP 出口互斥

### 2.1 源码事实

- `WarpConfig` 的注释写明：启用后，**匿名车道的免费模型流量与免费 Zen-key 回退**改走用户态 WireGuard 隧道，"instead of any configured proxy"（`internal/config/config.go:68-74`）。
- 匿名请求入口先判断 WARP：
  ```go
  // internal/gateway/upstream.go:359-361
  if g.usesWARP(route.ID, config.TierZen, true) {
      return g.doWarpAnonymousUpstream(ctx, route, bodies, ids, attemptOffset)
  }
  ```
- `usesWARP` 的条件是"WARP 池非空 + 免费模型 + 匿名/免费车道"（`internal/gateway/warp.go:36-39`）。
- README 也明确：`warp.enabled` 把免费车道的全部流量（匿名车道、免费模型经 Zen key 的回退、免费模型可用性探测、匿名目录刷新）搬到 WARP 隧道，"Those requests use no proxy"（`README.md:249`）；关掉 WARP 才恢复代理路由（`README.md:255`）。

**因此：`warp.enabled = true` 时，`proxyfile` 里的 16 个 SOCKS5 出口对免费模型完全不可见。** v1 报告里"`warp.enabled` 保持 `true` + `proxyfile`"的写法是错误的，已在 v1 文件头部加更正说明。

### 2.2 模式矩阵

| 模式 | `warp.enabled` | `proxyfile` | 免费车道实际出口 | 适用场景 |
| --- | --- | --- | --- | --- |
| A：静态池 | `false` | 16 行 | 本池 16 个境外 `/64` | 本池试点、中心机房 WARP 出口不理想 |
| B：内置 WARP | `true` | 保留但不用 | 网关自建 WARP 池（1–32 个） | 中心主机 UDP 可达、有可路由 `/64` |
| 混合 | 不支持同一车道 | — | 二选一 | 用 Reload 在 A/B 间切换 |

### 2.3 推荐路径

- **试点期用模式 A**：中心容器不改代码，只改配置；本池 16 个日本 `/64` 提供与存量三机房不同的 `/48` 家族。
- **保留模式 B 作为一键回退**：本池节点故障时，把 `warp.enabled` 改回 `true` 并 Reload，免费车道立刻切回网关内置 WARP（前提是中心主机 WARP 可用）。
- 两种模式都**不要**同时把 `proxyfile` 当免费车道出口用——模式 B 下它只是配置里的一段死数据（键控层级仍可能使用代理，见 3.6）。

---

## 3. 网关代理模型（源码级）

### 3.1 proxyfile 的解析规则

`internal/config/proxy.go`：

| 规则 | 位置 |
| --- | --- |
| 每行一个代理；空行丢弃；行首 BOM 剥除；单行上限 1 MiB | `proxy.go:68-89` |
| 注释：`#`、`;`、`//` 仅在**行首**或**空格/制表符之后**才生效；URL 内部的 `//` 与 `#pool` 片段保留 | `proxy.go:94-104` |
| 相对路径基于配置文件所在目录解析 | `proxy.go:42-55` |
| 去重：按"清洗后的拨号 URL + 池标记"判重，保留首次出现的拼写 | `proxy.go:106-137` |
| 列表为空时回退为 `direct` | `proxy.go:57-60` |
| 配置内 `proxies` 数组在前，proxyfile 条目在后，合并去重 | `proxy.go:42-61` |

对我们的含义：16 行 `socks5://user:pass@IP:1081..1096` 端口各不相同，全部保留；文件里不要出现任何 `#` 字符（我们的导出脚本已强制校验）。

### 3.2 静态条目 vs `#pool` 动态池

`ParseProxyEntry` 支持 `pool+` 前缀或 `#pool` 片段（`config/proxy.go:15-36`）。两者行为差异是耦合的关键：

| 行为 | 静态条目（我们的清单） | `#pool` 条目 |
| --- | --- | --- |
| 失败冷却 | 有（`MarkFailure` 生效） | **无**，直接 `return`（`gateway/pool.go:395-398`） |
| 连接复用 | 允许 keep-alive / HTTP/2 | 禁用 keep-alive、强制 HTTP/1.1，每次尝试新建 TCP+CONNECT（`pool.go:450-466`） |
| 抽取方式 | 每个请求最多访问一次 | 每次尝试都重新抽取，`retry.max_attempts` 次（`upstream.go:346-349`、`config.go:241-246`） |
| 池内优先级 | 正常轮转 | 一旦池内存在 `#pool`，第一轮之后只循环 `#pool` 条目（`pool.go:313-322`） |
| 健康检查 | 不健康的会被探测恢复（`pool.go:510+`） | 跳过 |

**结论：本池必须使用纯静态条目。** 一旦误加 `#pool`，网关会把它当无限旋转代理：不冷却、每次新建连接、优先循环——看起来"总在换出口"，实际绕过了我们精心设计的 `/64` 去重与 900s 换血语义。

### 3.3 匿名出口池的调度与冷却

- 每请求游标 `Next()` 的顺序：先健康且未冷却的静态节点；没有 `#pool` 时，再回退到"尚未访问、冷却最早到期"的节点（`pool.go:276-346`）。
- 免费层标记为滥用的出口（`blockedUntil`）**连回退都不会被选中**（`pool.go:294-307`）。
- `MarkFailure` 触发条件：传输错误，或 401/403/429/5xx；其它响应不惩罚（`pool.go:395-401`）。
- 冷却时长：`failure_cooldown_seconds × 2^(连续失败数-1)`，上限 8×；`Retry-After` 可延长，但同样封顶 8×（`pool.go:402-409`）。默认基数 15s ⇒ 15/30/60/120s。
- 成功一次即清零失败计数与冷却（`pool.go:348-354`）。
- 免费层 IP 屏蔽：`blockedUntil`，默认 900s（`config.go:247-250`；`pool.go:57-64`）。

### 3.4 匿名请求的尝试预算

```go
// internal/gateway/upstream.go:379-382
limit := min(g.anonymous.Len(), g.maxAnonymousExits())
if g.anonymous.HasPool() && g.cfg.Retry.MaxAttempts > limit {
    limit = g.cfg.Retry.MaxAttempts
}
```

- 静态池：`limit = min(16, 8) = 8`，每个出口最多试一次。
- `retry.max_attempts`（默认 3）**不会**扩大静态池的遍历范围；它只影响 `#pool` 与其他层级。
- 想让单个请求走完 16 个出口，调 `max_anonymous_exits = 16`；代价是全池故障时请求耗时线性增长。

### 3.5 配置重载

- `Reload()` = 重新读取配置文件（含 proxyfile）并应用（`gateway/runtime.go:428-442`），触发点只有管理端 API（`internal/admin/server.go:268`，WebUI 保存/重载走这里）。
- **没有 fsnotify / 定时重读**。
- 我们的清单 URL 恒定（`IP:1081..1096` 不变，换的是端口后面的 WARP 隧道与 `/64`），所以**换血、健康修复都不需要网关重载**；只有节点公网 IP 或端口段变化时才需要 Reload。

### 3.6 键控层级与代理的关系

`transportPool(cfg.RuntimeProxies())` 为每个运行时代理建一对 HTTP 客户端（普通 + Bun 伪装），Zen/Go 键池与匿名池共用这些 transport（`gateway/gateway.go:98-139`、`pool.go:440-498`）。键控层级是否实际绑定到某个代理由键的绑定/轮转策略决定；本报告不展开，但要注意：模式 B 下 `proxyfile` 并非"完全无副作用"，键控层级仍可能用到它。

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
| 端口固定、出口漂移 | 网关只在 Reload 时读清单；URL 恒定意味着换血零重载 |
| 纯 IPv6 `AllowedIPs = ::/0` | 网关看到的是 `2a09:*` `/64`，配额身份按 `/64` 计 |
| `/64` 去重 | 同一 `/64` 在网关侧共享免费额度，去重直接决定池的有效大小 |
| 900s 换血 | 对齐网关免费层屏蔽默认 900s：屏蔽到期时出口已换新 |
| 4×4 分批 | 每批只动 4 个端口，任意时刻至少 12 个端口在线；网关单请求预算 8 个，仍有足够候选 |
| 链式注册 | Cloudflare 对注册源 IP 限速；链式让每个账号从不同 `/64` 注册 |
| 账号级故障自动重注册 | 网关侧只会看到"某端口长时间不健康"，节点自愈比人工快 |

### 4.3 状态与文件

| 路径 | 内容 |
| --- | --- |
| `/data/egress-ext.txt` | 16 行清单，原子 mv 重写 |
| `/data/status.tsv` | `port pid endpoint prefix64 loc state epoch` |
| `/data/instances/nXX/` | wgcf 账号、profile、wireproxy.conf、日志 |
| 容器日志 | 首轮完成打印可复制清单块；换血/健康/恢复均有结构化日志 |

---

## 5. 接口契约 v2

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

### 5.3 文件落点

- 网关配置 `proxyfile` 指向容器内路径（如 `/var/lib/opencode2api/egress-ext.txt`），相对路径基于配置文件目录解析。
- 建议 `0640`，属主为网关运行用户；文件内含 SOCKS 账密，不要进镜像层。

---

## 6. 容量与时序

### 6.1 单请求视角

- 候选出口：16；单请求最多尝试：8。
- 池健康时：第 1 个出口成功率极高，8 个预算 ≈ 2 倍冗余。
- 单出口被 429：进入 15s 冷却（连续失败翻倍），请求自动落到下一个出口。
- 单出口被免费层标记：900s 内完全跳过；900s 后换血已给它新 `/64`。

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

---

## 7. 故障矩阵

| 现象 | 判定 | 处置 |
| --- | --- | --- |
| 节点容器起不来 | 镜像/权限/端口占用 | `docker logs`；确认 `network_mode: host` 与端口未被占用 |
| n01 注册 429 | 节点 IP 被 Cloudflare 限速 | 等待 10 分钟或换节点 IP；已注册账号会复用 |
| 后续实例注册失败 | 链式代理不可用 | 日志看 `register-failed`；确认上一条端口探活成功；自动回退直连 |
| 某端口一直 `probe-failed` | 隧道握手失败/Endpoint 被墙 | 自动换 Endpoint（4 次）；仍失败则重注册 |
| `/64` 重复 | 机房出口池小 | 换 Endpoint；4 次仍撞车留给下一轮换血 |
| 中心 `curl` 超时 | 防火墙/安全组 | 节点执行 `firewall-allow-center.sh`；云安全组放行 1081–1096 |
| 中心 `curl` 返回 `socks5: authentication failed` | 账密不符 | 核对 `.env` 与清单 |
| 网关日志 `no_healthy_proxies` | 静态池全不健康 | 先确认 `warp.enabled=false`；再看节点 `status.tsv` |
| 免费车道仍走中心 WARP | `warp.enabled=true` | 改 `false` 并 Reload/重启 |
| 网关改了节点 IP 后不生效 | proxyfile 未重读 | WebUI 保存/Reload 或重启容器 |
| 出口 `loc` 不对 | 节点路由/机房 | 这是节点属性；换节点国家 |
| 出口是 `104.28.x.x` | 隧道退化为 IPv4 | 检查 `AllowedIPs` 是否被改回 `0.0.0.0/0`；重渲染配置 |

---

## 8. 安全模型

- **双层准入**：SOCKS5 账密（`warpuser` / `WarpPass2026`）+ 节点防火墙源 IP 白名单（SIN `13.229.234.225`、IAD `18.212.22.159`、FRA `3.79.179.114`）。
- 容器 `cap_drop: ALL`、`network_mode: host`（让 INPUT 白名单真正生效），不挂 docker.sock，不 privileged。
- `/data` 含 WireGuard 私钥：备份加密、权限收紧；`docker compose down -v` 会销毁账号池。
- 建议：上线前更换默认 `WarpPass2026`；`.env` 不入库；清单文件 `0640`。

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

### 9.2 中心（模式 A）

1. 把 16 行清单写入中心容器 `proxyfile` 指向的路径（无 `#`）。
2. 网关配置：

   ```json
   {
     "warp": { "enabled": false },
     "proxyfile": "/var/lib/opencode2api/egress-ext.txt",
     "performance": {
       "max_anonymous_exits": 8,
       "failure_cooldown_seconds": 15,
       "free_tier_block_seconds": 900
     }
   }
   ```

3. 保存/Reload（管理端 API 或重启容器）。
4. 验证：中心容器内 `curl --socks5-hostname` 打 `cdn-cgi/trace`，`ip=` 应为 `2a09:*`。

### 9.3 回滚

- 切回内置 WARP：`warp.enabled = true`（需中心 WARP 可用）→ Reload。
- 完全移除本池：删 `proxyfile` 引用 → Reload；节点容器可停。
- 节点侧：`docker compose down`（保留 `/data` 卷）；恢复时 `up -d`。

---

## 10. 验收与证据

### 10.1 沙箱实测（已通过）

| 检查 | 结果 |
| --- | --- |
| 镜像构建 | 60.6 MB；wgcf 补丁编译通过；wireproxy 1.0.8-dev |
| 冷启动 | 15/16，健康检查后 16/16 |
| 900s 满池换血 | 4×4 分批；换血后 14/16，5 分钟内收敛 |
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

---

## 11. 监控与告警建议

| 信号 | 来源 | 建议阈值 |
| --- | --- | --- |
| 池大小 | 节点日志 `Active Unique /64 Pool Size: N / 16` | <14 持续 10 分钟告警 |
| 端口状态 | `/data/status.tsv` 的 `state` | 非 `ok` 数量 ≥3 告警 |
| 出口国家 | 抽样 `loc=` | 与节点国家不符告警 |
| 网关代理健康 | WebUI Resources / `ResourceSnapshot.Proxies` | 不健康 ≥4 告警 |
| 免费层 429 比例 | 网关日志 | 持续升高说明 `/64` 池被过度使用 |
| WARP 状态（模式 B） | `/healthz` 的 `warp` 块 | `active=0` 且 enabled ⇒ 503，立即处理 |

---

## 12. 对接决策清单（逐条给推荐值）

| # | 决策 | 推荐 | 理由 |
| --- | --- | --- | --- |
| 1 | `warp.enabled` | `false`（模式 A） | 否则本池被绕过 |
| 2 | `proxyfile` | 16 行清单路径 | 静态条目，无 `#` |
| 3 | `max_anonymous_exits` | `8`（保持默认） | 16 端口 = 2× 冗余；调到 16 可全池遍历但延迟变差 |
| 4 | `failure_cooldown_seconds` | `15`（默认） | 15/30/60/120s 冷却与 900s 换血互补 |
| 5 | `free_tier_block_seconds` | `900` | 与换血周期对齐 |
| 6 | `retry.max_attempts` | `6`（可选） | 只影响 `#pool`/键控层级，对静态池无感 |
| 7 | 重载流程 | 仅节点 IP/端口变化时 Reload | URL 恒定，换血无需重载 |
| 8 | 节点数量 | ≥2（不同国家） | 单节点故障时池不归零 |
| 9 | 第二节点端口段 | `2081–2096` | 清单可直接合并，网关零改动 |
| 10 | 监控 | 池大小 + 端口状态 + `loc` 抽样 | 见第 11 节 |

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

### 13.3 网关源码索引

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

### 13.4 版本锚点

- 生产者：`instacloud-warp` 分支，commit `8f304ab`（单提交，基于 `main@58f56676`）
- wgcf `v2.3.1` + `patches/wgcf-socks5-proxy.patch`（纯标准库 SOCKS5）
- wireproxy `github.com/windtf/wireproxy@35cce290db2f32d5e4316bd54102186f486d0af4`
- 网关源码：`main@58f56676`
