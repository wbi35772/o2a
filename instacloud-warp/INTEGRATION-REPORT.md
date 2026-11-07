# instacloud-warp 接入报告

> 会话 B 交付物 · 供主反代 opencode2api（会话 A）对接讨论
> 仓库：`https://github.com/wbi35772/opencode2api/tree/main/instacloud-warp`
> 状态：沙箱节点全链路实测通过（构建 / 链式注册 / 去重 / 满池换血 / 故障恢复 / 导出验收）

---

## 1. 概述

本项目在**异国 Linux 节点**上批量生产 WARP 隧道，把出口约束为**纯 IPv6 `/64`**，以**带账密认证的 SOCKS5** 暴露给中心三台网关容器，作为 opencode2api 的上游出口池。

- 单节点产出：**16 条独立 /64 出口**，端口 `1081–1096`
- SOCKS5 认证：`warpuser` / `WarpPass2026`
- 换血周期：`900s`（对齐网关 `limitedUntil` 15 分钟硬冷却）
- 红线：不改 opencode2api 任何代码；不产生 IPv4 出口；不与存量 SIN/IAD/FRA 的 `/48` 家族重叠

```
节点 (JP/KR/GB/AU，公网 IPv4)
┌──────────────────────────────────────┐         中心容器 (SIN/IAD/FRA)
│ warp-pool-producer (network_mode:host)│
│                                       │  SOCKS5  ┌───────────────────────────┐
│  n01 wireproxy :1081 ──► 2a09:bac*/64 │ +账密    │ opencode2api              │
│  n02 wireproxy :1082 ──► 2a09:bac*/64 │◄────────│  egress-ext.txt (16 行)   │
│  ...                                  │ 源IP白名单│  warp.enabled = true      │
│  n16 wireproxy :1096 ──► 2a09:bac*/64 │          │  max_anonymous_exits = 8  │
│                                       │          │  limitedUntil = 15min     │
│  wgcf 链式注册 / 去重 / 健康 / 换血    │          └───────────────────────────┘
└──────────────────────────────────────┘
```

网关只会看到一行 `socks5://warpuser:WarpPass2026@NODE_IP:PORT`，每个端口背后是**不同 WARP 账号 + 不同 /64 出口**。节点本身不需要 IPv6、不需要 TUN、不需要 `--privileged`。

---

## 2. 交付物清单

仓库 `instacloud-warp/`（13 个文件，与实测沙箱副本逐字节一致）：

```text
instacloud-warp/
├── Dockerfile                       # 多阶段：wgcf v2.3.1（打补丁）+ wireproxy 固定 commit
├── docker-compose.yml               # host 网络 / cap_drop ALL / healthcheck / 状态卷
├── entrypoint.sh                    # B1~B4 全部逻辑（注册/渲染/去重/换血/健康/导出）
├── healthcheck.sh                   # 容器 HEALTHCHECK（≥14 个 ok 且能探通）
├── endpoints.txt                    # 32 个 Cloudflare Anycast IP:PORT 组合
├── patches/wgcf-socks5-proxy.patch  # 纯标准库 SOCKS5 拨号（关键补丁）
├── scripts/export-egress.sh         # 一键导出 + --verify 自检
├── scripts/firewall-allow-center.sh # 三台中心机 IP 白名单（ufw/iptables）
├── README.md                        # 部署 / 验收 / 排障
├── Makefile                         # 常用命令封装
├── .env.example                     # 全部环境变量与默认值
├── .dockerignore / .gitignore
└── INTEGRATION-REPORT.md            # 本报告
```

构建产物：`warp-pool-producer:1.0.0`，镜像约 **60.6 MB**，内置 `wireproxy 1.0.8-dev`。

---

## 3. 关键实现（设计原因，非流水账）

### 3.1 链式注册（B1）——绕开 Cloudflare 同 IP 限速

