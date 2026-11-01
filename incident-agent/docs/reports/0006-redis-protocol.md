# 阶段 0006 改动说明：Redis 原子协议

后续进展：本文件保留该增量的历史状态，当前 checkpoint/Worker 接线结果见[Worker 报告](0006-worker-recovery.md)。

日期：2026-10-31。任务 0006 进行中，本轮完成原子协议与隔离测试。工作区未提交，不提供虚构提交号。现有 CLI 仍为内存模式，尚未完成生产恢复闭环。

## 改了什么

| 文件 | 改动与用途 |
| --- | --- |
| [state.lua](../../internal/adapter/outbound/redisstate/state.lua) | 固定命令的原子状态机：创建、读取、领取、续租、Begin、Commit、取消、终结；预算与参数先于调用持久化，epoch 隔离迟到 Worker |
| [script.go](../../internal/adapter/outbound/redisstate/script.go) | embed 固定脚本，后续客户端不动态拼接 Lua |
| [集成测试](../../internal/adapter/outbound/redisstate/script_integration_test.go) | 启动独立 Redis，验证真实 Lua 执行和 AOF 重载，无模拟存储替代 |
| [Makefile](../../Makefile) | 增加显式 `make test-redis`，默认测试不启动外部进程 |
| [状态协议指南](../development/redis-state.md) | 命令字段、重复响应处理、容量、故障边界和后续接线条件 |
| [任务](../../tasks/0006-redis-recovery.md)、README、AGENTS、索引及阅读指南 | 当前阶段切换与渐进式入口；明确已实现脚本不等于已可恢复调查 |

## 实测范围

本机 Redis 8.0.2，临时目录和 Unix socket，TCP port=0，AOF everysec、noeviction。首次启动发现继承 locale 不兼容，测试进程显式设置 LC_ALL=C 后通过。

- 创建幂等，活跃状态无 TTL；续租不改变业务 version。
- 重复 Begin 不重复扣费；重复 Commit 不重复提交，内容不同拒绝。
- 模拟租约到期后新 owner 接管，保留 deadline/预算，将 IN_FLIGHT 标 UNKNOWN；旧 Worker 续租和提交被拒绝。
- Token 额度耗尽、16 次查询上限、非法 cost、旧 version、2MiB 结果容量拒绝时不改变状态。
- 排队取消清除 ready；运行取消阻止调用；到期或取消时丢弃模型报告并确定性终结。
- 错误 Redis Key 类型在写入前拒绝，不产生部分状态变更。
- 终态移除索引并设置保留期；正常关闭再启动 Redis，AOF 读回同一报告。

验证命令：`make test-redis`（race）、`make verify`、`make build`、`go mod verify`，以及带 redisintegration tag 的定向 lint。正常关闭后的 AOF 重载与人工设置租约过期，不等于 Worker kill 或主机崩溃测试。

## 未完成与下一步

2026-10-31 时客户端依赖待授权；用户于 2026-11-01 授权后已新增固定版本并完成客户端，见[客户端增量报告](0006-redis-client.md)。脚本集成测试使用本机官方 redis-cli，仅存在于带 tag 的测试代码；不把 subprocess 当运行时数据库 Adapter。

客户端已完成，后续接完整 checkpoint，再实现 Worker 周期扫描、续租和进程中断恢复。当前不实现 Incident 去重、事件 Stream、usage 退款、UNKNOWN 自动重试或恢复后继续调查。报告和证据仍需应用 Guard；不能公开底层存储命令给模型。商城代码、缓存 Redis 和真实模型均未改动/调用。
