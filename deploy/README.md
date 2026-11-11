# Docker 部署（单仓库）

一个仓库、一份 compose、按 profile 分角色。核心机最少只需要两个容器：`warp-pool`（WARP 出口池 + 中继）和 `gateway`（OpenCode2API）。节点和 checker 按需另起。

## 一、角色一览

| 服务 | profile | 镜像 | 网络 | 端口 | 数据卷 |
|---|---|---|---|---|---|
| `warp-pool` | `core` | `o2a-warp-pool` | host | 1081 池 mux、11081 中继 mux、1578-1593 WARP、1594-1648 中继会话 | `data/pool` |
| `gateway` | `core` | `o2a-gateway` | host | 8080 API、8081 WebUI | `data/gateway` |
| `xray-node` | `edge` | `o2a-xray-node` | bridge | 8443 | `data/node` |
| `checker` | `tools` | `o2a-checker` | bridge | 8199 | `data/checker` |

`pool-init` 是一次性容器，只负责把 `data/pool` 的属主改成镜像里的 `warp` 用户，然后退出。

## 二、机器要求

- Docker 24+、Compose v2（`docker compose version` 有输出即可）
- 2 核 4GB 起步；跑满 55 个中继会话建议 8GB
- 公网 TCP：`8081` 必须，`1081` 只在需要远程网关时开放；`8443` 看节点
- 出站 UDP 必须放行：WARP `2408/500/1701/4500`，中继到上游服务器的端口
- 不能封 UDP，否则 WARP 池不可用（这是硬性前提）

## 三、首次部署（核心机）

```bash
git clone https://github.com/wbi35772/o2a.git
cd o2a/deploy
cp .env.example .env
```

编辑 `.env`，至少改这四项：

| 变量 | 说明 |
|---|---|
| `SOCKS_PASS` | 池 SOCKS 密码，随便设置一串强密码 |
| `POOL_API_TOKEN` | 池管理 API 的 Bearer token |
| `ADMIN_PASS` | 网关 WebUI 的 admin 密码（只在首次建卷时写入） |
| `RELAY_API_TOKEN` | 中继 mux 的 API token，保持 `147258hyh` |

可选：`RELAY_SUB_URL` 填订阅地址，中继启动后会自动拉节点。

启动：

```bash
docker compose --profile core up -d --build
```

首次必须带 `--build`：`pool-init` 复用 `warp-pool` 构建出来的镜像，镜像不存在时 compose 会拒绝创建。

预期结果：

- 构建 `o2a-warp-pool`、`o2a-gateway` 两个镜像
- `pool-init` 执行完退出（`Exited (0)`）
- `o2a-warp-pool`、`o2a-gateway` 常驻运行

## 四、验收

```bash
docker compose --profile core ps
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:1081/healthz
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:11081/healthz
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8081/healthz
```

三条都是 `200` 才算完整可用：

- `8081`（网关 WebUI）通常最先 `200`。
- `11081`（中继 mux）只要 mux 起来就是 `200`，没有配订阅也一样。
- `1081`（池 mux）在 16 个 WARP 出口达到 `MIN_HEALTHY` 之前返回 `503`，body 里是 `"status":"degraded"`。首次启动要等 `wgcf` 注册 + 探测跑完（几分钟）；一直 `503` 说明 UDP 出站被封或`PROBE_URL` 不可达。

看池导出的出口（需要 token）：

```bash
curl -s -H "Authorization: Bearer $POOL_API_TOKEN" \
  'http://127.0.0.1:1081/api/v1/endpoints?scope=private' | head
```

预期看到 `socks5://warpuser01:...@127.0.0.1:1081` 这类行。

看网关实际拿到的出口列表：

```bash
docker exec o2a-gateway wc -l /var/lib/opencode2api/egress-ext.txt
docker logs --tail 50 o2a-gateway
```

## 五、登录与配置

1. 浏览器打开 `http://服务器IP:8081`
2. 账号 `admin`，密码是 `.env` 里的 `ADMIN_PASS`
3. 配置页填 Zen key（匿名模式可跳过）
4. `SERVER_KEY` 已经写进 `server_keys`，客户端用这个 key 调 8080

说明：

- `config.seed.json` 只在 `data/gateway/config.json` 不存在时使用。之后改配置走 WebUI，改 `.env` 不再生效。
- 默认 `proxies: []`，即所有出站走池；想保留直连兜底，在 WebUI 里加回 `direct`。
- 网关启动后 `pool-sync` 每 60 秒拉一次池列表，`relay` 每 60 秒刷新中继列表，列表变化才热重载。

