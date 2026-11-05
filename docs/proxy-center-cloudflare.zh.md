# 代理中心：Cloudflare 出口源

日期：2026-11-05
分支：`warp-stability`
范围：`internal/admin/cfsource.go`、`internal/admin/egress.go`、`internal/exitprobe`、`proxyctl`、`webui/js/pages/proxy.js`

## 1. 它解决什么

Clash 订阅只能提供别人打包好的节点，节点坏了、出口被烧了都没法换。
「Cloudflare 出口」把**自己的 Worker 部署参数**直接变成节点：

```
部署参数（UUID/HOST/PROXYIP/SOCKS5）
  → 每个出口参数生成一个 VLESS WS 节点
  → 写入本地 provider（cf-<id>）+ 订阅文件
  → 同步订阅 → mihomo 本地 SOCKS 监听
  → 额度探测确认该出口还能不能出免费模型
```

不依赖 `workers.dev` 拉订阅（容器所在网络经常拉不动），入口和出口分开配置。

## 2. 面板位置

`配置中心 → 代理 → 代理中心 → Cloudflare 出口`

- **节点与出口**：节点表新增「出口 IP」「免费额度」两列，可「探测当前筛选」「探测勾选额度」。
- **Cloudflare 出口**：出口源表单 + 列表（名称 / Worker / 出口数 / 节点数 / 上次生成 / 错误 / 生成 / 删除）。

## 3. 字段

| 字段 | 说明 |
| --- | --- |
| 名称 | 面板展示名 |
| Worker 域名 | 节点的 Host/SNI，例如 `edgetunnel-full.<subdomain>.workers.dev` |
| UUID | Worker 的 VLESS UUID |
| 端口 | 通常 443 |
| WS 路径模板 | `/proxyip=<ip>` 或 `/socks5=<host:port>`；`<ip>` 会被出口参数替换 |
| 出口参数 | 每行一个。`ip:port` 走 `/proxyip=`，`socks5://user:pass@host:port` 走 `/socks5=` |
| 边缘 IP | 可选，每行一个。留空则在 Cloudflare 默认段上轮询 |
| 启用 | 关闭后同步时不再生成该源的节点 |

生成的节点名：`cf-<源id前8位>-<序号>`；provider id：`cf-<源id>`。

### 路径模板对应关系

- 模板含 `<ip>` → 出口参数按原样填入，`/proxyip=1.2.3.4:443`
- 模板含 socks5 占位 → `/socks5=user:pass@host:port`

一个出口参数 = 一个节点。源保存后到「节点与出口」点「同步订阅」才会进入 mihomo。

## 4. 从部署参数导入

支持两种输入，点「解析部署参数」自动填表：

```
UUID=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
HOST=edgetunnel-full.<subdomain>.workers.dev
KEY=...                # 只用于识别，不写入节点
PROXYIP=1.2.3.4:443,5.6.7.8:443
SOCKS5=user:pass@host:1080
```

或直接贴 JSON。重复键会合并（`PROXYIP` 多行等价逗号分隔）。
`KEY`/`TOKEN` 等敏感值不会进入节点配置；导出的订阅文件只有 UUID、Host、路径。

## 5. 额度探测

`POST /api/proxy/egress`（8 并发，异步任务）对选中/筛选后的节点：

1. 通过该节点自己的本地 SOCKS 端口发起请求（不是直连）；
2. 先取出口 IP（`ipify`，失败回退 Cloudflare trace）；
3. 用匿名车道同款请求打一次免费模型：
   `POST https://opencode.ai/inference/openai/v1/chat/completions`
   `Authorization: Bearer public`、`x-opencode-client: cli`、
   UA `opencode/latest/2.1.1/cli`、canonical session 头、
   `x-opencode-project` 40 位 hex、无 `x-opencode-request`、`stream:true`；
4. 结果写回节点：

| quota | 含义 |
| --- | --- |
| `ok` | 免费模型有输出 |
| `limited` | 429 `FreeUsageLimitError`（附 `retry_after`） |
| `blocked` | 403/地区或风控拦截 |
| `error` | 连接失败、EOF、非 relay 等 |
| `unknown` | 其他状态码 |

字段：`egress_ip` / `quota` / `quota_note` / `retry_after` / `quota_checked_at`。
清理或合并监听器时保留这些字段（`mergeListeners`）。

## 6. 同步与容错

`proxyctl` 拉 provider 时：

- 本地文件 provider（`/var/lib/opencode2api/.../cf-*.yaml`）直接复制，不走网络；
- 远程订阅缓存到 `proxycenter/sub-<provider-id>.yaml`；
- 单次 wget `-t 1 -T 15 --dns-timeout=10`，失败回退缓存；
- 删除 `providers.txt` 里已不存在的 `sub-*.yaml/.json`（防止旧节点污染）；
- 同步任务锁内不持 `proxyMu`（10 分钟超时），避免卡死面板。

