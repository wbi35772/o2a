# instabox-deploy（协调仓）

整个 Instabox 部署体系的调度与协调仓库。**main 只做统筹，不放任何项目业务代码。**

## 分支模型

每个项目一个独立分支，整棵树独立可构建、可单独克隆；main 只维护系统级契约与编排。

| 分支 | 项目 | 内容 |
|---|---|---|
| `main` | 系统协调 | 分支索引、端口/卷/网络契约、部署顺序、`stack/` 编排 |
| `proj/o2a-gateway` | OpenCode2API 网关 | Go 服务 + WebUI + 镜像 |
| `proj/instacloud-warp` | WARP 池与中继 | warp-pool 镜像、relay 脚本、socksmux |
| `proj/proxygate-ops` | 代理池运维 | matrix-sync / pool-sync / quota-recycler |
| `proj/xray-edge` | Xray 边缘节点 | 8443 WebSocket 入口 |
| `proj/checker` | 出口检测 | 8199 检查器服务 |

历史分支：`local/o2a-customizations` 保留为改造前基线；`archive/main-20261113` 为迁移前 main 快照。

## 部署

```sh
git clone -b main https://github.com/wbi35772/o2a.git o2a
cd o2a/stack
./sync.sh                 # 按 projects.tsv 拉取各项目分支
cp .env.example .env      # 填密码与令牌
docker compose up -d
```

## 规则

- 跨项目改动先改 main 里的契约（compose、端口、卷、env），再落项目分支。
- 项目分支只改自己目录，不做跨项目 cherry-pick。
- compose 只引用 `projects/` 路径或镜像，不引用分支名。
- 共享代码放 main 或独立项目分支。

## 目录

```
stack/        编排：compose、env 模板、同步脚本、项目清单
docs/         系统设计、部署手册、历史报告
assets/       文档图片
```
