# 阶段 0009：在线持久调查改动报告

日期：2026-11-01。对应[任务 0009](../../tasks/0009-online-investigation.md)与[生产路线 P2](../development/production-roadmap.md)。**本报告覆盖本地模拟服务上的工程验证。** 之后在本机真实后端上的运行及发现的 Token 结算问题见 [0010 报告](0010-http-entry.md)；真实供应商仍未联调。

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

## DeepSeek 联调

2026-11-02，官方接口，模型 `deepseek-flash`，窗口按 `/models` 配置为 1048576。发送的是本机商城遥测的规范化记录和请求摘要，业务 ID 已假名化。密钥只从环境变量读取，运行产物和日志里检索不到。

| 运行 | 结果 |
| --- | --- |
| 修复前，匿名 Fixture | 第 4 次调用输出 2703，超过 1500 的上限，以 MODEL_BUDGET 停止 |
| 只修请求形状，匿名 Fixture，5 次 | 全部正常收尾，用量在上限内；但有 DECISION_REJECTED、无记录的汇总，以及未查询就报告 |
| 全部修复后，匿名 Fixture，3 次 | 全部正常收尾，都先查询；2 次报告带有引用的事实，1 次无事实的 INCONCLUSIVE；仍偶有 DECISION_REJECTED |
| 持久 Worker＋独立 Redis，真实后端采集的 Bundle，3 次 | 全部 COMPLETED，每次 3–4 次模型调用，实际计费 5,771–6,533 Token；结果都是 INCONCLUSIVE，后两次带 1–2 条有引用的事实 |
| API＋Worker＋独立 Redis，在线模式 | 跑通，但 Docker 已停、三个后端都不可达：查询记 SOURCE_UNAVAILABLE，报告 INCONCLUSIVE，未编造结论 |

这些运行暴露并修复了四个问题，模拟模型都没有碰到：

| 问题 | 修复 |
| --- | --- |
| DeepSeek 忽略 `max_completion_tokens`，默认思考还占输出 | 新增 `INCIDENT_MODEL_API_PROFILE=deepseek`：发 `max_tokens`，关闭思考。profile 计入 Provider 身份，恢复时配置变化会停止 |
| `timeline.claim` 的说明含糊，模型写成字符串，被拒后也不知道原因 | 提示词写明对象形状；反馈改为固定的改正说明，不回显模型输出 |
| 汇总阶段被限制在 3,000 内，按字节估算放不下任何记录，只能 INCONCLUSIVE | 预留改为下限：汇总可用全部剩余额度，预留升到 6,000，输出上限不再压到 512 |
| 模型可能一次查询都没做就提交报告并被接受 | 未查询前的报告按决策拒绝并继续调查，反复如此才走无进展收尾 |

报告预留翻倍会减少调查阶段的额度上限，但按实际用量结算后，这几次运行都远未用满。按字节估算约是实际 Token 的 3 倍，尚未校准。

## 未验证与限制

- 模型诊断质量未评测：联调窗口是普通历史读取窗口，只能说明能跑通，不说明根因判断准确，这归 P5。
- 假名化不是匿名化：盐是 Run ID，能拿到 Run ID 的人可以穷举低熵 ID；Trace/Span ID、服务名和时间仍会发给模型。外发前需要操作员确认可接受。
- 模型上下文只遮蔽字段值。操作员写在 Request.Summary 里的内容会原样外发。
- 单次大查询可能耗尽额度，让后续查询都记为 RESULT_TOO_LARGE；额度未做真实数据校准。
- 后端连接没有认证头，非回环 HTTP 也未被拒绝，仅适合受控环境；认证归后续阶段。
- 导出 Bundle 重算记录 ID，报告里的 evidence_ids 与导出内容不能直接对应。
