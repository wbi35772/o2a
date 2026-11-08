# 出口池与优选 IP 设计 v1

日期：2026-11-04  
范围：`wbi35772/opencode2api` 分支 `warp-stability`，容器内 mihomo sidecar + `proxyctl` + 网关匿名车道。

## 0. 一句话

把“节点”拆成四层：**入口（优选 IP）→ 背板/供应商 → 出口（配额单位）→ 连接（配额载体）**。  
优选 IP 只解决入口延迟和可达性；免费额度由**出口 IP** 决定，而出口按 **TCP 连接**粘滞。  
所以真正的容量杠杆是：**换连接重抽出口 + 出口池多样性 + 连接钉扎**，不是换入口 IP。

## 1. 实测结论（2026-11-04，warp-test）

| 观测 | 数据 | 含义 |
|---|---|---|
| 订阅规模 | 1419 节点 / 422 唯一 server / 391 唯一 IPv4 | 节点很多，但入口不等于出口 |
| 背板数量 | 2 个域名：`cf4.mintyyoburneb.cc.cd`（1203）、`cf9.fulviaimasrvelkm.de5.net`（216） | 绝大多数“节点”只是同一背板的不同入口 |
| IP 归属 | 39/391 属 Cloudflare 官方段，352 属其他机房/反代 | “其他机房 IP”确实存在，但要看它指向哪个出口池 |
| 探活 | 389 个去重入口，190 个 SOCKS 可用 | 入口可用率约 49% |
| 出口分布 | 5 个 listener × 5 次请求 → 约 22 个不同出口 IP；同一出口跨 listener 出现 | 出口池是共享的，不是“一节点一出口” |
| 出口粘滞 | 同一 TCP 连接 5 次请求 → 同一出口（82.23.177.177 ×5；95.140.154.46 ×3） | **换连接才会换出口** |
| 配额 | 30 次请求 → 1×200；168/190 listener 收到 `FreeUsageLimitError`；`Retry-After` 56k–60k 秒 | 出口池当天基本烧穿，重置指向 ~00:00 UTC |
| 出口池来源 | 观测到的出口 `8.222.219.140`(SG)、`47.57.245.232`(HK)、`8.218.70.238`(HK) 与 IPDB `bestproxy` 列表头部一致 | 公共反代 IP 池被多人共享，已高度消耗 |

### 三个直接推论

1. **入口 IP ≠ 出口 IP。** 换优选 IP 只改变“从哪进”，不改变“从哪出”，因此不改变配额桶。
2. **配额单位是出口 IP/天，载体是连接。** 一条新 TCP 连接 = 一次抽签；抽中未耗尽出口的概率记为 `f`，当前约 3%。
3. **当前代码把 listener 当成出口。** `FreeUsageLimitError` 后 `markBlocked(16h)` 退役 listener，等于把“换连接重抽”的唯一通道封死；190 个 listener 在 30 次请求后剩 22 个可用，就是这个 bug 的直接结果。

## 2. 四层模型

| 层 | 单位 | 决定什么 | 可用手段 |
|---|---|---|---|
| 入口 ingress | `IP:port` + SNI/Host | 延迟、丢包、可达性 | 优选 IP 扫描、colo/ASN 过滤、EWMA、故障切换 |
| 背板 backend | 域名 / Worker / 反代 | 出口池归属 | 订阅聚合、供应商多样性、私有出口 |
| 出口 egress | 公网 IP | 免费配额/天 | 新鲜度、配额探测、出口图谱、按出口冷却 |
| 连接 conn | TCP/TLS | 出口粘滞 | 钉扎、429 后关闭重抽、并行 hedge |

### 关键公式

- 单次请求成功率：`P(N) = 1 - (1 - f)^N`，N 为独立新连接数。  
  `f=3%`：N=8 → 22%，N=16 → 39%，N=32 → 62%，N=64 → 86%，N=76 → 90%。
- 有效吞吐：`T = S / (D·t_draw + S·t_serve)`，S 为成功数，D 为抽签数。  
  优选 IP 降低 `t_draw`；出口多样性提高 `f`；连接钉扎降低 `t_serve` 的重复抽签。
- 出口日容量：`C = Σ_e Q_e`，`Q_e` 为该出口当日剩余配额。
- 入口评分：`score = 100 - (0.4·norm(L_p95) + 0.2·loss·100 + 0.1·norm(J) + 0.3·(1 - quotaYield)) - 5·failStreak`。

## 3. 为什么优选 IP 仍然必须做

优选 IP 不增加配额，但它决定“每次抽签有多快、多稳”：

