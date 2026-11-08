# 双容器部署：主容器 + 静态池容器

> 分支：`warp-fusion`
> 适用：本地 WARP + 外置 SOCKS5 静态池（InstaCloud / 裸机 / Docker 均可）
> 原则：所有对接参数走环境变量或 `POOL_ENV_FILE`，仓库内不写死地址、端口、令牌。

## 1. 角色

| 容器 | 作用 | 关键入口 |
| --- | --- | --- |
| 主容器（gateway） | opencode2api 网关，WARP 池 + 静态池融合 | `boot.sh`（InstaCloud 卷启动）或镜像默认 entrypoint→wrapper |
| pool 容器（producer） | 16 个 wireproxy SOCKS5 出口 + 管理面 | `instacloud-warp/entrypoint.sh` + `socksmux` |

两种容器共用同一套契约：`socks5://warpuserNN:SOCKS_PASS@HOST:MUX_PORT`，每行一个健康节点。

## 2. pool 容器参数

| 变量 | 必需 | 说明 |
| --- | --- | --- |
| `API_TOKEN` | 建议 | 管理 API Bearer token；空=关闭管理 API |
| `MUX_PORT` | InstaCloud 必需 | 统一入口端口（SOCKS + HTTP）；裸机可不设 |
| `MUX_LISTEN` | 否 | 默认 `0.0.0.0` |
| `START_PORT` | 是 | wireproxy 起始端口；mux 模式必须大于 `MUX_PORT`（例如 1082） |
| `WIREPROXY_BIND_HOST` | 否 | mux 模式建议 `127.0.0.1` |
| `POOL_PUBLIC_HOST` | 公网清单必需 | 写入 `scope=public` 的主机名 |
| `POOL_PRIVATE_HOST` | 私网清单必需 | 写入 `scope=private` 的主机名；空则私网清单返回 409 |
| `POOL_ID` | 否 | 控制面展示名，默认主机名 |
| `SOCKS_USER` / `SOCKS_PASS` | 是 | 客户端与上游共用的 SOCKS 账密 |
| `INSTANCE_COUNT` / `POOL_SIZE` | 是 | 实例数，默认 16 |
| `REROLL_INTERVAL` | 否 | 换血周期，默认 900s，与网关硬冷却对齐 |
| `MIN_HEALTHY` | 否 | `/healthz` 通过阈值，默认 14 |

InstaCloud 示例（服务端口指向 `MUX_PORT`）：

```text
MUX_PORT=1081
START_PORT=1082
WIREPROXY_BIND_HOST=127.0.0.1
API_TOKEN=<secret>
POOL_PRIVATE_HOST=POOL_PRIVATE_HOST
POOL_PUBLIC_HOST=<service-public-tcp-host>
```

裸机 / host 网络示例（不需要控制面）：

```text
START_PORT=1081
INSTANCE_COUNT=16
EXTERNAL_HOST=<public-ip>
# MUX_PORT 留空
```

## 3. 主容器参数

### 3.1 pool-sync

`pool-sync.sh` 从 pool 控制面拉取清单，内容变化才替换 `EGRESS_FILE`，然后登录本地网关并 `POST /api/config/reload`。

| 变量 | 必需 | 说明 |
| --- | --- | --- |
| `POOL_API_URL` | 是 | pool 控制面地址，如 `http://POOL_PRIVATE_HOST:1081` |
| `POOL_API_TOKEN` | 是 | 与 pool 的 `API_TOKEN` 一致 |
| `POOL_SYNC_SCOPE` | 否 | `private`（默认）或 `public` |
| `EGRESS_FILE` | 否 | 默认 `$STATE_DIR/egress-ext.txt` |
| `POOL_ENV_FILE` | 否 | 默认 `$STATE_DIR/upgrade/pool.env`，启动时 source |
| `POOL_SYNC_RELOAD` | 否 | `1`（默认）变化后 reload；`0` 只拉取 |
| `POOL_SYNC_INTERVAL` | 否 | 循环间隔，默认 60s |
| `ADMIN_API` | 否 | 本地网关，默认 `http://127.0.0.1:8081` |
| `ADMIN_USER` / `ADMIN_PASS` / `ADMIN_PASS_FILE` | 是（reload） | 默认 `admin` / 空 / `$STATE_DIR/upgrade/admin.pass` |

### 3.2 boot.sh

适用于平台会重建根盘、只保留数据卷的部署（InstaCloud compute）。它：