Cloudflare 对 `wgcf register` 有源 IP 限速（实测观察约 **2–3 次 / 10 分钟**，不同节点有差异）。16 个账号若都直连注册，必然 429。

- `n01`：容器原生网络直连注册。
- `n02+`：`HTTPS_PROXY=socks5h://warpuser:WarpPass2026@127.0.0.1:<LAST_WORKING_PORT>`，借用**前一条已探活隧道**的 `/64` 出口去注册，等价于每个账号来自不同源地址。
- **补丁原因**：wgcf 的 uTLS 传输自己 dial，原生不认 `HTTPS_PROXY`。`patches/wgcf-socks5-proxy.patch` 仅用标准库实现 SOCKS5 握手：TCP 先经 SOCKS5 到 `api.cloudflareclient.com`，uTLS 指纹握手仍端到端直达 Cloudflare，不引入 `golang.org/x/net`。
- 失败处理：删除损坏的 `wgcf-account.toml` → 等 15s 重试 → 链式连续两败回退直连（`REGISTER_RETRIES=3`）。
- 账号持久化在 `/data/instances/nXX/`，重启复用不重复注册。

影响：冷启动顺序注册 16 个账号，**1–3 分钟**；热启动（账号已存在）**<60 秒**。

### 3.2 纯 IPv6 渲染（B2）

`wgcf-profile.conf` → `wireproxy.conf` 的逐项变换（`render_conf`）：

| 变换 | 说明 |
| --- | --- |
| 剥离 `172.16.0.2/32` | 去 IPv4 隧道地址 |
| 剥离 `1.1.1.1, 1.0.0.1` | 去 IPv4 DNS |
| 剥离 `0.0.0.0/0` | 去 IPv4 默认路由 |
| `Address` 只保留 IPv6 | 按内容筛选，不依赖 wgcf 输出顺序 |
| `DNS` 只保留 IPv6 | 过滤后为空则回填 `2606:4700:4700::1111, 2606:4700:4700::1001` |
| `AllowedIPs = ::/0` | 强制纯 IPv6 出口（红线） |
| `Endpoint` 替换 | 从 `endpoints.txt` 随机取 Anycast `IP:PORT` |
| 追加 `[Socks5]` | `BindAddress = 0.0.0.0:PORT` + `Username` + `Password` |

启动前先跑 `wireproxy -n -c`（config test），失败不打端口。

### 3.3 Endpoint 池、去重与换血（B3）

- `endpoints.txt`：8 个 Anycast 地址 × 4 端口 = **32 组合**，启动时同容器内不重复分配。
  - `162.159.193.1` 已于测试后移除：同一已知可用账号对其 4 个端口全部握手失败，其余 8 个地址全部通过。
- 探活：`curl -x socks5h://... https://opencode.ai/cdn-cgi/trace`，取 `ip=` 前 4 段为 `/64`。
- 判定：空、非 `2a09:`、命中禁区 `/48`、或与其它端口重复 → 换 Endpoint 重启该实例，`DEDUP_RETRIES=4`。
- 4 次仍空/非法（账号级故障）→ `recover_account`：停实例、删账号与 profile、重走链式注册（实测 ~30 秒恢复）。
- **纯 `/64` 重复不再重注册**（省账号额度），留给下一轮换血或健康检查。
- 换血：`REROLL_INTERVAL=900s` 全量 16 个，分 4 批 × 4 个（批间 5s），降低整体抖动。
- 健康检查：`HEALTH_INTERVAL=300s` 并发探活全部端口；失败/重复先换 Endpoint，再兜底 `bring_up`（必要时重注册），然后重跑去重与导出。

### 3.4 状态与导出（B4）

- `/data/status.tsv`：`port  pid  endpoint  prefix64  loc  state  epoch`（制表符分隔）。
- `/data/egress-ext.txt`：每行一个 `socks5://user:pass@HOST:PORT`，**原子 mv 重写**。
- `export-egress.sh --verify`：逐端口实测，校验唯一 `/64`、非 `2a09:` 拒绝、禁区 `/48` 拒绝、文件含 `#` 拒绝、有效行数必须等于 `POOL_SIZE`，失败即删文件并非零退出。
- 容器日志首轮完成后直接打印可复制的 16 行清单块。

