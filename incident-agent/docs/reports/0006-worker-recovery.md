# 阶段 0006 增量：Harness checkpoint 与 Worker 恢复

后续安全进展：Go 工具链升级和漏洞复验结果见[任务 0007 报告](0007-production-baseline.md)；本文保留当时的验收记录。

日期：2026-11-01。本轮完成持久 Harness/Worker 的可运行切片；不代表整个项目生产就绪。工作区尚未提交，不虚构提交号。

## 改动与原因

| 位置 | 改动 |
| --- | --- |
| [Harness](../../internal/app/investigation/runner.go)、[checkpoint](../../internal/app/investigation/checkpoint.go) | 共享 Execute/Resume 循环；版本化保存 phase、step、计数、反馈、证据和原 deadline，保存失败立即停止外部工作 |
| [模型预算](../../internal/app/investigation/model.go)、[请求预算](../../internal/telemetry/query.go) | 支持恢复已消耗额度和调用前持久预留；持久模型预算保守不退款 |
| [应用存储端口](../../internal/app/runstate/types.go) | 存储 DTO/接口移入应用层，Redis 保留类型别名，Worker 不依赖具体客户端 |
| [Worker](../../internal/app/worker/worker.go)、[调用记录](../../internal/app/worker/calls.go) | 单 Run 顺序执行，启动/周期发现、租约续期、调用结果复用、UNKNOWN 保守处理、停止与取消区分 |
| [Redis 脚本](../../internal/adapter/outbound/redisstate/state.lua)、[客户端](../../internal/adapter/outbound/redisstate/client.go) | checkpoint 原子提交、免费工具操作与实际请求独立计数、有界候选扫描、持久配置检查；容量随冗余快照显式调整 |
| [CLI](../../cmd/incident-worker/run.go)、Makefile | 新增第五个二进制 incident-worker，提供 enqueue/work/status/cancel，冻结配置身份及 Fixture digest |
| 测试与文档 | 覆盖原有内存入口回归、持久恢复故障窗口、编译后的 CLI；同步任务/入口/指南 |

## 本地验证

独立临时 Redis 8.0.2，Unix socket、TCP 关闭、AOF everysec、noeviction。本地模型 Fake/HTTP 模拟服务，不是供应商准确率评测。

- 模型/查询提交后模拟确认丢失：第一次 Worker 停止，接管复用已提交结果。合计 **2 次模型、1 次 Fixture 查询、0 次后端请求**；deadline 不变，决策计数为 2，证据不重复。
- 真正子进程在模型提交后 `os.Exit(23)`，没有正常收尾；等待真实 15 秒租约过期，从 leases 发现任务，替代进程逻辑复用决策，仅新增一次报告模型调用及一次 Fixture 查询。
- 模型预留成功而确认丢失：不发起 Provider 请求；接管将该调用视为 UNKNOWN，保留预留并 INCONCLUSIVE 收尾。
- 实际后端请求预留确认丢失：Adapter 的预留钩子拒绝继续，测试请求计数保持 0；Redis 保守消耗 1 次，恢复不重置、不补发。
- 慢模型期间实际 5 秒续租，lease_until 增加而业务 version 不变；取消 Worker 父 context 后 Run 仍 RUNNING，接管对未知调用不重发。
- 运行中已请求取消/已到 deadline 的任务接管，不构造 Provider，确定性终结；旧 epoch 提交由原子协议拒绝。
- checkpoint 恢复保留 step、phase、feedback、无进展、请求计数及 deadline；checkpoint 写失败不进入 Decider。
- 非法模型原文不持久，保留拒绝种类和修复次数；查询结果落盘前校验并脱敏归一化。
- 编译后 CLI 对本地 HTTP 模型完成 enqueue → work -once → status；enqueue 模型调用 0，完成合计 2，再次扫描仍为 2。
- Redis 配置不符合 AOF everysec/noeviction 时拒绝启动执行。

验证入口：`make verify`、`make test-redis`、`golangci-lint run --build-tags=redisintegration ./internal/adapter/outbound/redisstate/...`、`make build`、`go mod verify`。新增进程故障/续租测试有真实等待，不能只跑默认单元测试替代。

## 交付边界与下一步

使用步骤和故障解释见[Worker 指南](../development/worker-recovery.md)。当前 CLI 只调用配置好的模型并读取冻结 Fixture，旧内存 CLI 行为保留；尚未端到端验证在线遥测持久模式。UNKNOWN 不自动重试同一操作；这比目标协议中的有限重试更保守，已明确作为本切片策略。

取消/到期的 Redis 状态优先于 checkpoint；若截止边界赢过完成提交，丢弃迟到报告，仅保留上次已确认的中间快照。不能把中间 Run.status 当作终态。后续工作台/API 应提供专门的终态读模型。

持久模式上限为结果 2MiB、checkpoint 3MiB、编码状态 7MiB、40 条操作，尚无全局准入/容量压测。当前单 Worker 串行执行，未实现 Incident 去重、events/SSE、身份认证或高可用。Go 1.25.3 已有漏洞扫描阻断仍需单独升级复验；本轮没有重新运行漏洞扫描或宣称安全门槛通过。

商城代码、缓存 Redis 和历史 Fixture 均未改动。建议下一开发目标先处理工具链安全更新，再验证选定供应商与在线遥测恢复、容量控制及终态读接口。