## 六、中继

两种方式，二选一：

**方式 A：订阅自动同步**

`.env` 里设置：

```env
RELAY_SUB_URL=https://订阅地址
```

重启生效：

```bash
docker compose --profile core up -d
```

**方式 B：手动放节点表**

文件：`data/pool/relay-root/relay/nodes.tsv`，每行 6 列，Tab 分隔：

```
label	host	port	sni	ws_path	password
```

改完等下一轮同步，或：

```bash
docker compose restart warp-pool
```

部署机上这个容器是独立实例，重启不会影响 InstaCloud 上的旧环境。

中继参数在 `data/pool/relay-root/relay/settings.env`，relay 每 5 秒重读一次，改文件不需要重启。

## 七、迁移旧数据

从 InstaCloud 导出两个卷后，在新机器上：

```bash
mkdir -p data/pool data/gateway
sudo tar -C data/pool -xzf warp-pool-data.tar.gz
sudo tar -C data/gateway -xzf gateway-state.tar.gz
docker compose --profile core up -d --build
```

注意：

- 卷里已有 `config.json` 时不会重新播种，`ADMIN_PASS` 不生效，登录密码以旧配置为准。
- `pool-sync` 热重载需要 `data/gateway/upgrade/admin.pass`，旧卷里通常已有；没有就用 `ADMIN_PASS` 生成一次后重启。
- 检查 `data/gateway/config.json` 的 `listen` 必须是 `0.0.0.0:8080`，旧配置写 `127.0.0.1:8080` 时公网访问不到，改成前者。
- 属主不用手动改：`pool-init` 和网关启动脚本会自动 chown。

必须带走的内容见根目录 `DEPLOY.md` 的“必须迁移的数据”一节。

## 八、边缘节点

在另一台机器上：

```bash
git clone https://github.com/wbi35772/o2a.git
cd o2a/deploy
cp .env.example .env
```

`.env` 只填边缘相关：

```env
EDGE_PORT=8443
EDGE_NODE_PORT=8443
EDGE_WS_PATH=/proxy
EDGE_PUBLIC_PORT=443
EDGE_PUBLIC_TLS=1
EDGE_UUID=
```

启动：

```bash
docker compose --profile edge up -d --build
```

- `EDGE_PUBLIC_TLS=1`：生成的分享链接是 `:443 + tls`，前面需要有反向代理或平台入口终止 TLS。
- 直接裸端口测试：设 `EDGE_PUBLIC_TLS=0`、`EDGE_PUBLIC_PORT=8443`，然后访问 `http://IP:8443/healthz`。
- `EDGE_UUID` 留空会生成并保存到 `data/node/uuid.txt`，迁移时把这个文件带走，否则客户端链接全部失效。

## 九、Checker

```bash
docker compose --profile tools up -d --build
curl -s http://127.0.0.1:8199/healthz
```

接口：`/healthz`、`/api/check`，结果追加到 `data/checker/checks.jsonl`。设置 `CHECKER_TOKEN` 后调用需要带 token。

## 十、升级

```bash
git pull
docker compose --profile core up -d --build
```

数据卷不受影响，`pool-init` 会再跑一次 chown。

## 十一、排错

| 现象 | 先看这里 |
|---|---|
| 端口被占用起不来 | host 网络直接用宿主端口，`ss -ltnp \| grep -E '1081\|8081\|11081'` |
| 池容器反复重启 | `docker logs o2a-warp-pool`，多半是 UDP 被封或 wgcf 注册被限流 |
| `egress-ext.txt` 是空文件 | 池没起来，或 `POOL_API_TOKEN` 与池的 `API_TOKEN` 不一致 |
| 网关报没有可用出口 | 同上，先确认 `curl .../api/v1/endpoints` 有输出 |
| 中继 mux 通了但没节点 | `RELAY_SUB_URL` 没配，或 `nodes.tsv` 为空 |
| 8081 打不开 | 网关是 host 网络，检查机器防火墙和云安全组 |

安全基线：

- `1081`、`11081` 不要直接暴露公网；确有远程网关需求时用防火墙白名单。
- `8081` 放公网就套一层 TLS 反向代理。
- `.env` 不进仓库，已由 `.gitignore` 忽略。
