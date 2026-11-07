# warp-pool-producer（会话 B）

在异国 Linux 节点批量生产 WARP 隧道：链式注册绕过同 IP 限速，强制纯 IPv6 出站，`/64` 去重，每 15 分钟换 Endpoint 重摇号，并以带账密认证的 SOCKS5 端口暴露给 opencode2api 网关。

## 契约

| 项目 | 值 |
| --- | --- |
| 端口 | `1081`–`1096`（16 个实例） |
| SOCKS5 认证 | `warpuser` / `WarpPass2026` |
| 出口 | 纯 IPv6（`AllowedIPs = ::/0`），禁止 IPv4 出口 |
| 去重 | 同容器内 16 个端口的 `/64` 前缀互不相同 |
| 存量 `/48` 禁区 | SIN `2a09:bac5:55*` / `2a09:bac1:65*`；IAD `2a09:bac5:c82*`；FRA `2a09:bac1:27b*` / `2a09:bac1:1e*` |
| 换血周期 | `900` 秒（15 分钟，对齐网关免费层 IP 屏蔽 `free_tier_block_seconds=900`） |
| 边界 | 不修改 opencode2api 任何代码 |

## 目录结构

```text
warp-pool-producer/
├── Dockerfile
├── docker-compose.yml
├── entrypoint.sh                  # 注册 / 渲染 / 去重 / 换血 / 导出
├── healthcheck.sh                 # 容器 HEALTHCHECK
├── endpoints.txt                  # 32 个 Cloudflare Anycast IP:PORT
├── patches/wgcf-socks5-proxy.patch
├── scripts/export-egress.sh       # 一键导出 + --verify 自检
├── scripts/firewall-allow-center.sh
├── .env.example
├── Makefile
└── README.md
```

## 前置条件

- 异国 Linux VPS（JP / KR / GB / AU），已安装 Docker 与 compose v2。
- 节点有公网 IPv4，供中心三台容器连接。
- 出站 UDP 到 Cloudflare Anycast 的 `2408 / 500 / 1701 / 4500` 未被封。
- 不需要宿主机 IPv6、不需要 TUN、不需要 `--privileged`。
- 必须部署在 Linux 上：`network_mode: host` 在 Docker Desktop（Windows/macOS）不适用。

## 快速开始

```bash
cp .env.example .env
# 编辑 .env，至少填写 EXTERNAL_HOST=节点公网IPv4
docker compose up -d --build
docker logs -f warp-pool-producer
```

首轮完成后日志会出现：

```text
Active Unique /64 Pool Size: 16 / 16
================= egress-ext.txt (copy to session A) =================
socks5://warpuser:WarpPass2026@NODE_PUBLIC_IP:1081
...
socks5://warpuser:WarpPass2026@NODE_PUBLIC_IP:1096
======================================================================
```

`egress-ext.txt` 只写入首轮去重后可用的端口；若不足 16 个，后续健康检查与换血会补齐并重写该文件。

受限网络构建（Alpine 源 / Go module 源）：

```bash
docker compose build \
  --build-arg APK_MIRROR_HOST=mirrors.aliyun.com \
  --build-arg GOPROXY_URL=https://goproxy.cn,direct
# 或写进 .env 后 docker compose up -d --build
```

放行中心三台容器的出站 IPv4：

```bash
sudo bash scripts/firewall-allow-center.sh
```

导出与自检：

```bash
bash scripts/export-egress.sh --host NODE_PUBLIC_IP --out egress-ext.txt
# --verify 会从 127.0.0.1 逐个探测，必须在节点本机执行
bash scripts/export-egress.sh --host NODE_PUBLIC_IP --verify
```

## 配置项

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `EXTERNAL_HOST` | 自动探测 | 写入清单的公网 IPv4；建议显式填写 |
| `START_PORT` | `1081` | 首个 SOCKS5 端口 |
| `POOL_SIZE` / `INSTANCE_COUNT` | `16` | 实例数（两变量等价，任一设置即可） |
| `SOCKS_USER` / `SOCKS_PASS` | `warpuser` / `WarpPass2026` | SOCKS5 账密 |
| `REROLL_INTERVAL` | `900` | 全量换血周期（秒），对齐网关免费层屏蔽窗口（`free_tier_block_seconds=900`） |
| `REROLL_BATCH` | `4` | 换血每批实例数 |
| `DEDUP_RETRIES` | `4` | 单个端口去重最大重试 |
| `REGISTER_RETRIES` | `3` | 单个账号注册最大尝试 |
| `WGCF_KEEPALIVE` | `25` | WireGuard PersistentKeepalive |
| `PROBE_URL` | `https://opencode.ai/cdn-cgi/trace` | 探活地址 |
| `PROBE_TIMEOUT` | `6` | 探活超时（秒） |
| `HEALTH_INTERVAL` | `300` | 健康检查周期（秒） |
| `ENDPOINTS_FILE` | 镜像内 `/usr/local/share/warp-pool/endpoints.txt` | Endpoint 池 |
| `DENY_48_PREFIXES` | 见 `.env.example` | 禁止出现的 `/48` 家族 |

