# 模型 Provider、上下文与预算

更新：2026-11-02。对应[任务 0005](../../tasks/0005-model-boundary.md)。本切片实现可配置模型调用、输入选择与内存预算；已用本地 HTTP 模拟服务验证，并与 DeepSeek 官方接口联调，含在线后端的完整运行（结果见[任务 0009](../../tasks/0009-online-investigation.md)），不代表诊断效果或生产部署验收通过。当前遥测来源仍为冻结 Fixture。

## 1. 两种命令与启用方式

`incident-investigate` 保持脚本驱动、完全离线。新增 `incident-model` 会将请求摘要和选中的匿名遥测记录发送到操作者配置的模型端点，使用 Provider 返回的 JSON 决策执行调查。它不查询商城数据库，不修改商城，也不启动在线遥测查询。

在项目目录先 `make build`、`mkdir -p data`。通过本机环境配置下面四项；不要把真实密钥写入命令示例、脚本、Git 或报告：

| 环境变量 | 含义 |
| --- | --- |
| INCIDENT_MODEL_ENDPOINT | 完整 Chat Completions 请求 URL；没有默认供应商地址 |
| INCIDENT_MODEL_NAME | 部署者选定的实际模型名称；建议固定版本 |
| INCIDENT_MODEL_CONTEXT_WINDOW | 实际模型上下文窗口，整数 4096–2000000；必须显式配置，本项目不推断 |
| INCIDENT_MODEL_API_KEY | 认证密钥；非回环地址必填，回环本地模型可省略 |
| INCIDENT_MODEL_API_PROFILE | 请求形状：`openai`（默认）或 `deepseek`，见第 2 节；API 与 Worker 必须一致 |

配置后执行：

```bash
./bin/incident-model \
  -request testdata/investigation/observed-request.json \
  -fixture testdata/telemetry/observed-20261028/bundle.json \
  -out data/model-run.json
```

该命令会调用模型，可能产生费用；此前的脚本模式不会。当前示例是普通历史读取窗口，不能用其结果发布故障诊断准确率。已有输出不覆盖，目录须预先存在；完整文件权限 0600，stdout 只输出运行状态、调用次数及核算 Token 数。

退出码 0 表示已保存终结产物，包括因 Provider 故障生成的 INCONCLUSIVE；1 表示取消或文件/执行错误；2 表示配置或输入错误。自动化必须检查 `run.report.stop_reason` 与 `run.model_calls`，不能只按退出码判断模型可用。突然断电或强杀可能留下空/不完整产物，尚不支持断点恢复。

## 2. HTTP 兼容范围