- 把 `t_draw` 从 1.5s 降到 0.8s，N=32 时 90% 成功率的时间从 ~48s 降到 ~26s。
- 减少 TLS/WS 握手失败和半开连接，避免把一次抽签浪费在死入口上。
- 把“其他机房 IP”作为入口候选，能验证它们是否指向**不同的出口池**；如果指向同一背板，则只提升延迟，不提升配额。

**因此扫描必须同时测三件事：延迟、出口 IP、配额产出。** 只测延迟的优选 IP 会得出错误结论。

## 4. 总体架构

```
订阅源 ──> proxyctl sync ──> merged nodes（模板）
                                  │
IPDB / CT / DNS / ASN ──> candidate builder ──> candidates.json
                                  │
                        exitctl scan（临时 mihomo）
                                  │
        ┌─────────────────────────┼─────────────────────────┐
        │ 1. TCP/TLS 延迟         │ 2. SOCKS 探活           │ 3. 出口 IP + 配额探针
        └─────────────────────────┼─────────────────────────┘
                                  ▼
                     scores.json / egress.json
                                  │
                     selector（topK + 权重 + colo 多样性）
                                  │
                     proxyctl apply ──> mihomo.yaml + proxies.txt
                                  │
                     gateway（钉扎 / 429 关闭重抽 / hedge）
```

### 组件

| 组件 | 位置 | 职责 |
|---|---|---|
| `exitctl` | `cmd/exitctl`（新） | 候选构建、临时 mihomo 生命周期、三合一探针、评分、watch |
| `exitpool` | `internal/exitpool`（新） | 数据模型、候选源、评分、出口图谱、配额探针客户端 |
| 代理中心 API | `internal/admin/proxy.go` | `/candidates`、`/scan`、`/egress`、`/weights`、`/plan` |
| 代理中心 UI | `webui/js/pages/proxy.js` | 订阅 / 节点 / 优选 IP / 出口图谱 / 日志 |
| `proxyctl` | 仓库根 | 接受 `selected.json` + `weights.tsv`，生成克隆与监听端口 |
| 网关 | `internal/gateway` | 连接钉扎、429 语义、health 指标 |

## 5. 数据模型

### 5.1 `proxycenter/candidates.json`

```json
{
  "version": 1,
  "generated_at": "2026-11-04T12:00:00Z",
  "candidates": [
    {
      "id": "cand_p1_s0US01_104.16.1.1",
      "provider": "p1",
      "template": "s0-US-01",
      "server": "104.16.1.1",
      "port": 443,
      "sni": "cf4.mintyyoburneb.cc.cd",
      "source": "ipdb|ct|dns|sub|asn",
      "asn": 13335,
      "colo": "SIN",
      "first_seen": "2026-11-04T08:00:00Z",
      "last_probe": "2026-11-04T12:00:00Z",
      "latency_ms": 120,
      "latency_p95_ms": 180,
      "loss": 0.02,
      "jitter_ms": 8,
      "fail_streak": 0,
      "quota_yield": 0.12,
      "egress_samples": ["8.222.219.140", "47.57.245.232"],
      "score": 73.4,
      "status": "fresh|burned|dead|unknown"
    }
  ]
}
```

### 5.2 `proxycenter/egress.json`

```json
{
  "version": 1,
  "egress": {
    "8.222.219.140": {
      "first_seen": "2026-11-04T08:00:00Z",
      "last_seen": "2026-11-04T12:00:00Z",
      "providers": ["p1"],
      "ingress": ["104.16.1.1"],
      "quota": "burned",
      "retry_after": 57733,
      "hits": 12,
      "successes": 1
    }
  }
}
```

### 5.3 `proxycenter/weights.tsv`

```
name	weight
p1|s0-US-01@104.16.1.1	5
p2|s0-HK-03@47.57.245.232	3
```

### 5.4 `proxycenter/selected.json`

完整的 mihomo 节点定义（含 `server` 覆盖），由 selector 生成。`proxyctl apply` 优先读它；不存在时才回退到 `selected.txt` 的“均匀采样”。

## 6. 算法

### 6.1 候选来源

| 来源 | 方法 | 用途 |
|---|---|---|
| 订阅 server | 从 `merged.json` 提取 `server`/`servername` | 已知入口 |
| DNS | `dig +short A domain`、CNAME 链 | 背板当前地址 |
| IPDB | `bestcf`、`bestproxy`、`proxy`（30–60 分钟） | CF 官方 + 反代候选 |
| LancelotRar | `best-cf-ip-scanned-top20.txt`、`best-cf-ip-collected.txt` | 已扫描的 CF 候选 |
| byJoey/cfnew-ipdb | 按 colo 的 `SIN/HKG/NRT/LAX` 文件（102k） | 分区域候选 |
| CT / 被动 DNS | `crt.sh` 找 hostname，再解析 A | “其他机房”发现 |
| ASN | 对已知入口的 ASN/相邻 /24 做有界扫描 | 其他机房同背板候选 |