---

## 4. 与网关的对接契约（本节是讨论重点）

### 4.1 清单格式（硬性，不可协商）

```
socks5://warpuser:WarpPass2026@NODE_PUBLIC_IP:1081
...
socks5://warpuser:WarpPass2026@NODE_PUBLIC_IP:1096
```

- 每行严格 `socks5://USER:PASS@IP:PORT`，共 16 行（= `POOL_SIZE`）。
- **禁止任何 `#` 字符**：网关源码对含 `#pool` 的节点在 `MarkFailure` 时直接 `return`，会**跳过 `limitedUntil` 的 15 分钟硬冷却**，破坏冷却语义。`export-egress.sh` 写出后强制校验，出现 `#` 立即报错删文件。
- SOCKS5 为 `socks5h` 语义：DNS 在节点侧解析，网关不需要能解析 IPv6。

### 4.2 冷却与重试（需网关确认现状）

| 参数 | 值 | 对齐原因 |
| --- | --- | --- |
| `warp.enabled` | `true` | 启用上游出口池 |
| `proxyfile` | `/var/lib/opencode2api/egress-ext.txt` | 本清单写入路径 |
| `max_anonymous_exits` | `8` | 16 端口 = 2× 冗余 |
| `retry.max_attempts` | `6` | 单端口失败立即换下一个 |
| `limitedUntil` | 15 分钟 | 与 `REROLL_INTERVAL=900s` 对齐：冷却到期时出口已换新 |

一个端口被网关冷却 15 分钟期间，池里仍有 15 个可用；满池换血每 15 分钟一次，理论上网关永远吃到“新号”。

### 4.3 文件刷新机制（**需要网关侧给出结论**）

- 我方在以下时机重写 `/data/egress-ext.txt`：首轮去重后、每 900s 换血后、健康检查修复后。
- 待确认：网关是“每次请求读文件”还是“启动读一次缓存”？若缓存，需要哪种重载（定时 ≤60s 重读 / fsnotify / SIGHUP）？
- 建议：每 ≤60s 重读该文件（文件极小，16 行）；或换血后由节点侧 rsync 下发 + 网关 fsnotify。
- 换血期间单个端口约 1–2 秒不可用，网关靠 `retry.max_attempts=6` 与多端口冗余吸收。

### 4.4 中心侧连通验证（接入后第一件事）

在任一中心容器内：

```bash
curl -sS -m 10 --socks5-hostname 'warpuser:WarpPass2026@NODE_PUBLIC_IP:1081' \
  https://opencode.ai/cdn-cgi/trace
```

期望回显包含 `ip=2a09:bac...`，且 `loc=` 为目标国家（JP/KR/GB/AU）。失败先查节点防火墙白名单。

### 4.5 节点防火墙（必须执行一次）

节点上以 root 运行：

```bash
sudo bash scripts/firewall-allow-center.sh
```

只放行三台中心机到 `1081–1096/tcp`：

- SIN `13.229.234.225`
- IAD `18.212.22.159`
- FRA `3.79.179.114`

安全模型是**双层**：SOCKS5 账密 + 源 IP 白名单。不要把端口裸奔到公网。

### 4.6 扩容与多节点（讨论项）

- 单节点固定 16 端口；横向扩容时新节点使用不同端口段（如 `START_PORT=2081` → `2081–2096`），清单直接合并为同一文件，网关无需改动。
- 中心三台机的出口 IP 若变化，需同步节点白名单（脚本可重复执行，幂等）。
- 建议至少 2 个节点（不同国家），单节点故障时池不归零。

---

## 5. 实测证据（沙箱节点，非纸面推演）

