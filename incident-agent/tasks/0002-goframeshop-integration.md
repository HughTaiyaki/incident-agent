# 任务 0002：接入已完成 GoFrame 商城的遥测

状态：**进行中**。2026-10-26 替代已废弃的[任务 0001](0001-observable-checkout.md)。2026-10-28 已交付查询 Adapter、匿名快照、离线回放和 CLI，完成真实历史窗口的只读接入验证；固定版本故障案例验收仍未完成。

2026-10-29 用户要求暂不修改商城，继续 Agent 开发：先推进[任务 0003 的离线 Harness](0003-offline-investigation.md)，本任务的外部实验验收保留未完成；不通过改写状态绕过固定版本样本要求。

## 已交付与剩余工作

- 已实现三源受限查询、模板与响应范围校验、请求预算/取消/有限重试、结构字段筛选、匿名 Bundle 与离线过滤回放。当前只读首批，无分页游标；日志正文省略。
- 已在本机后端采集 6 次查询、169 条结构记录；真实 Trace 可搜索和按 ID 读取，Loki metadata 可解析，未定义指标值没有补零。操作说明见[使用指南](../docs/integration/query-adapters.md)。
- `make verify`、`make build` 和显式在线集成测试通过；人工核查与数据缺口见[接入报告](../docs/reports/0002-readonly-integration.md)。
- 用户已确认商城本机路径，当前 HEAD 为 `2f25dc5`，未提交内容为文档；运行进程对应二进制的构建信息为 `bb921e9` 且 `vcs.modified=true`，历史遥测的精确源码版本仍未证实。服务端口已占用，未运行故障实验；详见[本机核对记录](../docs/integration/goframeshop-baseline.md#本机入口与运行状态)。尚无固定版本 D1/D3/N1 和证据不足对照包，不能把普通历史查询样本当作本任务全部验收完成。

## 目标

基于商城提交 `385d3649b4f38f1db82e65aade57b00e10badb02`，完成一个真实交易异常的人工证据闭环：受限查询指标 → Trace → 日志 → 保存脱敏快照 → 解释异常与是否已恢复。以此作为后续 Agent 的输入基础。

## 按工作项读取

先读 [AGENTS.md](../AGENTS.md) 和本任务的范围、验收。以下只加载当前工作项涉及的章节；完整路由见[渐进式阅读指南](../docs/reading-guide.md)。

| 工作项 | 读取章节 |
| --- | --- |
| 确认上游入口与接入缺口 | [代码基线](../docs/integration/goframeshop-baseline.md) §2、§4；需要决策背景时读 [ADR-0002](../docs/adr/0002-reuse-goframeshop.md) |
| 所有查询 Adapter 的共同约束 | [遥测契约](../docs/telemetry/data-contract.md) §1、§6–7；[调查设计](../docs/architecture/investigation-design.md) §5 |
| 指标 / 日志 / Trace | 遥测契约分别读 §3 / §2 / §4；不默认全量读取所有后端细节 |
| 告警响应核验 | 遥测契约 §5、§8；本切片不实现自动轮询 |
| Evidence 与人工报告 | 调查设计 §6；遥测契约 §6–7 |
| 冻结 Fixture、脱敏与案例选择 | [评测方案](../docs/evaluation/plan.md) §2–4；基线 §3 |
| 完成交接与文档同步 | [工作流程](../docs/development/workflow.md) §4–5 |

协议中的 Harness、Redis、工作台是后续设计，本任务不因阅读这些文档而扩展实施范围。

## 范围

1. 配置只读 Prometheus/Loki/Jaeger 地址、服务/环境白名单、时间与返回大小限制；凭据不进仓库。
2. 以真实样本核验范围查询、日志 metadata 和 Trace references，保存契约 Fixture。Adapter 放在 `internal/adapter/outbound/`，工具接口在使用方定义，不引入商城业务包。
3. 提供 metrics、trace search/detail、logs 的最小结构化查询接口与错误类型；schema/范围/时间/结果量校验先于请求。
4. 实现有限的离线 Fixture 回放与证据快照格式；Evidence 来源、查询、时间窗、采集时间、缺失/截断与 hash 完整。
5. 采集 D1/T3 或经人工确认更可诊断的现有案例，并配正常业务拒绝与缺证据对照；根据入库情况选择事实充分的样本，不凭测试名填结论。
6. 固定 source_quality、聚合所需结构字段与 count_basis 的 Fixture 契约，测试空结果、截断和未知覆盖；本切片不实现完整日志聚合工具。
7. 写一份人工调查报告和接入验收记录，分别记录已观察到什么、无法判断什么、故障是否已经恢复。

商城实验由操作员在独立上游 checkout 运行：先按上游文档配置并启动基础设施，确认 user/goods/order/paysim 端口空闲，再执行其 `make fault-trade`。本仓库不能把实验程序或 DB/故障注入权限暴露为 Agent 工具；不执行 infra-reset。

## 非本切片内容

不重建商城，不实现业务补偿，不改商城依赖/采集栈，不引入自研 KV/Tempo。完整 LLM Harness、日志聚合工具、Redis 调度恢复、SSE 工作台和自动告警轮询属于后续切片；本切片只核验告警响应契约，不声称自动告警入口已落地。

## 验收

- [ ] 固定上游 SHA 与后端版本，记录实际入口、采样和查询窗口；禁止把源码核对记为实测。
- [ ] Prometheus 查询返回真实 trade_* 数据；DB WaitCount 映射为等待；limit=0 和无样本不是零异常。
- [ ] Loki 按真实环境/服务/事件/关联 ID 查询；确认 body 非整行 JSON，stdout Fixture 另行解析。
- [ ] 已知 Trace 可以搜索及按 ID 读取；延迟事件 Link/恢复新 Trace 不被误转成同一棵树；越界 Span 被裁剪。
- [ ] EMPTY/UNAVAILABLE/PARTIAL 与正常结果区分；超时、429、截断和分页均有界；记录来源质量，缺乏新鲜度／覆盖依据时为 UNKNOWN。
- [ ] Freeze manifest 有逐场景时间、关联 ID、来源状态、SHA/哈希；无 failpoint、答案、测试标题、配置凭据泄漏。
- [ ] 人工使用相同证据可支持约定粒度的结论；证据不足案例保留未知项；包含正常业务拒绝对照。
- [x] 保存接入报告；链接、运行方式和 README 当前状态同步更新。其余复合验收项须完成固定版本故障案例后逐项确认，不能仅凭工程测试勾选。

## 验证方式

开发时运行受影响 Adapter/Guard 的单元测试；共享 Go 代码变更按仓库运行 `make test`、必要的 `make test-race`/`make vet`。真实后端集成测试须显式启用，不启动依赖时不能伪通过。新增测试命令须在实现时绑定真实脚本/退出码。

在线验证至少记录一次实际请求/脱敏响应、已知 Trace、关联日志与指标，并人工核查报告。T3 的瞬时故障不一定触发现有告警，应标 manual；自动 firing 验证另建持续负载场景。

## 交接到下一任务

按最新用户范围，先基于现有 Adapter 与历史 Fixture 实现任务 0003 的程序闭环；本任务验收仍是后续真实模型效果验证的前置条件。随后实现真实单 Investigator 调查；再做固定 Workflow 对照与恢复切片。不得重新回到小型商城搭建或自研 KV 路线。
