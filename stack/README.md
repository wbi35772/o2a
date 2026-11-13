# stack —— Instabox 部署编排

main 分支上的唯一编排入口。业务代码在各项目分支，`sync.sh` 按 `projects.tsv` 物化到 `projects/`。

## 步骤

1. `./sync.sh`
2. `cp .env.example .env` 并填写（SOCKS 密码、池令牌、网关 key、admin 密码）
3. `docker compose up -d`
4. `docker compose ps`，再按 `docs/deploy-manual-legacy.md` 做出口自检

## projects.tsv

三列，制表符分隔：

```
name	branch	pin
```

`pin` 是发布时锁定的 commit SHA；留空或 `-` 表示跟随分支。`O2A_SYNC_PULL=1 ./sync.sh` 会对无 pin 的项目做 fast-forward。

## 目录

```
compose.yml       服务：pool-init、warp-pool、gateway、xray-node、checker
.env.example      环境模板（不含真实密钥）
config.seed.json  网关初始配置模板
gateway-boot.sh   网关容器入口
projects.tsv      项目分支与固定版本
sync.sh           项目物化脚本（幂等）
projects/         sync.sh 生成，已 gitignore
data/             运行时卷，已 gitignore
```

## 契约

- 端口：gateway 8080/18080、warp-pool 8081、relay 1594-1633、matrix 8090、xray 8443、checker 8199
- 共享运行时卷：`data/pool`、`data/gateway`、`data/shared`
- compose 只引用 `projects/` 或镜像，不引用分支名
- 跨项目改动先改本目录，再落项目分支并更新 `projects.tsv` 的 pin

原 `deploy/` 手册已归档为 `docs/deploy-manual-legacy.md`，路径以本目录为准。