**候选过滤**：先做 TLS 握手，证书 SAN 必须覆盖节点 `servername`（除非节点本身 `skip-cert-verify`）；不匹配的直接淘汰，避免把配额探针浪费在错背板上。

### 6.2 三合一探针

对每个“模板 × 候选 IP”克隆：

1. **TCP/TLS**：连接 `server:port`，SNI = `servername`，ALPN `h2,http/1.1`；记录握手毫秒、证书是否匹配。
2. **SOCKS 探活**：通过临时 mihomo 的 listener 发 `GET https://opencode.ai/`；10s 超时；任何 HTTP 响应算活。
3. **出口 ID**：同一条连接（keep-alive）请求 `https://api.ipify.org?format=json`，记录出口 IP。
4. **配额探针**：同一条连接 `POST https://opencode.ai/inference`，使用网关同款 free-model body（含工具定义，`max_tokens=1`，`stream=false`），新随机 session；记录 `200 / 429 FreeUsageLimitError / 403 abuse / 其它`、`Retry-After`、`x-ratelimit-*`。

> 关键：出口 ID 和配额探针必须尽量走**同一条连接**，否则 429 无法归因到观测到的出口。Go 探针用 `MaxIdleConnsPerHost=1`、不设 `req.Close`，并校验前后出口 IP 一致；不一致则标 `attribution=uncertain`。

探针预算：每个候选 1 次推理请求；全量 500 候选约消耗 500 次请求，其中只有新鲜出口会计入配额。默认每日上限 100 次配额探针，其余只做 TCP/TLS + 探活。

### 6.3 评分

- 延迟 EWMA：`L_t = α·l_t + (1-α)·L_{t-1}`，`α=0.125`（借 cfnat）。
- 丢包：最近 10 次失败率。
- 抖动：相邻延迟差的 EWMA。
- 连续失败：`failStreak`；**连续 2 次失败即切换**（cfnat 策略）。
- 配额产出：Beta 后验 `Beta(1+success, 1+quota429)`，取 5% 分位，避免小样本运气。
- 综合分：见第 2 节公式。
- **两阶段**：先并行 50 路做 TCP/TLS + 探活；只对每个 provider 的前 30 名做配额探针。
- **多样性约束**：每个 provider 的 topK 至少覆盖 2 个 colo/ASN，避免整个 topK 来自同一个已烧穿的池。

### 6.4 选择与权重

- 每个 provider 保留 top 8 入口；2 个 provider → 约 16 个模板入口。
- 每个入口可克隆 1–3 个候选 IP（按分数），总 listener 控制在 48–96，而不是 600。
- 权重 `w = max(1, round(score/20))`；`proxies.txt` 按权重重复该 listener 行。网关把每一行当成独立 transport + 独立连接池，因此重复行 = 更多抽签槽位，同时不改变 mihomo 端口。
- 每 6 小时全量扫描；每 30 分钟热探测（top 16 + 随机 8）；连续 3 次失败从候选池补位。
- 每日 `00:05 UTC` 把所有 `burned` 出口重置为 `unknown`，跑一次 top 40 验证扫描（`Retry-After` 指向 ~00:00 UTC）。

### 6.5 自动换血（watch）

`exitctl watch` 常驻循环：

1. 检查 mihomo 存活、`proxyctl status`、listener 数量。
2. 某 listener `failStreak ≥ 2` → 标 dead，从 spare 补位并 `proxyctl apply`。
3. 全部选中入口的 `quota_yield` 低于阈值（如 0.02）→ 触发一次扩大扫描。
4. 到达 UTC 00:05 → 重置出口状态 + 验证扫描。
5. 扫描使用**临时 mihomo**（独立 `-d` 和端口段），不干扰主 fleet；扫描结束关闭。

## 7. 网关侧 P0（必须先做）

### 问题

`FreeUsageLimitError` 是**出口连接**的属性，不是 listener 的属性。当前 `internal/gateway/upstream.go` 的 429 分支：

```go
until := time.Now().Add(g.freeUsageBlockTTL(resp.Header.Get("Retry-After")))
node.proxy.markBlocked(until)
```

把 listener 退役 16h。但同一个 listener 换一条连接就会换出口，因此正确动作是**关闭连接、重抽**，不是封 listener。

### 改动