当前采用 [Chat Completions API](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create) 的非流式 JSON 子集：发送两条 system/user 消息、`max_completion_tokens`、`response_format.type=json_object`、`stream=false`、`store=false`；读取单条 choices、finish_reason 与 usage。JSON 模式仍需由本地 Guard 校验业务 schema，参考[结构化输出说明](https://developers.openai.com/api/docs/guides/structured-outputs)。

**DeepSeek 配置（2026-11-02 实测）：** 端点 `https://api.deepseek.com/chat/completions`，`INCIDENT_MODEL_API_PROFILE=deepseek`，模型名和窗口以其 `/models` 接口为准（实测时 `deepseek-flash`、1048576）。必须用这个配置：DeepSeek 忽略 `max_completion_tokens`，实测设 30 仍返回 425 个输出 Token，只有 `max_tokens` 生效；`max_tokens` 又把思考内容计入，所以该配置同时关闭思考，并省略文档未列出的 `store`；它还固定 `temperature=0`，实测同样输入的运行明显更一致，但仍不完全相同。

只接受 `finish_reason=stop`，拒绝工具调用、拒答、截断和异常响应形状。不能据此宣称任意“兼容 OpenAI”的网关或模型都已兼容；支持不同参数、Responses API 或其他供应商需要独立 Adapter 与联调。即使请求包含 store=false，也不能将其解读为供应商的完整数据保留承诺。

- 固定配置 URL，模型和来源文本不能改变它；不接收 URL 凭据、查询串或 fragment。
- 非回环地址必须 HTTPS；仅 localhost/回环 IP 允许 HTTP，便于本地模型与测试。
- 禁用环境代理和重定向；单次最长 30 秒，父 Run 更短时沿用父截止。
- 响应最多 256KiB；不保存或输出供应商错误正文、API Key 或请求原文。
- 不做自动重试，避免超时/响应丢失时重复付费；失败由 Harness 确定性收尾。

## 3. 上下文怎样选择

请求带 `log_sweep` 时（HTTP 入口默认开启），调查开始由程序查一遍所有服务不带过滤的日志，不花模型调用，这几步仍占决策次数。固定规则与不可信数据分成两条消息。规则按阶段发送：调查阶段只有查询说明，模型返回 `{"kind":"synthesize"}` 结束调查后，才给报告格式，这样每次调用不用重复发送报告说明。被拒绝后，反馈是按结果代码选择的固定改正说明，不回显模型输出。数据包含冻结 Request、阶段、固定的改正说明、剩余决策数和 Token、已执行查询清单（范围、过滤条件、指标、状态和记录数，最新一个带来源质量）、还没查过日志或 Trace 的服务、日志聚合的计数（最多 8 组，错误优先），以及规范化记录（调查阶段最多 8 条，汇总阶段最多 24 条）。长摘要保留前 128 字符并标记截断；放不下的查询、聚合和记录分别计入 omitted_queries/omitted_patterns/omitted_records。

先保留范围和省略标记，再装入查询清单。若预算仍不足，省略较旧查询并增加计数；聚合最多占剩余空间的三分之一，其余留给可引用的记录；必要基础内容都装不下时，不发起调用，输出 MODEL_CONTEXT_LIMIT。上下文窗口低于约 16K 时可能放不下记录。模型不能把未显示的信息当成不存在。

调查阶段从最近三个查询、汇总阶段从全部查询轮流选择记录，避免单个指标大结果挤掉全部其他来源；日志先考虑已生成聚合的代表记录，出错的 Span、带错误 Span 的 Trace 摘要、结构化事件和 WARN/ERROR 日志优先，否则它们会被普通记录挤掉；指标先取每条序列的首尾和极值点，否则窗口后段的峰值会被记录上限挡掉；没有值的点只计数，不发送。发送时去掉采集时间和环境，指标值保留四位有效数字，记录 ID 缩短为 12 位，收到回复后再还原；前缀冲突时不还原，由报告校验拒绝。只传入规范化结构记录，不传 canonical_query、普通日志正文、实验报告或整个 Bundle。

**脱敏文本（用户 2026-11-02 决定放开）：** WARN/ERROR 和带事件名日志的 `message`、出错 Span 的 `status_description` 会发给模型，因为评测发现这是区分故障和正常行为的关键证据。只取第一行，最多 200 字符；引号内容、URL、邮箱、IP、UUID、十六进制串、长数字和非 ASCII 文本都替换为占位符；含凭据或实验注入点字样的整条丢弃。普通 INFO/DEBUG 正文、SQL、主机和 IP 标签继续不发。**这是减少暴露，不是匿名化：** 脱敏只能挡住已知形态，接生产数据前要重新评估。这些文本同样按不可信数据处理，不能扩展任何权限。

摘要选择是启发式，不保证保留全部反例，不能宣传为无损压缩。

发送前，记录中的业务 ID（order_id、operation_id、event_id、order.no、payment.id、messaging.message.id）换成按 Run 加盐的假名，同一 Run 内仍可关联；Trace/Span ID 保留以便继续查询。这是假名化，不是匿名化。

模型可以按 severity、event_name、order_id 等过滤日志，因为不过滤的查询只返回窗口内最早的记录。查询的环境和时间窗默认取请求的范围，模型可以收窄，不能放宽。模型只见过假名，所以查询里的假名由程序还原成真实值再发给只读后端；报告正文仍保留假名。模型只得到有界数据，不得到 Shell/SQL/URL 工具。模型漏掉外层 `kind` 时（DeepSeek 常见），只补外层，内容仍走严格解码和报告校验。返回决策继续经过既有查询 Guard 和 Report Guard；提示词不能替代代码权限控制，引用存在也不证明语义成立。

## 4. Token 预留与审计

| 项目 | 行为 |
| --- | --- |
| Run 总额度 | 36,000，输入＋输出一起核算 |
| 调查阶段 | 可用额度扣除 9,000 报告预留；不能消耗报告预留；可用额度不足 4,000 时直接进入汇总，不再让模型近乎盲选查询 |
| 汇总阶段 | 可用全部剩余额度，至少有报告预留；预留是下限，不是上限 |
| 单次输出 | 最多 1,500，临近额度时缩小 |
| 输入上限 | 本次可用额度与模型窗口 60% 中较小值，再扣输出上限 |
| 输入估算 | （UTF-8 字节数＋固定规则长度）的一半＋256 开销，版本 utf8-half-bytes-plus-256-v2。DeepSeek 实测输入约为字节数的 1/3，所以仍有余量 |
| 发起调用 | 先在内存中记录调用并扣预留，再请求 Provider |
| 完整、合法 usage | 按 prompt_tokens＋completion_tokens 核算，退回剩余预留 |
| 失败、超时、缺失 usage | 全额消耗预留，标 estimated/UNKNOWN 或 RECEIVED；未知用量不退回 |
| 不合法 usage | 保留预留，停止本次调查 |
| 实际用量超过预留/输出上限 | 记录供应商用量，丢弃决策并禁止继续调用，停止原因为 MODEL_BUDGET |

这是保守估算和事后核算，不是精确 tokenizer，也不是供应商账单硬上限。供应商隐藏开销、错误 usage 或不遵守输出限制可能超过本地预留；出现超额后只能停止后续请求，不能撤销已产生费用。报告额度包含输入与输出，不保证足够生成复杂报告；装不下时保留证据并输出确定性 INCONCLUSIVE。

`run.model_calls` 保存 step、配置模型、上下文窗口、估算版本、context_digest、reserved_tokens、charged_tokens、output_limit、usage/estimated、status 和省略数量。原始 Prompt、原始响应与私有推理不落盘。调用返回 RECEIVED 只表示收到结果，具体决策是否通过见同 step 的 Run 步骤。

Decider 一次只归属一个 Run，禁止重复 Execute 复用；额度由互斥锁串行预留和结算，未知请求不会因失败退款。调用审计当前仅内存，正常收尾后写入产物；进程崩溃后不提供预算守恒或计费恢复保证。

## 5. 验证与生产门槛

```bash
go test ./internal/app/investigation ./internal/adapter/outbound/modelhttp ./cmd/incident-model
make verify
make build
go mod verify
```

本地测试覆盖真实 HTTP 请求形状、查询→证据→模型报告闭环、调用前配置校验、不覆盖、报告预留、usage、失败保守计费、超额停止、取消、重定向、拒答、截断和有限上下文。未使用真实密钥调用外部模型。

生产部署前还需要：选定供应商/模型并验收兼容性和效果；明确匿名数据外发规则；部署独立 Redis，实现调用审计持久化、租约和崩溃恢复；完成入口身份认证、容量与并发控制、网络隔离、运维监控；补齐任务 0002 的固定版本故障/正常/缺证据样本。HTTP 健康检查 API 仍没有调查端点。