| 检查项 | 结果 |
| --- | --- |
| 镜像构建 | 通过，60.6 MB；wgcf 补丁编译通过；`wireproxy 1.0.8-dev` |
| 冷启动首轮 | 15/16（1 个 `/64` 撞车） |
| 健康修复 | 5 分钟后 **16/16**，禁区重叠 0 |
| 定时满池换血 | 900s 到达，全量 16 端口 4×4 批（5s 间隔） |
| 换血后瞬时 | 14/16（2 个 `/64` 撞车） |
| 健康收敛 | 5 分钟内自动修复 → **16/16** |
| 账号级故障恢复 | 人为破坏私钥：4 次换 Endpoint → 自动重注册（经 :1096 链式）→ ~30s 恢复上线 |
| 导出验收 | `16/16 unique IPv6 /64 prefixes, 0 failure(s)`，exit 0 |
| 清单格式 | 16 行、`#` 数量 0、账号行数 = POOL_SIZE |
| 禁区家族 | 全程 `deny-family overlap: 0` |
| 出口样例 | `2a09:bac5:b1bc:4aa`、`2a09:bac1:7680:de10`（`ip=2a09:bac5:b1bc:4aa::77:e6`） |

### 5.1 关于“16/16”的工程现实（重要，别当成 SLA）

实测机房可用 `/64` 只有约 **20 个**（`b1b8–b1bf × {26be,4aa}` 共 16 个，加 `7680/76a0/76c0/76e0:de10` 共 4 个）。16 个唯一前缀已接近该出口的物理上限，因此：

- 满池换血后短暂出现 **14–15/16** 是预期行为，**5 分钟内**健康检查会收敛回 16/16。
- 换机房/换国家后池容量会变化，验收一律以日志 `Active Unique /64 Pool Size` 为准。
- 网关侧应把“偶发 14–15/16 窗口”视为正常，而不是故障。

---

## 6. 已知限制与风险

- **WARP 免费策略可变**：Cloudflare 可能调整免费额度、限速或 `/64` 分配策略。
- **loc 由节点路由决定**：日本节点即 JP；测试沙箱在美西所以 `loc=US`，这是节点属性，不是代码问题。
- **`/64` 每连接随机**：同一账号换 Endpoint 会换 `/64`，因此去重必须实测探测；300s 健康检查兜底分配漂移。
- **SOCKS5 仅 TCP**：HTTP API 足够；UDP 场景不适用。
- **单节点故障域**：建议双节点起步。
- **`/data` 含 WireGuard 私钥**：备份时注意加密与权限；`docker compose down -v` 会销毁账号池。
- **换血抖动**：单端口重启约 1–2 秒不可用，4×4 分批已尽量平滑。

---

## 7. 从零到接入（操作清单）

```bash
# 1. 上传项目到节点
scp -r warp-pool-producer root@NODE_PUBLIC_IP:/opt/

# 2. 配置
cd /opt/warp-pool-producer
cp .env.example .env
# 编辑 .env：EXTERNAL_HOST=NODE_PUBLIC_IP（其余默认即可）

# 3. 构建并启动（受限网络加镜像源 build-arg，见 README）
docker compose up -d --build
docker logs -f warp-pool-producer

# 4. 等待日志出现
#    Active Unique /64 Pool Size: 16 / 16

# 5. 放行三台中心机（节点上 root）
sudo bash scripts/firewall-allow-center.sh

# 6. 导出并自检
bash scripts/export-egress.sh --host NODE_PUBLIC_IP --verify

# 7. 把 16 行写入中心容器
#    中心挂载/写入 /var/lib/opencode2api/egress-ext.txt（无任何 # 字符）

# 8. 中心容器验证（见 4.4）
```

网关侧保持 `warp.enabled = true`、`proxyfile` 指向该文件、`retry.max_attempts = 6`。

---

## 8. 排障速查

