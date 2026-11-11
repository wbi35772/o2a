# 部署说明（单分支 main）

本仓库是 OpenCode2API 部署栈的唯一源码源。长期只保留 `main` 一个分支；旧的实验线已删，只留两个只读标签 `legacy/free-only`、`legacy/instacloud-warp`。

## 构建单元

| 单元 | 目录 | 建议镜像名 | 端口 | 数据卷 | 备注 |
|---|---|---|---|---|---|
| 网关 | `/`（根 Dockerfile） | `o2a-gateway` | 8080 API / 8081 WebUI | `/var/lib/opencode2api` | Go + mihomo；卷里是配置、admin 密码、池凭证 |
| WARP 池 + 中继 | `instacloud-warp/` | `o2a-warp-pool` | 1081 mux（SOCKS/API/控制台）、11081 中继 mux、1594–1648 会话 | `/data` | `network_mode: host`；必须放行 UDP 出站 |
| Xray 节点 | `net/xray-ws-node/` | `o2a-xray-node` | 8080（node-sg-ws）或 8443（node-us/eu/sg） | `/data` | Node 前门 + Xray VLESS/WS `/proxy`；TLS 由入口终止 |
| 健康检查 | `net/checker/` | `o2a-checker` | 8199 | `/data` | 纯标准库 Go；`/healthz`、`/api/check` |

## 平台最低要求

1. Docker（或直接跑镜像）+ 持久卷（池 ≥1GiB）。
2. **UDP 出站**：wireproxy → Cloudflare WARP（如 `162.159.192.1` 的 UDP 2408/500/1701/4500）；中继 → 上游 trojan 节点（TCP 443 + WS）。封 UDP 时池不可用。
3. 公网 TCP：池 `1081`、网关 `8081`（`8080` 可选）、节点端口（8080/8443）、checker `8199`（可只内网）。
4. 中继会话端口 `1594–1648` 与 mux `11081` 只在容器内或同机使用，不需要公网。
5. 池和网关可以分机；网关只需能访问池的 `1081`。

## 构建

```sh
docker build -t o2a-gateway .
docker build -t o2a-warp-pool ./instacloud-warp
docker build -t o2a-xray-node ./net/xray-ws-node
docker build -t o2a-checker ./net/checker
```

网关（最简）：

```sh
docker run -d --name o2a-gateway -p 8080:8080 -p 8081:8081 \
  -v o2a-state:/var/lib/opencode2api o2a-gateway
```

池（host 网络，完整环境变量见 `instacloud-warp/docker-compose.yml`）：

```sh
docker run -d --name o2a-warp-pool --network host --restart unless-stopped \
  -e SOCKS_PASS='CHANGE_ME' -e INSTANCE_COUNT=16 -e START_PORT=1081 \
  -v o2a-pool:/data o2a-warp-pool
```

节点组参考 `net/docker-compose.example.yml`。

## 必须迁移的数据

| 卷 | 关键文件 | 丢失后果 |
|---|---|---|
| 池 `/data` | `relay/settings.env`、`relay/nodes.tsv`、`relay/instances/*/wgcf-account.toml`、`control/tokens.json`、`control/upstreams.json`、`status.tsv` | 订阅与 Token 重配；wgcf 账号可重建，但 55 连发会触发 Cloudflare 限流 |
| 网关 `/var/lib/opencode2api` | `config.json`、`upgrade/pool.env`、`upgrade/admin.pass`、`proxycenter/state.json`、`egress-ext.txt` | 等于全新网关 |
| 节点 `/data` | `uuid.txt`、`xray-config.json` | UUID 变化，客户端链接全部失效 |
| checker `/data` | `checks.jsonl` | 可丢 |

## 环境要点

- 池：`SOCKS_PASS` 必填；`SOCKS_USER`（默认 warpuser）、`INSTANCE_COUNT`/`POOL_SIZE`、`START_PORT`（默认 1081）、`MUX_PORT`、`API_TOKEN`、`EXTERNAL_HOST`/`POOL_PUBLIC_HOST`/`POOL_PRIVATE_HOST`/`POOL_ID`。控制台 `POOL_ADMIN_USER`/`POOL_ADMIN_PASSWORD` 上线前必须改。
- 中继：运行期读 `/data/relay/settings.env`（`RELAY_SUB_URL`、`RELAY_SYNC_INTERVAL`、`RELAY_MAX_AUTO`），节点表自动同步；不要把节点写死进镜像。
- 网关：`CONFIG_PATH=/var/lib/opencode2api/config.json`、`STATE_DIR=/var/lib/opencode2api`；`config.json` 与 `upgrade/pool.env` 从卷加载。
- Xray 节点：`PORT`、`XRAY_PORT`、`WS_PATH`、`UUID`（不填则持久化随机生成）、`PUBLIC_PORT`、`PUBLIC_TLS`。

## 仓库约定

- 只从 `main` 构建，不再建 `warp-*` 部署分支。
- 不提交：`.env`、`settings.env`、`tokens.json`、`wgcf-account.toml`、`*.exe`。
- 历史设计文档在 `docs/`，旧报告在 `docs/archive/`。
