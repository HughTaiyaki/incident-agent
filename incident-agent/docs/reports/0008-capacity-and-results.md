# 阶段 0008：Run 容量与终态查询改动报告

日期：2026-11-01。对应[任务 0008](../../tasks/0008-capacity-and-results.md)与[生产路线 P1](../development/production-roadmap.md)。工作区尚未提交，不填写提交号。

## 修改前后

| 方面 | 之前 | 现在 |
| --- | --- | --- |
| 创建 Run | 不限数量 | create 脚本内原子校验活跃数上限和内存预留，拒绝时不写入 |
| Redis 配置检查 | AOF、fsync、noeviction | 另要求 maxmemory 非零 |
| status/cancel 输出 | 内部 `code`+`state`，含 checkpoint 和调用输入/结果 | 版本化投影 `run-result-v1`，只有 COMPLETED 带报告 |
| 证据查看 | 从 state 的 checkpoint 中自行解析 | 新增 evidence 模式，输出 `run-evidence-v1` |
| 状态异常 | 版本不符与损坏都报 CORRUPT | 分为 CORRUPT、UNKNOWN_SCHEMA、NOT_FOUND；Worker 错误带 Run ID 和响应码 |
| 残留索引 | 无状态的条目永久留在索引中 | Claim 时清理，释放名额 |

状态 schema 未变，已有 Run 可继续读取。**不兼容变更：** enqueue 必须设置 `INCIDENT_MAX_ACTIVE_RUNS`，enqueue/work 要求 Redis 配置 maxmemory，status/cancel 输出格式改变。

## 文件与职责

| 文件 | 改动 |
| --- | --- |
| [state.lua](../../internal/adapter/outbound/redisstate/state.lua) | 准入、内存预留、UNKNOWN_SCHEMA、残留索引清理、响应带 Redis 时间 |
| [client.go](../../internal/adapter/outbound/redisstate/client.go) | 新响应码；CheckPersistence 检查 maxmemory |
| [runstate/types.go](../../internal/app/runstate/types.go) | Command.MaxActive、Reply.Now |
| [worker.go](../../internal/app/worker/worker.go) | Enqueue 接收上限并映射 ErrOverloaded；错误带诊断信息 |
| [result.go](../../internal/app/worker/result.go) | 结果与证据投影、ErrNotFound/ErrCorrupt/ErrUnknownSchema |
| [run.go](../../cmd/incident-worker/run.go) | 配置解析、status/evidence/cancel 投影、过载退出码 3 |
| [result_test.go](../../internal/app/worker/result_test.go) | 投影与 Enqueue 单元测试 |
| [capacity_integration_test.go](../../internal/adapter/outbound/redisstate/capacity_integration_test.go) | 真实 Redis 上的准入、回收、预留、迟到完成和损坏状态测试 |
| 其他集成测试 | 传入上限，临时 Redis 设置 256MB maxmemory；CLI 全流程扩展 |
| Worker/Redis 指南、调查协议、入口文档 | 同步契约与状态；新增[任务 0009](../../tasks/0009-online-investigation.md) |

没有新增依赖，没有修改商城或商城 Redis。

## 验证记录

环境：macOS arm64，Go 1.26.10，独立 Redis 8.0.2，临时 Unix socket 实例。没有调用真实供应商或在线遥测。

| 检查 | 结果 |
| --- | --- |
| 完整 `make release-check` | 退出 0：格式、模块校验、vet、race、lint（0 issues）、Redis 回归 65.7s、五个命令构建 |
| 普通和 redisintegration 标签 govulncheck | 均为 `No vulnerabilities found.` |
| 并发创建 | 16 个不同 ID 抢 4 个名额：恰好 4 个 OK、12 个 OVER_CAPACITY，被拒 ID 无状态键 |
| 重复 ID | 8 个并发同 ID 只创建一次；已满时仍返回 EXISTS/CONFLICT，不报过载 |
| 名额回收 | 排队取消、完成后释放；运行中取消和到期在 Worker 收尾前不释放 |
| 接管 | 租约到期后重新领取，活跃数不变 |
| 内存预留 | 剩余空间不足两份预留时拒绝新 Run，同时已准入 Run 仍能写 3MiB checkpoint 并完成；maxmemory 为 0 时拒绝 |
| 迟到完成 | 取消/到期后提交报告，并预置声称完成的 checkpoint：投影无报告，状态取自 state.status，证据仍可查 |
| 异常状态 | 无效 JSON、ID 不符、未来 schema、不可读 checkpoint、缺报告分别给出明确错误；create/cancel 不覆盖 |
| 编译后 CLI | enqueue/work/status/evidence/cancel；缺上限退出 1，过载退出 3 且未写入 |

完整日志保存于 `data/capacity-results-20261101/release-check.log`，属于 git 忽略的运行产物。

## 未验证与限制

- 没有压测，8MiB 预留是按编码上限推算的值，未测量真实内存开销和碎片；未在 Redis 真正写满内存时验证收尾行为。
- 没有运行远端 CI；本机 macOS 通过不能替代 Linux runner 结果。
- 保留期过后无法区分已过期和从未创建，统一报 NOT_FOUND。
- 损坏或未知版本的活跃 Run 占用名额，并让 Worker 停止；需人工处理。
- 不同 enqueue 进程可配置不同上限，判定以各自传入的值为准，由操作员保持一致。

P1 通过不代表可以公开部署；真实供应商、在线持久调查、认证入口和部署演练仍待后续阶段验收。
