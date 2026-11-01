# 阶段 0009：在线持久调查改动报告

日期：2026-11-01。对应[任务 0009](../../tasks/0009-online-investigation.md)与[生产路线 P2](../development/production-roadmap.md)。**本报告只覆盖本地模拟服务上的工程验证，不是真实后端或真实供应商的验收结果。**

## 修改前后

| 方面 | 之前 | 现在 |
| --- | --- | --- |
| 遥测来源 | Worker 只能用 Fixture | 增加 `-live`，地址显式配置并冻结身份 |
| 单次调用超时 | 被当作持久化失败，Worker 退出 | 记为调用失败，Run 继续收尾 |
| 超大查询结果 | 超出额度时提交被拒，Worker 退出 | 记为 `RESULT_TOO_LARGE`，Run 继续 |
| Prompt 中的业务 ID | 在线数据会原样发给模型 | 按 Run 加盐假名化 |
| 失败结果来源 | 一律标为 `fixture` | 标为 `harness` |
| 证据留存 | 只能输出含真实 ID 的证据 | 新增 export，输出匿名 Bundle |
| 请求扣费记录 | status 显示为 IN_FLIGHT/UNKNOWN | 显示 CHARGED，避免误读为调用结果 |

前三项是接入在线数据才会暴露的问题：Fixture 不会超时，结果也小，既有测试碰不到。第一项对真实供应商影响最大，因为一次模型慢响应就会让 Worker 停止处理所有 Run。

Job 版本仍是 worker-v1，新增字段可选，已有 Run 可继续读取。

## 文件与职责

| 文件 | 改动 |
| --- | --- |
| [run.go](../../cmd/incident-worker/run.go) | `-live`、export 模式、来源选择 |
| [calls.go](../../internal/app/worker/calls.go) | 区分调用超时和 Run 结束；限制查询结果占用 |
| [worker.go](../../internal/app/worker/worker.go)、[result.go](../../internal/app/worker/result.go) | Job 来源身份；证据 origin；CHARGED |
| [model_context.go](../../internal/app/investigation/model_context.go) | 发往模型前遮蔽业务 ID |
| [checkpoint.go](../../internal/app/investigation/checkpoint.go)、[runner.go](../../internal/app/investigation/runner.go)、[types.go](../../internal/app/investigation/types.go) | FailedResult；View.RunID |
| [bundle.go](../../internal/telemetry/bundle.go) | 共用标识字段判断；接受 harness 来源 |
| [online_integration_test.go](../../internal/adapter/outbound/redisstate/online_integration_test.go) | 真实 Redis＋模拟后端/模型的失败与恢复路径、CLI 全流程 |
| [calls_test.go](../../internal/app/worker/calls_test.go)、[model_context_test.go](../../internal/app/investigation/model_context_test.go) | 超时分类、额度和遮蔽单元测试 |

没有新增依赖，没有修改商城。

## 验证记录

环境：macOS arm64，Go 1.26.10，独立 Redis 8.0.2。后端和模型均为进程内 httptest 模拟服务，**没有连接真实遥测或外部模型**。

| 场景 | 结果 |
| --- | --- |
| 503 后重试 | 后端 2 次请求，持久扣费 2 次，证据来源 loki |
| 后端 500 | Worker 不退出，保存失败证据，Run 完成 |
| 后端不响应 | 单次超时后 Worker 不退出，Run 完成 |
| 查询结果提交确认丢失 | 接管后复用已提交结果，后端不再请求 |
| 扣费确认丢失 | 后端 0 次请求，接管后不重发，不退款 |
| 请求途中进程停止 | 接管后不重发，不伪造结果 |
| 模型单次超时 | 记录失败并提交，不返回持久化错误 |
| 业务 ID | 所有 Prompt 不含真实 order_id、日志正文和 Run ID；Redis 证据保留真实值 |
| 接近单查询上限的指标结果（20 序列×236 点） | Worker 不退出，额度内保存，超出记 RESULT_TOO_LARGE |
| 编译后 CLI | 来源参数校验、缺地址拒绝、地址变化时停止且无外部调用、work/status/export；导出不含真实 ID、不覆盖、权限 0600 |

保护有效性核对：临时去掉结果大小判断后，超大结果测试以 `CAPACITY` 失败，恢复后通过。其余保护没有逐项做这种反向验证。

本机 `make release-check` 退出 0：格式、模块校验、vet、race、lint、Redis 回归 136.6s、五个命令构建，普通与 Redis 标签漏洞扫描均为 `No vulnerabilities found.`。代码随提交 `7552a10` 入库，[远端 CI](https://github.com/HughTaiyaki/incident-agent/actions/workflows/agent-release-check.yml) 在 ubuntu-24.04 通过。CI 同样只用模拟服务。

## 未验证与限制

- 没有连接真实 Prometheus/Loki/Jaeger，模拟响应只覆盖 Adapter 测试已知的形状。
- 没有调用真实供应商，兼容性、模型诊断质量和费用都未知。
- 假名化不是匿名化：盐是 Run ID，能拿到 Run ID 的人可以穷举低熵 ID；Trace/Span ID、服务名和时间仍会发给模型。外发前需要操作员确认可接受。
- 模型上下文只遮蔽字段值。操作员写在 Request.Summary 里的内容会原样外发。
- 单次大查询可能耗尽额度，让后续查询都记为 RESULT_TOO_LARGE；额度未做真实数据校准。
- 后端连接没有认证头，非回环 HTTP 也未被拒绝，仅适合受控环境；认证归后续阶段。
- 导出 Bundle 重算记录 ID，报告里的 evidence_ids 与导出内容不能直接对应。