## 运行机制

### B1 链式注册

- `n01` 使用容器原生网络直连 `wgcf register --accept-tos`。
- `n02` 起通过 `HTTPS_PROXY=socks5h://warpuser:WarpPass2026@127.0.0.1:LAST_WORKING_PORT` 借道前一个已探活端口的 WARP `/64` 注册。
- wgcf 原生 uTLS 传输会绕过 `HTTPS_PROXY`，镜像构建时应用 `patches/wgcf-socks5-proxy.patch`：TCP 连接走 SOCKS5，uTLS 握手仍端到端到 Cloudflare。
- 失败处理：删除损坏的 `wgcf-account.toml`，等待 15 秒重试；链式连续失败后回退直连。
- 账号保存在 `/data/instances/nXX/`，容器重启复用，不重复注册。

### B2 纯 IPv6 `wireproxy.conf`

由 `wgcf-profile.conf` 渲染，逐项过滤：

- 删除 `172.16.0.2/32`、`1.1.1.1, 1.0.0.1`、`0.0.0.0/0`。
- `Address` 只保留 IPv6 地址（不依赖原文件顺序）。
- `DNS` 只保留 IPv6；若过滤后为空，插入 `2606:4700:4700::1111, 2606:4700:4700::1001`。
- `AllowedIPs` 强制为 `::/0`。
- `Endpoint` 替换为随机 Anycast `IP:PORT`。
- 追加带认证的 `[Socks5]` 段（`BindAddress` / `Username` / `Password`）。

### B3 去重与换血

- `endpoints.txt` 提供 32 个组合（8 个 Anycast IP × 4 个端口），启动时同容器内不重复分配。
  - `162.159.193.1` 已于 2026-11-07 移除：同一已知可用账号对该地址的 4 个端口全部握手失败，其余 8 个地址全部通过。
- 探活：`curl -x socks5h://... https://opencode.ai/cdn-cgi/trace`，取 `ip=` 前 4 段作为 `/64`。
- 前缀为空、前缀非 `2a09:`、命中禁区 `/48`、或与其他端口重复 → 更换 Endpoint 并重启该实例，最多 4 次。
- 4 次换 Endpoint 仍失败且前缀为空/非法（账号级故障）→ 删除账号重新注册一次再复探（`recover_account`）；仅因 `/64` 重复则不再重注册，留给下一轮换血。
- 每 `900` 秒全量换血（每批 4 个，批间 5 秒），与网关 15 分钟免费层屏蔽窗口对齐。
- 每 `300` 秒健康检查：探活失败或 `/64` 与其他端口重复先换 Endpoint；仍失败则完整 bring-up（必要时重新注册）。

### B4 接入清单

- 文件：`/data/egress-ext.txt`（每行一个 `socks5://user:pass@HOST:PORT`）。
- 控制台：首轮去重后打印可直接复制的清单块。
- 状态：`/data/status.tsv`（端口、PID、Endpoint、`/64`、`loc`、状态、时间戳）。

## 验收（Definition of Done）

1. 节点直连显示目标国家：

   ```bash
   curl -4 -sS https://www.cloudflare.com/cdn-cgi/trace | grep '^loc='
   ```

   应为 `JP` / `KR` / `GB` / `AU`，不能是 `SG` / `US` / `DE`。

2. 池规模：

   ```bash
   docker logs warp-pool-producer 2>&1 | grep 'Active Unique'
   ```

   输出 `Active Unique /64 Pool Size: 16 / 16`。冷启动需要顺序注册 16 个账号，通常 1–3 分钟；账号已持久化的热启动通常 60 秒内完成。

3. 抽检 IPv6 出口：

   ```bash
   curl -sS -m 10 --socks5-hostname 'warpuser:WarpPass2026@NODE_PUBLIC_IP:1081' \
     https://opencode.ai/cdn-cgi/trace | grep '^ip='
   ```

   必须为 `2a09:bac...` 形式的 IPv6。

4. `/48` 不重叠：`--verify` 会打印每个端口的 `/48` 并拒绝禁区家族。

## 跨会话契约 v1.1（硬性）

- 清单每行严格为 `socks5://USER:PASS@IP:PORT`，**绝不能**带任何 `#` 字符。网关会把含 `#pool` 的条目识别为动态池：不参与失败冷却（`MarkFailure` 直接返回）、每次尝试新建连接、并在池内循环抽取；只有纯静态条目才会进入失败冷却（15s 起、逐次翻倍、上限 8 倍）与免费层屏蔽（默认 900s）。
- `export-egress.sh` 写出后强制校验：文件出现 `#`、或有效 `socks5://` 行数不等于 `POOL_SIZE`，立即报错、删除输出并以非零退出。
- 默认 `POOL_SIZE=16`（端口 `1081~1096`，是 `max_anonymous_exits=8` 的两倍冗余），`REROLL_INTERVAL=900` 秒，与网关免费层屏蔽窗口（默认 900s）对齐。

## 交给会话 A