| 现象 | 处理 |
| --- | --- |
| n01 注册 429 | Cloudflare 同 IP 限速；等待后重启容器，已注册账号会复用 |
| `register-failed` | 看 `/data/instances/nXX/register.log` |
| `generate-failed` | 账号损坏；删该实例 `wgcf-account.toml` 后重启，或等恢复逻辑重注册 |
| `bad-prefix` / 非 `2a09:` | 看 `wireproxy.log`；确认节点出站 UDP `2408/500/1701/4500` 未被封 |
| 端口不监听 | `docker exec warp-pool-producer wireproxy -n -c /data/instances/nXX/wireproxy.conf` |
| 中心连不上 | 查 `EXTERNAL_HOST`、云安全组、`firewall-allow-center.sh` 是否执行 |
| `/data` 权限错误 | 用 named volume；bind mount 时 `chown` 给容器内 `warp` 用户 |
| 池子长期 <16 | 该机房 `/64` 池枯竭；换节点或接受较低 `POOL_SIZE` |

---

## 9. 对接讨论清单（给网关会话逐条过）

1. **刷新机制**：网关目前如何读取 `egress-ext.txt`？缓存则用什么方式重载（≤60s 定时 / fsnotify / SIGHUP）？
2. **冷却语义**：确认静态代理 429 → `limitedUntil` 15 分钟；确认清单禁止 `#pool` 标记。
3. **冗余参数**：`max_anonymous_exits` 是否维持 8（16 端口 2× 冗余）？`retry.max_attempts` 定 6 是否合适？
4. **收敛窗口**：满池换血后 5 分钟内 14–15/16，网关是否能平滑吸收（不产生用户可见失败）？
5. **多节点规划**：第二节点端口段（如 `2081–2096`）与国家选择。
6. **下发方式**：中心主动拉取 vs 节点 rsync 推送；文件权限与原子性。
7. **监控告警**：网关侧对每个 socks 出口的失败率/延迟采样，异常多久触发告警。

---

## 10. 附录

### 10.1 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `EXTERNAL_HOST` | 自动探测 | 写入清单的节点公网 IPv4（建议显式设置） |
| `START_PORT` | `1081` | 首个 SOCKS5 端口 |
| `POOL_SIZE` / `INSTANCE_COUNT` | `16` | 实例数（两变量等价） |
| `SOCKS_USER` / `SOCKS_PASS` | `warpuser` / `WarpPass2026` | SOCKS5 账密 |
| `REROLL_INTERVAL` | `900` | 全量换血周期（秒） |
| `REROLL_BATCH` | `4` | 换血每批实例数 |
| `DEDUP_RETRIES` | `4` | 单端口去重最大重试 |
| `REGISTER_RETRIES` | `3` | 单账号注册最大尝试 |
| `WGCF_KEEPALIVE` | `25` | WireGuard PersistentKeepalive |
| `PROBE_URL` | `https://opencode.ai/cdn-cgi/trace` | 探活地址 |
| `PROBE_TIMEOUT` | `6` | 探活超时（秒） |
| `HEALTH_INTERVAL` | `300` | 健康检查周期（秒） |
| `DENY_48_PREFIXES` | 见 `.env.example` | 禁止出现的存量 `/48` 家族 |

### 10.2 `status.tsv` 列

```text
port    pid    endpoint             prefix64              loc    state    epoch
1081    1234   162.159.192.1:2408   2a09:bac5:b1bc:4aa    JP     ok       1762527000
```

`state` ∈ `ok / bad / bad-prefix / register-failed / generate-failed / render-failed / config-failed / listen-failed / probe-failed`。

### 10.3 版本锚点

- wgcf：`v2.3.1`（+ `patches/wgcf-socks5-proxy.patch`，纯标准库）
- wireproxy：`github.com/windtf/wireproxy@35cce290db2f32d5e4316bd54102186f486d0af4`（自称 1.0.8-dev）
- 构建：`golang:1.26-alpine` → `alpine:3.20`
- 仓库 commit：见 GitHub `instacloud-warp/` 目录历史
