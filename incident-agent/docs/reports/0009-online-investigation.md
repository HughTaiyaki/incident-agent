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

按字节估算约是实际 Token 的 3 倍，尚未校准。

## 在线完整运行

2026-11-02，API、Worker、独立 Redis、本机 Prometheus/Loki/Jaeger 和 DeepSeek `deepseek-flash`。窗口是 `experiment` 环境 2026-10-27 07:25–07:40 的历史数据，含大量 `order_id` 和 ERROR 日志，没有操作商城。发送内容包括遥测记录和请求摘要，业务 ID 已假名化。

| 运行 | 结果 |
| --- | --- |
| 经回环记录代理，核查发出内容 | COMPLETED，5 次调用，3 次后端请求，17,612 Token。报告有 3 条带引用的事实和时间线，如错误率从约 0.76 升到约 0.82 |
| 遮蔽核查（同上） | 证据含 15 个真实业务 ID，发出内容里 0 个；出现的 3 个 ID 字段都已假名化 |
| 直连 DeepSeek | COMPLETED，6 次调用，5 次后端请求，17,762 Token，10.7 秒；NO_PROGRESS，回退为无事实的 INCONCLUSIVE |
| 运行 6 秒时 `kill -9` Worker | 新 Worker 接管（epoch 2），状态不明的 `tool-6` 不重发并记为失败证据；COMPLETED，8 次调用，18,166 Token，25.3 秒，报告带 2 条事实 |

运行产物和日志里检索不到密钥。

这些运行暴露的问题见下一节。

另外，`bench` 环境的窗口查询结果为空，这是范围校验在起作用：环境只允许 `local` 和 `experiment`。

## 调查质量修复

上述运行有三个问题：额度接近上限、模型重复查询、结果不稳定。同一窗口和请求，DeepSeek `deepseek-flash`，修复前后对比：

| 指标 | 修复前（3 次） | 修复后（6 次） |
| --- | --- | --- |
| 单次 Token | 17,612–18,166，其中 1 次因 Worker 被杀多调用 | 5 次约 16,600，1 次因修正报告 21,556 |
| 空报告 | 1 次 | 0 次 |
| 带引用的事实 | 0–3 条 | 5–6 条 |
| 重复已成功的查询 | 有 | 被 Harness 拒绝，不发请求 |
| 模型调用 | 5–8 次 | 7–8 次，查询覆盖更多指标 |

样本很小，且只有这一个窗口，只能说明方向对，不能作为诊断效果。结果仍全是 INCONCLUSIVE，报告说明了原因：日志正文被省略，Trace 为空，其他服务未查询。

| 根因 | 修复 |
| --- | --- |
| 查询历史只有类型和状态，模型不知道查过哪个服务和指标 | 发送完整查询清单；已成功的相同查询直接拒绝，不发请求（新结果 `QUERY_REPEATED`） |
| 汇总阶段一次格式错误就回退成空报告 | 报告最多可修正两次；拒绝原因细分为固定代码，记入步骤审计 |
| 模型常漏掉外层 `kind`，内容正确却被整份拒绝，每次白费约 4,800 Token | 只补外层，内容仍走严格校验 |
| 每次调用重发 12–24 条完整记录和 64 位 ID，日志聚合却没发 | 调查阶段 8 条，发送聚合计数，缩短 ID，去掉多余字段 |
| 指标按时间取前几个点，窗口后段的峰值看不到 | 优先发每条序列的首尾和极值点 |
| 字节估算约为实际的 3 倍，后期上下文只剩 0–1 条记录 | 估算减半，仍高于实际；额度不足 4,000 时直接汇总 |
| 模型不知道剩余额度，留给报告的空间不足 | 发送 `tokens_left`；报告预留升到 9,000 |

Harness 策略版本升到 `harness-v4`。

**仍未解决：**

- **总用量没有大幅下降：** 约 16,600 Token 仍接近上限，大多数运行以 MODEL_BUDGET 进入汇总，模型并没有主动提前收尾。费用不高（整轮约 20 次共 ¥0.6），所以没有继续压缩。
- **Trace 为空：** Jaeger 重启后内存数据丢失，这些运行没有用到 Trace 证据。Trace 路径只在之前脚本模型的运行中验证过。
- **诊断效果未评测：** 窗口的故障真值没有和结果比对，归 P5。

## 未验证与限制

- 模型诊断质量未评测：联调窗口是普通历史读取窗口，只能说明能跑通，不说明根因判断准确，这归 P5。
- 假名化不是匿名化：盐是 Run ID，能拿到 Run ID 的人可以穷举低熵 ID；Trace/Span ID、服务名和时间仍会发给模型。外发前需要操作员确认可接受。
- 模型上下文只遮蔽字段值。操作员写在 Request.Summary 里的内容会原样外发。
- 单次大查询可能耗尽额度，让后续查询都记为 RESULT_TOO_LARGE；额度未做真实数据校准。
- 后端连接没有认证头，非回环 HTTP 也未被拒绝，仅适合受控环境；认证归后续阶段。
- 导出 Bundle 重算记录 ID，报告里的 evidence_ids 与导出内容不能直接对应。