1. 将 `egress-ext.txt` 的 16 行写入中心容器 `/var/lib/opencode2api/egress-ext.txt`；文件内不得出现任何 `#` 字符。
2. 中心侧连通性验证：

   ```bash
   curl -sS -m 10 --socks5-hostname 'warpuser:WarpPass2026@NODE_PUBLIC_IP:1081' \
     https://opencode.ai/cdn-cgi/trace
   ```

3. 确认防火墙已放行 `13.229.234.225`、`18.212.22.159`、`3.79.179.114`。
4. 网关侧：`warp.enabled` 必须为 `false`——设为 `true` 时免费模型流量全部改走网关内置 WARP，`proxyfile` 静态池会被完全绕过。`proxyfile` 指向该文件；`performance.max_anonymous_exits = 8`（16 端口 = 2× 冗余）；`performance.free_tier_block_seconds = 900`。
5. `proxyfile` 只在配置加载/管理端 Reload 时读取（网关没有文件监视）。本清单 URL 恒定（出口在端口后漂移），换血无需重载；仅当节点公网 IP 或端口变化时，在 WebUI 保存配置或调用管理端 Reload。

## 运维

- 状态：`docker exec warp-pool-producer cat /data/status.tsv`。
- 日志：`docker logs -f --tail=200 warp-pool-producer`。
- 持久化：named volume `warp-pool-data`；`/data` 内含账号私钥，注意保密。
- 重启：`docker compose restart`，账号复用，不重新注册。
- 扩容：同时调整 `INSTANCE_COUNT` 与 `START_PORT`，并确保 `endpoints.txt` 行数不少于实例数。
- 换节点：新节点冷启动后，把新清单追加给会话 A，并同步防火墙白名单。

## 故障排查

| 现象 | 处理 |
| --- | --- |
| `n01` 注册 429 | Cloudflare 同 IP 限速约 10 分钟；等待后 `docker compose restart`，已注册账号会复用 |
| `register-failed` | 查看 `/data/instances/nXX/register.log` |
| `generate-failed` | 账号可能损坏；删除该实例 `wgcf-account.toml` 后重启容器 |
| `bad-prefix` / 非 `2a09:` | 查看 `wireproxy.log`；确认节点出站 UDP 未被封；去重逻辑会自动换 Endpoint |
| 端口不监听 | `docker exec warp-pool-producer wireproxy -n -c /data/instances/nXX/wireproxy.conf` |
| 中心连不上 | 检查 `EXTERNAL_HOST`、安全组、`firewall-allow-center.sh` 是否执行 |
| `permission denied: /data` | 使用 named volume；bind mount 时把目录 `chown` 给容器内 `warp` 用户 |
| Docker Desktop 上端口异常 | 本项目要求 Linux 主机 + host 网络 |

## 红线

- 不产生 IPv4 出口：`AllowedIPs` 仅 `::/0`，探活拒绝非 `2a09:` 前缀。
- 不与 SIN / IAD / FRA 的 `/48` 家族重叠。
- 不修改 opencode2api 的任何代码。
- 端口必须同时受 SOCKS5 账密与中心 IP 白名单保护，不要裸奔到公网。

## 已知限制

- WARP 免费额度与 Cloudflare 策略可能变化；账号被限流时需等待或更换节点。
- 实际落地 Colo 由节点路由决定，以 `cdn-cgi/trace` 的 `loc` 为准。
- 冷启动受注册限速影响，可能超过 60 秒。
- SOCKS5 仅承载 TCP；对 HTTP API 足够。

## 本地验证记录

在 Docker 29.9.2 / golang:1.26-alpine / alpine:3.20 上实测：

| 检查 | 结果 |
| --- | --- |
| `docker build`（打补丁的 wgcf + 固定 commit 的 wireproxy） | 通过，`wireproxy version 1.0.8-dev` |
| `wgcf register` 直连注册 | 通过，生成 `wgcf-account.toml`（单引号 TOML 已被 `account_valid` 接受） |
| `render_conf` | `Address` 仅保留 IPv6、`DNS` 仅 IPv6、`AllowedIPs = ::/0`、Endpoint 已替换、追加 `[Socks5]` |
| 单实例 SOCKS5 探活 | `ip=2a09:bac5:21b3:281e::3ff:c`，/48=`2a09:bac5:21b` |
| 链式注册（`HTTPS_PROXY=socks5h://...@127.0.0.1:1081`） | 通过，n02 账号有效并可 `generate` |
| 2 实例 entrypoint | `Active Unique /64 Pool Size: 1 / 2`，`egress-ext.txt` 只写健康端口 |
| 探活链路 | `ip=2a09:bac5:113:3464::538:79 colo=SYD loc=AU` |
| 契约 v1.1 清单格式 | 16 行进/出 1081–1096，`#` 行数 0，导出脚本自校验通过 |
| 契约 v1.1 默认值 | 容器内 `POOL_SIZE=16`、`INSTANCE_COUNT=16`、`REROLL_INTERVAL=900`；清空环境变量回退仍为 16/16/900 |