1. `proxyTransport` 增加：
   ```go
   func (t *proxyTransport) closeIdle() {
       if t == nil { return }
       if t.client != nil { t.client.CloseIdleConnections() }
       if t.bun != nil { t.bun.CloseIdleConnections() }
   }
   ```
2. 配额 429 分支改为：
   - `node.proxy.closeIdle()`
   - `node.proxy.noteQuotaHit()`（原子计数）
   - `lastErr = errFreeTierIPBlocked`，`deferred = true`
   - **不调用 `markBlocked`**
3. 403 abuse 分支：
   - 同样 `closeIdle()`；
   - 记 `abuseHits`；连续 3 次才标记 15 分钟（可能是 provider 级封禁）；
   - 单次 403 只做 30s 软冷却。
4. health：
   - `healthProxies` 增加 `quota_hits`、`abuse_hits`、`quota_yield`；
   - `blocked` 只统计硬封禁；
   - `anonymous_exits_exhausted` 只在 `blocked == total` 时触发，不再被配额 429 误触发。
5. 配置：
   - `performance.rotating_egress`（默认：配置了 `proxyfile` 时为 true，静态 `proxies` 为 false）；
   - `performance.hedge_draws`（P1，默认 4）：前 2 次串行尝试都是配额 429 时，并行开 4 条新连接，首个 2xx 胜出，取消其余。
6. **快速失败**：前 K 次抽签全 429 时立即 503，不再走满 8 次（当前 9–16s，可降到 ~3s）。

### 预期

- `blocked` 不再随请求数上升；`quota_hits` 反映真实抽签失败次数。
- 190 个 listener 保持可抽签状态，不会在 30 次请求后只剩 22 个。
- 单请求成功率仍由 `f` 和 `N` 决定（8 次抽签约 22%）；hedge 后 N=32 时约 62%，p95 目标 ≤ 6s。

## 8. T0 轮子的取舍

| 项目 | Stars | 借什么 | 不借什么 |
|---|---:|---|---|
| `fscarmen/cfnat` | 49 | EWMA 评分、连续 2 次失败切换、colo 过滤、池耗尽重扫 | 单端口 TCP 转发器：mihomo 需要每节点 SOCKS，不适用 |
| `cmliu/CFnat-Docker` | 359 | 容器化、env 配置、实时 IP 过滤 | 同上 |
| `XIU2/CloudflareSpeedTest` | 29815 | TCPing/HTTPing 延迟 + 下载测速 + `result.csv` | 只测速不测配额 |
| `ymyuuu/IPDB` | 2616 | `bestcf/bestproxy/proxy` 候选源（30–60 分钟） | 不能把公共反代 IP 当可信出口 |
| `LancelotRar/best-cf-ips` | 1639 | 已扫描 top20 + collected | 同上 |
| `byJoey/cfnew-ipdb` | — | 102k 按 colo 的候选 | 同上 |
| `cmliu/CF-Workers-SUB` | 7514 | 订阅聚合/转换 | 不替代 `proxyctl` |
| `cmliu/socks5-for-serv00` | 603 | **私有出口**：Serv00/CT8 上部署 SOCKS5，crontab 保活、Actions 续期 | 免费机稳定性有限，作为补充池 |
| `cmliu/CF-Workers-CheckProxyIP` | 734 | 反代 IP 可用性检查 | 只检查可用性，不检查配额 |
| `jiangxianli/ProxyIpLib` | 856 | 反代 IP 库 | 同上 |

**结论**：cfnat 的“扫描 + 评分 + 切换”直接借；它的转发器不借。我们用 mihomo 克隆节点做探针，复用现有 VLESS/Trojan 实现，避免重写协议。

## 9. 路线图

### P0（1–2 小时）— 止血

- 网关：429 关闭连接重抽，不封 listener；health 加 `quota_hits`；快速失败。
- 部署到 `warp-test`，跑 30 次请求验证 `blocked` 不增长、`quota_hits` 增长。
- 这是当前可用性 bug，优先于优选 IP。

### P1（半天）— 扫描 MVP

- `cmd/exitctl`：候选源（订阅 server + IPDB + DNS）、临时 mihomo、TCP/TLS + 探活 + 出口 ID + 配额探针。
- `scores.json` / `egress.json` 落盘；admin API `/scan`、`/candidates`、`/egress`。
- UI「优选 IP」表格：延迟、丢包、出口、配额产出、状态。

### P2（1 天）— 选择与换血

- selector：topK + 权重 + colo/ASN 多样性；`selected.json` + `weights.tsv`。
- `proxyctl apply` 支持克隆节点（`server` 覆盖）与权重重复行。
- `exitctl watch`：2 次失败换血、30 分钟热探测、00:05 UTC 重置。
- UI：出口图谱（provider → ingress → egress）。

