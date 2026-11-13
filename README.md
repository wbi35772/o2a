# proxygate-ops

Matrix 订阅与出口池的运维脚本，运行在 `o2a-warp-pool` 容器内。

| 脚本 | 作用 |
|---|---|
| `matrix-sync.sh` | 拉取 matrix 订阅，合并进 relay 节点表 |
| `pool-sync.sh` | 逐槽探测 `/64`，发布去重后的 SOCKS 出口 |
| `quota-recycler.sh` | 监听网关 429，隔离被烧 `/64` 并让对应 relay 槽重注册 |

依赖：`bash`、`curl`、`nc`。运行时数据（节点表、状态、日志）在部署卷 `/data/o2a/deploy/data` 下，不入库。