1. 首次 `pool-sync.sh --fetch-only`；
2. 用 `jq '.warp.enabled=false'` 生成运行配置，先起静态池；
3. 后台等待 `/healthz`，登录后把 `warp.enabled=true` 写回并 reload（WARP 注册慢不阻塞健康检查）；
4. 后台跑 `pool-sync.sh --loop`；
5. `exec` 网关二进制。

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `STATE_DIR` | `/var/lib/opencode2api` | 数据卷 |
| `CONFIG_SOURCE` | `$STATE_DIR/config.json` | 源配置 |
| `CONFIG_PATH` | `$STATE_DIR/config-boot.json` | 运行时配置 |
| `O2A_BIN` | `$STATE_DIR/upgrade/opencode2api-app.new`，回退 `/usr/local/bin/opencode2api-app` | 网关二进制 |
| `POOL_SYNC_BIN` | 卷上 `upgrade/pool-sync.sh`，回退镜像 `/usr/local/bin/pool-sync.sh` | 同步脚本 |
| `POOL_SYNC_ENABLE` | `1` | 关闭设 0 |
| `WARP_ENABLE_ON_BOOT` | `1` | 关闭设 0 |
| `O2A_WEB_SAME_PORT` | `1` | 传 `-web-same-port` |
| `O2A_LISTEN` / `O2A_WEB_LISTEN` | 空 | 可选监听覆盖 |

## 4. 网关配置（融合模式）

```json
{
  "anonymous": true,
  "proxies": [],
  "proxyfile": "/var/lib/opencode2api/egress-ext.txt",
  "warp": {
    "enabled": true,
    "pool_size": 32,
    "stack": "ipv6",
    "direct_fallback": false
  },
  "performance": {
    "max_anonymous_exits": 8,
    "static_proxy_rate_limit_cooldown_seconds": 900
  }
}
```

`proxyfile` 必须与 `EGRESS_FILE` 一致；`proxies` 保持空数组，避免旧条目混入静态池。

## 5. 部署顺序

1. 推送 `warp-fusion` 分支。
2. 先部署 pool 容器（它不依赖主容器）：
   - 服务端口 = `MUX_PORT`（如 1081）；
   - 卷挂到 `STATE_DIR`（如 `/data`）；
   - 注入 `API_TOKEN`、`POOL_PRIVATE_HOST`、`POOL_PUBLIC_HOST` 等服务 secret/env；
   - 首次启动后确认 `/healthz`、`/api/v1/status` 16/16。
3. 部署主容器：
   - 卷保留 `config.json`、`egress-ext.txt`、`upgrade/`；
   - 启动命令用 `boot.sh`（镜像 `/usr/local/bin/boot.sh`，或卷上副本）；
   - 注入 `POOL_API_URL`、`POOL_API_TOKEN`（或写 `upgrade/pool.env`）。
4. 主容器启动后确认 `egress-ext.txt` 行数 = pool 健康节点数，`healthz` 静态池全绿。

## 6. 验收

```bash
# pool
curl -sS http://POOL_HOST:1081/healthz
curl -sS -H "Authorization: Bearer $API_TOKEN" \
  "http://POOL_HOST:1081/api/v1/status"
curl -sS -H "Authorization: Bearer $API_TOKEN" \
  "http://POOL_HOST:1081/api/v1/endpoints?scope=private" | wc -l

# pool SOCKS 抽测（用户名 warpuserNN 选择节点）
curl -sS --socks5-hostname "warpuser01:$SOCKS_PASS@POOL_HOST:1081" \
  https://opencode.ai/cdn-cgi/trace | sed -n 's/^ip=//p'

# 主容器
curl -sS http://MAIN_HOST:8081/healthz | jq '.proxies'
wc -l /var/lib/opencode2api/egress-ext.txt
tail -n 20 /var/lib/opencode2api/upgrade/pool-sync.log
```

放行判据：20 次请求 5xx ≤ 1，日志无 `anonymous_breaker_open`。

## 7. 故障排查

| 现象 | 检查 |
| --- | --- |
| pool `/healthz` 非 200 | `MIN_HEALTHY`、`/data/status.tsv`、`/data/socksmux.log` |
| 私网清单 409 | 未设 `POOL_PRIVATE_HOST` |
| 主容器清单 0 行 | `POOL_API_URL`/`POOL_API_TOKEN`、私网 DNS、`pool-sync.log` |
| 清单不更新 | pool 端节点非 `ok`；`endpoints` 只输出健康节点 |
| reload 失败 | `ADMIN_PASS_FILE`、`ADMIN_API`、CSRF、`/api/config/reload` 超时 |
| WARP 未挂回 | `WARP_ENABLE_ON_BOOT=1`、`boot.log` 的 login/reload 回显 |