## 7. 部署（InstaCloud）

服务 `ap-sg`，分支 `warp-test`，卷 `/var/lib/opencode2api`，启动命令：

```
/usr/local/bin/opencode2api -config /var/lib/opencode2api/config.json -web-same-port
```

重部署必须带端口，否则平台按 Dockerfile 探测成 8080，而 `-web-same-port` 只监听 8081，会部署失败并回滚失败：

```bash
insta compute connect-repo <owner/repo> ap-sg \
  --repo-branch warp-stability --branch warp-test \
  --no-auto-deploy --port 8081
```

部署后自检：

```bash
curl -sS https://<service-url>/healthz
curl -sS -b cookie https://<service-url>/api/proxy | jq .settings
```

`settings.test_timeout_seconds` 存在即说明是新构建。

## 8. 已知限制（2026-11-05 实测）

- 公共 relay 池（bestproxy / cmliu colo）已整体被烧：
  - `bestproxy` 条目不是合法 edgetunnel relay，探测 `error`（EOF / 非 relay TLS）；
  - `cmliu colo` 返回 `limited`，示例出口 `104.28.167.56`、`159.65.1.74`，`retry_after` 约 12 小时；
  - 容器直连出口 `54.255.213.127` 同样 429。
- 因此 `/v1/chat/completions` 仍会返回 `503 all proxies are unavailable`；
  功能链路本身可用，缺的是**未被烧的出口**（自建 WARP / 独立账号 Workers / 住宅代理），
  接入后在「Cloudflare 出口」里加一行出口参数并重新探测即可。

## 9. 自建优选 IP（边缘入口扫描）

面板里每个 Cloudflare 出口行都有「优选 IP」按钮，`POST /api/proxy/cf/{id}/scan`，
不再依赖社区整理的 `bestcf` 列表。

流程（`internal/edgescan`）：

1. 从 Cloudflare 官方公布的 15 个 IPv4 段随机采样，按 /24 去重；
2. 对每个候选做 TCP 443 + TLS 握手，SNI 固定为 Worker 主机名；
3. 复用已部署 Worker 的验收口径：`GET /version?uuid=<UUID>` 必须 200 且 body 含 `Version`；
4. 按握手+响应耗时排序，保留最快 N 个，写回该出口的 `edge` 并重建 manifest；
5. 扫描成功后自动触发一次 `sync`，新入口立即进入 fleet（有任务在跑时返回 `sync_started=false`）。

请求体与默认值：

```json
{"candidates": 200, "timeout_ms": 1500, "keep": 8}
```

- `candidates`：1–1000，默认 200；每个 /24 只保留一个样本，避免同一个 anycast 集群刷屏。
- `timeout_ms`：500–5000，默认 1500；单地址上限，200 个候选在 32 并发下约 10 秒完成。
- `keep`：1–32，默认 8；按延迟升序写入 `edge`。

返回：

```json
{"source": {...}, "report": {"attempted": 200, "reachable": 37, "duration_ms": 8123,
 "hits": [{"ip": "104.16.x.x", "latency_ms": 42}]}, "sync_started": true}
```

注意：优选 IP 只优化**入口**（延迟与可达性），不改变免费额度的归属；额度仍按 Worker
`/proxyip=`、`/socks5=` 决定的**出口 IP** 计算。入口再好也不能把已烧的出口救回来。

## 10. 出口方式（代理池 / WARP / 直连）

代理中心顶部有「出口方式」选择器，`PUT /api/proxy/egress-mode`，四种互斥模式：

| 模式 | 免费模型 | Go / 付费 | config.json 结果 |
| --- | --- | --- | --- |
| `proxy` | 代理池 | 代理池 | `warp.enabled=false`，`proxyfile=<cfnew>/proxies.txt` |
| `warp` | 内置 WARP | 代理池 | `warp.enabled=true`，`registration_proxies=[]`，`proxyfile` 保留 |
| `warp-only` | 内置 WARP | 容器直连 | `warp.enabled=true`，`registration_proxies=<proxies.txt>`，`proxyfile=""` |
| `direct` | 容器直连 | 容器直连 | `warp.enabled=false`，`registration_proxies=[]`，`proxyfile=""` |

切换由 `proxyctl mode` 写 `config.json` 后同步 `Reload`；写入失败会回滚到上一个模式。
旧的 `warp-farm.txt` 标记仍然兼容（无显式模式时等价 `warp-only`），但显式切换会删除该标记。

