# 阶段 0011：部署与运维改动报告

日期：2026-11-02。对应[任务 0011](../../tasks/0011-deployment-operations.md)与[生产路线 P4](../development/production-roadmap.md)。

## 修改前后

| 方面 | 之前 | 现在 |
| --- | --- | --- |
| 单个坏 Run | Worker 退出，重启后再撞上，所有 Run 停摆 | 常驻 Worker 跳过、记录并计数，其他 Run 继续 |
| Worker 可观测性 | 只有退出错误 | JSON 日志；`/metrics` 报告队列、内存、Worker 存活和累计用量 |
| 部署 | 需要手工启动进程 | 镜像 + Compose + 告警规则 + 值班手册 |
| 密钥 | 只能用环境变量 | 也可 `*_FILE`；发布检查扫描密钥 |
| 容量取值 | 没有依据 | 单机测量，建议从 2 开始、不超过 8 |

## 文件与职责

| 文件 | 改动 |
| --- | --- |
| [worker.go](../../internal/app/worker/worker.go) | `ErrRunBlocked`、日志、完成后计数 |
| [metrics.go](../../internal/adapter/inbound/httpapi/metrics.go) | `/metrics` |
| [client.go](../../internal/adapter/outbound/redisstate/client.go) | `Stats`、`RecordOutcome`、`RecordBlocked` |
| [secretenv.go](../../internal/secretenv/secretenv.go) | `NAME` 或 `NAME_FILE` |
| [checks/secrets](../../internal/checks/secrets/main.go) | 扫描 Git 会发布的文件，只报位置不回显 |
| [Dockerfile](../../Dockerfile)、[compose.yaml](../../deploy/compose.yaml)、[.env.example](../../deploy/.env.example) | 镜像和部署 |
| [alerts.yml](../../deploy/prometheus/alerts.yml)、[smoke.sh](../../deploy/smoke.sh) | 告警规则；不需要密钥的整栈冒烟测试 |
| [ops_integration_test.go](../../internal/adapter/outbound/redisstate/ops_integration_test.go) | 坏 Run、指标、备份恢复、Redis 重启 |

Lua 状态协议和 schema 未变。没有新增 Go 依赖，没有修改商城。

## 验证记录

环境：macOS arm64，Go 1.26.10，独立 Redis 8.0.2。**本机 Docker 未运行，镜像和 Compose 没有在本机启动过**，只做了 `docker compose config`，由 CI 的 `deploy-smoke` 任务实际运行。

| 演练（真实 Redis） | 结果 |
| --- | --- |
| 损坏状态和配置不匹配的 Run 排在正常 Run 前面 | Worker 不退出，正常 Run COMPLETED，两个坏 Run 仍占名额并计数；`Process` 仍返回错误 |
| 备份后恢复到新 Redis | 终态 Run 报告和预算一致；排队的 Run 继续完成 |
| Redis 停止再启动 | Worker 带错误退出；readyz 为 `redis_unavailable`；重新提交后新 Worker 完成 |
| Redis 停止时抓取指标 | 200，`incident_redis_up 0`，不含其他值 |

容量测量见[部署指南第 3 节](../operations/deployment.md#3-容量取值)。

## 未验证与限制

见[部署指南第 6 节](../operations/deployment.md#6-未覆盖)。另外：

- 资源上限只配置未压测，Worker 256–512MiB 的取值没有测量依据。
- 真实模型耗时只有 3 个样本，且窗口数据简单，慢模型下的排队上限需要重新估算。
- 多 Worker 未验收。租约协议允许，但 Compose 只跑一个。
- 备份恢复的 `tar` 和数据卷命令未实际运行。
