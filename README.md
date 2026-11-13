# net/ — 节点组与探活服务源码

这一目录收纳 InstaCloud `net` 分支上 5 个容器的可重建源码，以及全局服务映射。

原始部署有两个问题：

1. `node-us / node-eu / node-sg / node-sg-ws / checker` 在 InstaCloud 上的 `source` 字段为空（本地目录构建，没有 GitHub 绑定），镜像会随平台关停消失。
2. 能找到的两个节点模板（`wbi35772/InstaCloud-github`、`wbi35772/pella-node`）都没有绑定到服务，且各自内嵌了 Xray 配置与页面，属于一次性实验代码。

这里把两者合并成一个参数化镜像，并补上缺失的 checker。

## 服务映射

| 服务 | branch | 容器端口 | 卷 | 源码 | 状态 |
|---|---|---|---|---|---|
| node-us | net | 8443 | 无 | `xray-ws-node`（`PORT=8443`） | 重建，模板来源见下 |
| node-eu | net | 8443 | 无 | `xray-ws-node`（`PORT=8443`） | 重建 |
| node-sg | net | 8443 | 无 | `xray-ws-node`（`PORT=8443`） | 重建 |
| node-sg-ws | net | 8080 | `/data` | `xray-ws-node`（`PORT=8080 STATE_DIR=/data`） | 与原 `pella-node` 行为一致 |
| checker | net | 8199 | `/data` | `checker` | 原实现缺失，按接口契约重建 |
| xx | main | 8080 | 无 | **缺失** | 原仓库 `xx` 已不存在，建议弃用 |
| nodejs-argo | main | 3000 | 无 | 公开镜像 `eooce/nodejs-argo` | 可直接重建 |
| warp-us / warp-eu / warp-expand / warp-sg | warp-multi / probe-01 | 8081 | `/var/lib/opencode2api` | 仓库根目录 `Dockerfile` + `boot.sh` | 见根 README |
| warp-pool | warp-multi | 1081 | `/data` | `instacloud-warp/` | 见 `instacloud-warp/README.md` |

### 证据与推断

- **已确认**：原 `InstaCloud-github` 仓库的架构是 nginx `:8080`（静态页 + `/proxy` WS 转发）→ Xray VLESS+WS `127.0.0.1:8081`，路径 `/proxy`，固定 UUID。
- **已确认**：原 `pella-node` 是同一架构的 Node.js 版本，随机 UUID 持久化到 `uuid.txt`，并提供 `/healthz`、`/sub`、`/sub?format=clash`。
- **推断**：`node-sg-ws` 的 8080 + `/data` 卷与 `pella-node` 完全对应，因此 UUID 持久化文件在 `/data/uuid.txt`。
- **推断**：8443 的三个节点是同一镜像改了监听端口（平台边缘终结 TLS），因为容器声明的端口就是 8443，而模板里 Xray 只监听回环。
- **缺失**：`checker:8199` 的原始实现无法找回。本目录的 `checker/` 按最小可用契约重建（`/healthz` + `/api/check`，`/data/checks.jsonl` 留痕）；若旧实现有额外行为，需要解冻后导出镜像或卷再核对。

## xray-ws-node

一个镜像覆盖 4 个服务，靠环境变量切换：

| 变量 | 默认 | 含义 |
|---|---|---|
| `PORT` | `8080` | 前端监听端口（平台声明端口） |
| `XRAY_PORT` | `8081` | Xray 回环端口 |
| `WS_PATH` | `/proxy` | VLESS WS 路径 |
| `UUID` | 空 | 指定 UUID；留空则读 `/data/uuid.txt`，不存在就生成并持久化 |
| `STATE_DIR` | `/data` | UUID 与 Xray 配置存放目录 |
| `PUBLIC_PORT` | `443` | 生成节点链接时使用的公网端口 |
| `PUBLIC_TLS` | `1` | 节点链接是否使用 `security=tls`（平台边缘有 TLS 时为 1） |
| `XRAY_LOG_LEVEL` | `warning` | Xray 日志级别 |

端点：

- `/` 节点面板（按访问域名生成 vless 链接、订阅地址）
- `/healthz`、`/ping` 前端与 Xray 子进程状态
- `/sub` base64 vless 订阅
- `/sub?format=clash` Clash/Mihomo 订阅
- `/api/info` JSON 形式的本节点信息
- `/proxy` WebSocket 转发到 Xray

Xray 已固定为 `v26.11.9`（构建参数 `XRAY_VERSION`），不再运行时下载。

构建与运行：

```sh
docker build -t xray-ws-node:2.0.0 net/xray-ws-node

docker run -d --name node-sg-ws \
  -p 8080:8080 \
  -v node-sg-ws-data:/data \
  -e PORT=8080 -e STATE_DIR=/data \
  xray-ws-node:2.0.0
```

8443 节点把 `-p 8443:8443 -e PORT=8443` 换掉即可；不需要 `/data` 时可以不挂卷，UUID 每次重建会变，节点链接需重新生成。

## checker

| 变量 | 默认 | 含义 |
|---|---|---|
| `LISTEN` | `:8199` | 监听地址 |
| `STATE_DIR` | `/data` | `checks.jsonl` 写入目录 |
| `CHECKER_TOKEN` | 空 | 设置后 `/api/check`、`/api/info` 需要 Bearer 或 `X-Checker-Token` |
| `MAX_BATCH` | `64` | 单次批量上限 |
| `DEFAULT_TIMEOUT_MS` | `5000` | 默认超时 |

```sh
# 单目标 TCP
curl -sS 'http://127.0.0.1:8199/api/check?target=node-sg-ws.insta.internal:8080'

# 批量，带 HTTP 断言
curl -sS -X POST http://127.0.0.1:8199/api/check \
  -H 'Content-Type: application/json' \
  -d '{"targets":["node-sg-ws.insta.internal:8080","node-us.insta.internal:8443"],
       "scheme":"http","path":"/healthz","expect_status":200,"contains":"\"ok\":true",
       "concurrency":4,"timeout_ms":5000}'
```

响应：

```json
{"ok":true,"total":2,"passed":2,"failed":0,
 "results":[{"target":"...","scheme":"http","ok":true,"status":200,"latency_ms":12}]}
```

成功信号：`"failed":0` 且每个 `results[].ok` 为 `true`。

## 一起跑

```sh
docker compose -f net/docker-compose.example.yml up -d --build
curl -sS http://127.0.0.1:8080/healthz
curl -sS 'http://127.0.0.1:8199/api/check?target=127.0.0.1:8080'
```

## 不放进仓库的东西

- 线上实际 UUID、域名、平台 Token、控制台密码。
- 旧平台的镜像 digest（会随 ECR 一起消失，重建必须用本目录 Dockerfile）。