### P3（1–2 天）— 容量提升

- 网关 hedge draw（4/8/16 可配），首个 2xx 胜出，取消其余。
- 私有出口：`socks5-for-serv00` 部署指南 + 接入 `proxies`（静态出口，按天封禁）。
- 供应商权重：Thompson sampling（Beta 后验），按 provider/backend 分配抽签预算。

### P4 — 运营化

- 告警：出口耗尽、扫描失败、mihomo 掉线。
- 导出/导入候选与出口图谱。
- 成本与配额报表（每日抽签数、成功数、有效出口数）。

## 10. 验收标准

| 阶段 | 验收 |
|---|---|
| P0 | 30 次请求后 `blocked=0`、`quota_hits≈29`、无 `anonymous_exits_exhausted`；失败耗时 ≤ 16s |
| P1 | 200 候选扫描 < 120s；每条有延迟/出口/配额；扫描期间主 fleet 正常服务；UI 可见 |
| P2 | 连续 2 次失败的 listener 60s 内被替换；topK 覆盖 ≥2 colo/ASN；`proxies.txt` 权重生效 |
| P3 | `f=3%` 时 32 路 hedge 成功率 ≥ 60%，p95 ≤ 6s；私有出口产出 ≥1 个新鲜 IP |
| P4 | 出口图谱可下钻；烧穿 IP 标记；00:05 UTC 自动恢复验证通过 |

## 11. 风险与约束

- **配额探针本身消耗配额**：每日上限 100 次；只对每个 provider 前 30 名做。
- **临时 mihomo 内存**：测试机 6GB，主 fleet 约 200MB；扫描并发 ≤ 50，候选 ≤ 500。
- **出口归因不确定**：连接中途换出口时标 `uncertain`，不把 429 记到错误出口。
- **公共 IPDB 已共享**：只当入口候选，不当“新鲜出口”保证。
- **服务条款/滥用**：扫描限速、随机延迟；不把探针打到非目标域名。
- **hedge 放大上游负载**：限制并发、取消败者、设置总预算；不要在高峰期开太大。
- **免费机私有出口不稳定**：只作为补充，不替代订阅池；掉线由 watch 自动剔除。

## 12. 复现命令（容器内）

```sh
# 同连接出口粘滞（5 次请求走一条连接）
/.insta/tools/bin/curl -s \
  --socks5-hostname 127.0.0.1:21000 \
  https://api.ipify.org https://api.ipify.org https://api.ipify.org \
  https://api.ipify.org https://api.ipify.org

# 跨 listener 出口分布（每次新进程 = 新连接）
for p in 21000 21001 21002 21003 21004; do
  for i in 1 2 3 4 5; do
    /.insta/tools/bin/curl -s --socks5-hostname 127.0.0.1:$p https://api.ipify.org
    echo
  done
done

# 配额探针（示例，body 需用网关同款 free-model body）
/.insta/tools/bin/curl -s -o /dev/null -w '%{http_code}\n' \
  --socks5-hostname 127.0.0.1:21000 \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer $API_KEY' \
  -H "x-opencode-session: ses_probe_$(date +%s)" \
  --data @/tmp/probe-body.json \
  https://opencode.ai/inference
```

## 13. 代码落点

| 文件 | 改动 |
|---|---|
| `internal/gateway/upstream.go` | 429 分支：`closeIdle()` + `noteQuotaHit()`，移除 `markBlocked` |
| `internal/gateway/pool.go` | `proxyTransport.closeIdle()`、`quotaHits`、`rotating` 标志 |
| `internal/gateway/health.go` | `quota_hits`、`abuse_hits`、`quota_yield`；修正 `anonymous_exits_exhausted` |
| `internal/gateway/abuse.go` | `freeUsageBlockTTL` 仅保留给静态出口；新增配额产出的滚动统计 |
| `internal/admin/proxy.go` | `/scan`、`/candidates`、`/egress`、`/plan`；探针复用 `proxyTester` |
| `proxyctl` | 读 `selected.json` / `weights.tsv`；克隆节点；权重重复行；`plan` 子命令 |
| `webui/js/pages/proxy.js` | 优选 IP、出口图谱、权重、扫描任务 |
| `cmd/exitctl`（新） | 扫描编排、watch、临时 mihomo |
| `internal/exitpool`（新） | 候选/评分/出口图谱/配额探针 |

---

**设计原则**：优选 IP 是入口优化，不是配额魔法。先把“换连接重抽”这条唯一的路修好，再用扫描把每次抽签的成本降下来，最后用私有出口/更多背板提高 `f`。
