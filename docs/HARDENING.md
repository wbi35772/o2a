# 安全基线自审与加固记录

日期：2026-11-05
分支：`warp-stability`（基线 `a5f8d77`）

## 1. 基线状态

加固前的本地基线全部通过：

- `go build ./...`
- `go vet ./...`
- `gofmt -l .`（无输出）
- `go test ./...`（当时仓库没有任何 `*_test.go`）

部署形态：容器内 `opencode2api -web-same-port`，API 与 WebUI 共用公网端口，前置 InstaCloud edge 反向代理。

## 2. 发现与处理

| # | 问题 | 位置 | 处理 | 状态 |
| - | ---- | ---- | ---- | ---- |
| 1 | 登录/Playground 限流按 `RemoteAddr` 计数；在反向代理后所有客户端共用一个桶，攻击者可用 5 次失败把所有人锁在登录外；同时不能直接信任 `X-Forwarded-For`，否则可轮换伪造 IP 绕过限流 | `internal/admin/auth.go` | 新增可信代理链解析：仅当直连 peer 属于回环/私网（或 `TRUSTED_PROXIES` 指定网段）时才读取 XFF，并从右向左跳过可信代理取第一个不可信跳点；`TRUSTED_PROXIES=none` 可完全关闭 | 已修复，有测试 |
| 2 | 修改密码、查看密钥等密码复核接口没有频率限制；argon2id 每次占用 64 MiB，并发请求可造成内存放大 | `internal/admin/auth.go`、`internal/admin/server.go` | 复核类接口独立限流 5 次/客户端/5 分钟；所有密码校验经全局互斥串行化，避免并发 64 MiB 峰值 | 已修复，有测试 |
| 3 | CSP 缺少 `base-uri`、`form-action`、`frame-ancestors`、`object-src`；`index.html` 的内联主题脚本被 `script-src 'self'` 拦截（浏览器控制台可见），主题初始化失效 | `internal/admin/server.go`、`webui/index.html`、`webui/js/theme-init.js` | 内联脚本外置为 `/js/theme-init.js`；CSP 补齐指令；HTTPS 响应增加 HSTS | 已修复，有测试 |
| 4 | `config.json`、`config.json.bak`、代理中心 `providers.txt`/`selected.txt`/`state.json` 等可能以 0644 落盘，包含上游密钥、WebUI 密码哈希和订阅 token | `internal/config/persistence.go`、`internal/admin/proxy.go`、`docker-entrypoint.sh`、`proxyctl` | 配置与备份钳制到 0600；代理中心文件 0600、目录 0700；入口脚本 `chmod 700/600`；`proxyctl` 设置 `umask 077` 并收紧已有文件 | 已修复，有测试 |
| 5 | `-web-same-port` 下前端 `derivedAPIBase()` 仍从 `listen` 推导端口，展示的 API 地址会带错误的 `:8081` | `webui/js/api.js`、`internal/admin/server.go`、`cmd/opencode2api/main.go` | 后端在 `effective` 中暴露 `same_port`，前端同端口时直接使用 `location.origin` | 已修复 |
| 6 | 仓库没有任何自动化测试，回归只能靠手工 | `internal/**` | 新增认证限流/可信代理解析、CSP/HSTS、配置权限三类测试 | 已补充 |
| 7 | 管理/WebUI 服务器只设了 `ReadHeaderTimeout`，慢速请求体可长期占用连接；Header 默认上限 1 MiB 偏大 | `cmd/opencode2api/main.go` | 增加 `ReadTimeout` 60s 与 `MaxHeaderBytes` 64 KiB；不设 `WriteTimeout`，避免截断 SSE 和长响应 | 已修复 |
| 8 | 镜像 `HEALTHCHECK` 固定探 `8080`，当前 `-web-same-port` 部署监听 `8081`，镜像健康检查会一直失败 | `Dockerfile` | 先探 `8080`，失败再探 `8081` | 已修复 |

## 3. 未处理项

以下问题记录在案，本轮未改动：

- 日志 SSE 没有每会话连接数上限，已登录管理员可开大量长连接。
- CSRF 的 `Origin` 校验仍信任 `X-Forwarded-Proto`（任意来源可声明），实际防护依赖 `SameSite=Strict` + 会话内 CSRF token。
- 代理订阅 URL 支持任意 http(s) 地址，属于管理员功能，未加目标白名单（SSRF 面仅限管理员）。
- 管理 API 未开启 CORS；浏览器跨域直连不支持，服务端/CLI 调用正常。

## 4. 新增环境变量

`TRUSTED_PROXIES`：逗号分隔的 CIDR 列表，例如
`TRUSTED_PROXIES=10.0.0.0/8,172.16.0.0/12`。
默认信任回环与私网；设为 `none` 则忽略所有 `X-Forwarded-For`，全部按直连地址限流。
Compose 中通过 `OPENCODE2API_TRUSTED_PROXIES` 传入。

## 5. 验证

本地（WSL + Go 1.24.6）：

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .
```

结果：全部通过；`internal/admin` 与 `internal/config` 测试通过。

运行时冒烟（同一二进制、临时配置、`-web-same-port`）：

- `GET /healthz` 返回 200。
- `GET /` 返回 CSP，且包含 `base-uri 'none'`、`frame-ancestors 'none'`。
- `GET /js/theme-init.js` 返回 200，首页不再有内联脚本。
- 登录返回 200 并下发 `HttpOnly; SameSite=Strict` 会话 cookie。
- `GET /api/config` 的 `effective.same_port` 为 `true`。

## 6. 兼容性

- `admin.New` 增加了 `samePort` 参数，仅内部调用，已同步 `main.go`。
- `EffectiveView` 新增 `same_port` 字段，旧前端会忽略它。
- 文件权限收紧后，若同机其他用户依赖读取 `config.json`，需要显式放开；这不是支持的使用方式。
- 可信代理默认值覆盖常见容器/私网；如果 edge 使用公网地址回源，需要把它加入 `TRUSTED_PROXIES`，否则限流仍按 edge 地址计数。
