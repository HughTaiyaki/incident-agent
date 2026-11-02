# 受控调查、证据报告与恢复设计

状态：**完整目标设计；执行与模型调用边界已实现，真实效果与持久化待验收/实现**。更新：2026-10-31。本文整合 2026-09-07 技术选型稿，并按商城实际代码和本次评审更新。已实现状态见 [README](../../README.md)，范围见 [ADR-0002](../adr/0002-reuse-goframeshop.md)。

整体产品架构、选型理由与图示见[主设计](../../03-日志诊断Agent详细设计.md)。本文维护执行协议。三源查询、来源质量子集及匿名快照回放已通过独立 CLI 实现，见[使用指南](../integration/query-adapters.md)；[离线 Harness](../development/offline-investigation.md)已实现脚本决策、范围/次数/时间限制、证据保存与报告 Guard；任务 0005 已实现[模型输入与内存 Token 预算](../development/model-provider.md)及 HTTP Provider；真实供应商尚未联调，下文的持久 Run、告警与工作台核查闭环仍为目标契约。离线模式只有 INVESTIGATING/SYNTHESIZING，范围由操作者预先冻结，恢复判断暂只接受 UNKNOWN。

## 1. 目标与选择

用户从商城告警或人工请求发起调查，获得影响范围、时间线、证据支持的主假设、备选解释、未知项和下一步建议。工作台支持取消、证据核查、历史报告、人工反馈与恢复证据关联。修复与交易补偿在 Agent 外执行。

首版单 Investigator＋确定性 Harness。固定查询 Workflow 保留为对照；当效果相当且更便宜时，诚实记录它更适合该场景。无多 Agent 自由讨论、通用聊天、向量库、全公司拓扑发现或自研存储。

模型只提交一个工具调用或报告草稿；代码决定范围、执行、记录和停止。报告准确性由独立评测衡量，不因引用 ID 存在而保证。

## 2. 输入、接口与持久模型

Incident 输入：alert_id、service、environment、started_at、summary、initial_window_minutes；告警来源另记 prometheus/manual。自动来源由服务端规范化为 fingerprint 与 episode_id，alert_id 采用 episode_id；manual 使用请求幂等键。tenant/角色由服务端身份取得，初版固定单租户。POST 最大 16KiB，summary 最多 2,000 字符。scope 不受用户摘要或遥测文本扩权。

当前实现是下表的手动入口子集，见 [API 指南](../development/http-api.md)；其余为目标设计。

| 目标接口 | 行为 |
| --- | --- |
| POST /v1/incidents | 按 tenant＋source＋alert_id 幂等创建 Incident/首个 Run，202 返回 ID |
| GET /v1/incidents | 归属过滤、游标分页 |
| GET /v1/runs/{id} | 状态、阶段、预算、最新事件序号和报告摘要 |
| GET /v1/runs/{id}/events | SSE；Last-Event-ID 断线回放 |
| GET /v1/runs/{id}/report | 持久报告或 409 NOT_READY |
| GET /v1/runs/{id}/evidence/{evidence_id} | 检查 Run/身份归属并返回脱敏快照 |
| POST /v1/runs/{id}/cancel | 幂等取消；返回已持久状态 |
| POST /v1/incidents/{id}/runs | 显式新调查或恢复验证、新预算，保留旧 Run；校验 purpose 和 parent_run_id，不允许调度器自动无限重开 |
| POST /v1/runs/{id}/feedback | 后续工作台切片：追加人工评价/确认原因/处置与恢复证据，不改写原报告 |

| 模型 | 关键字段 |
| --- | --- |
| Incident | id、tenant、alert_id、source、fingerprint、episode_id、service、environment、started_at、last_seen、observation_count、source_state、summary |
| Run | id、incident_id、purpose、parent_run_id、status、phase、state_version、lease_epoch、owner、lease_until、scope、budget、cancel_requested、model_config、policy_version、created_at、deadline_at |
| ModelCall | call_id、step、status、context_digest、预算预留、usage/estimated、决策结果、error_code |
| ToolOperation | op_id、step、tool、canonical_args、args_hash、status、attempt、started_at、result_digest、evidence_ids、error_code |
| Evidence | id、run_id、tenant、origin_run_id、origin_evidence_id、type、source、query_id、canonical_query、event_time_range、collected_at、record_refs、redacted_excerpt、digest、truncated、source_status、source_quality、aggregation_version、count_basis |
| Hypothesis | claim、supporting_ids、contradicting_ids、unknowns、strength |
| RunEvent | sequence、run_id、type、at、payload |
| Report | run_id、result、impact、timeline、facts、primary_hypothesis、alternatives、unknowns、next_actions、recovery_assessment、stop_reason、policy_version |
| Feedback | author、at、评价、人工确认原因、处置说明、恢复证据引用及归属 |

ModelCall 是预算审计，不保存私有思维链。证据 ID 由系统产生，模型不能提供新 ID。反馈是人工记录，不自动成为原调查的事实或准确率标签。purpose 默认 diagnosis；verification 必须引用同一归属 Incident 下的已完成 parent_run_id。recovery_assessment 记录 STILL_ABNORMAL／RECOVERED／UNKNOWN、观察窗口、对比口径和证据；不由 COMPLETED 或告警消失推导。

## 3. 状态、停止与预算

Run.status：QUEUED → RUNNING → COMPLETED / FAILED / CANCELLED。Worker 崩溃后恢复 RUNNING，不直接标失败。Report.result：DIAGNOSED / INCONCLUSIVE；与执行是否完成分开。已有证据足以报告“已恢复”或“未见当前故障”时明确写出，不强造故障根因。

Run.phase：SCOPING → INVESTIGATING → SYNTHESIZING；明确证据缺口时可从汇总回调查，仍受同一预算约束。Harness 校验迁移。

| 限制 | 实验初值与规则 |
| --- | --- |
| 模型决策 | 总共 12 次，调查至多 11 次，留 1 次报告；格式错误/被拒绝决策也消耗 |
| 后端查询 | 16 次实际请求；每页、每次重试都计费，发出前扣减 |
| 模型 Token | 输入＋输出总预留上限 36,000；每次发起前原子预留 |
| 单次输出 | 最大 1,500，显式传给 Provider |
| 报告预留 | 9,000 Token，**包含输入与输出**，是汇总阶段的下限，够写一次报告并修正一次；汇总可使用全部剩余额度，构建器压缩报告输入使其与输出上限合计不超额 |
| Run 时限 | 创建起 180s，包含排队和恢复；不重置 |
| 模型/工具超时 | 分别最多 30s/5s，同时不得超出剩余 Run 时间 |
| 报告时间 | 调查阶段为一次报告调用保留至多 30s；不足时停止新调查并尽力汇总，截止后仅确定性模板 |
| 无进展停止 | 连续 3 次无新的记录/事实或有用反例；空结果也记录，规则标为启发式 |
| 相同查询 | 同一规范化查询和窗口最多两次，包括 UNKNOWN 重试；分页使用受控游标 |

每次上下文上限取模型可用窗口的 60% 与本次 Token 可用额的较小值，再扣输出预算；不能只按模型窗口装配。报告预留不能被调查占用。输出不足以覆盖报告时减少上下文和非必要字段，仍不合格则确定性 INCONCLUSIVE，保留所有原始证据供人工阅读。

响应返回按供应商 usage 核算；超时/响应丢失按预留额消耗，不回补未知用量。无可靠 tokenizer 时记录保守估算，不保证账单严格等于本地预算。超限、后端故障、模型不可用尽可能生成含停止原因的 INCONCLUSIVE；FAILED 用于无法形成可用报告的执行错误。

## 4. 执行与上下文

1. 校验 owner/epoch/租约、state_version、取消、截止时间与预算。
2. 装配规则、Incident、scope、事实及引用、假设/反例/未知项、最近三个工具摘要和阶段 schema；不上传大批原始日志。
3. 原子记录 ModelCall 与预算预留，再请求模型；返回决策持久化后才继续执行。
4. 对模型结果做 schema 和权限检查。非法调用只记录拒绝与有限反馈，绝不先执行后检查。
5. 合法查询原子创建 PLANNED；实际请求前置 IN_FLIGHT 并扣查询预算。保存的 canonical_args 决定恢复参数。
6. 查询结果在进程内限量、规范化、脱敏；**一次有界原子提交**保存 Evidence、attempt 结果、Operation、Run 状态和事件。避免首版额外实现“孤立证据＋二次补提交”协议。
7. Report Guard 校验草稿。最多一次有预算的修正；无剩余额度则采用确定性模板。
8. 原子保存报告、COMPLETED 和事件；通知仅提示，报告通过 GET 读取。

证据去重基于源记录身份/规范化内容，采集时间不同不自动算新事实。相同 trace 的多份摘要不算独立支持。模型摘要不是原始证据；压缩保留引用、反例和未知项。不保存模型私有推理过程，只保存工具决策、状态和可审计结论。

## 5. 工具与权限

| 工具 | 输入/输出边界 |
| --- | --- |
| get_service_topology | 服务端登记的商城依赖，最多 20 节点；静态拓扑不是当前故障证据 |
| query_metrics | 仅契约 metric_key，固定模板、合法 service/peer/step/窗口；不接受自由 PromQL |
| query_traces / get_trace | 最多 20 摘要/200 Span；链接和范围校验；不假设后端支持游标 |
| query_logs | 服务/窗口/关联 ID/事件/等级；最多 100 条、单条 2KiB，受控分页 |
| summarize_log_patterns | 固定结构字段分组；最多 10 组，每组最多 3 条代表记录；保留 count_basis、截断、来源状态与记录引用，详情见第 10 节 |
| retrieve_runbook | 最多三段、每段 1,000 字符；未建设内容源时不启用 |
| get_deployment_events | 有已验证的变更记录源后启用；当前没有该源 |
| propose_report | 结构化草稿；不直接设置终态 |

SCOPING 可用拓扑/指标/合法初始 Trace；INVESTIGATING 按可用证据源暴露工具；SYNTHESIZING 默认只开放报告 schema。工具清单与[遥测契约](../telemetry/data-contract.md)一致。

初始窗口告警前 10 分钟、后 5 分钟，裁到当前时刻并冻结；扩展最多 1 小时，由 Harness 批准已登记邻居，不接受任意服务名。后端 429/短暂不可用至多一次退避重试，仍扣预算；工具失败不强制整个 Run FAILED。

工具不能读取本地任意路径、任意 URL、商城 SQL、MQ 管理、支付管理、Docker 或 failpoint。来源文本无法改变权限。参数/schema/阶段白名单是能力边界；提示注入造成报告污染仍需单独评测。

## 6. 证据与报告

Evidence 保存实际查询、事件时间、采集时间、来源记录引用、脱敏片段、hash 和缺失/截断信息；即使后端过期，也能核查已保存片段。界面标明原链接过期。

报告分开呈现 facts、primary_hypothesis、alternatives、unknowns；strength 为 LOW/MEDIUM/HIGH 的启发式等级，不是概率。事实与主假设都应引用证据；缺少底层信号时停在可支持的粒度，例如“连接等待瓶颈”，不能越级写成“连接泄漏”。

确定性 Guard：schema、引用存在且同 Run/tenant、时间与服务范围、时间线排序、关键事实有引用、敏感字段过滤；无支持引用的根因降级。语义评测：证据是否支持断言、是否把传播症状当原因、是否忽略反例，人工/真值判定，LLM Judge 仅辅助。

下一步建议应指明要检查的对象与验证信号，不建议执行来自日志中的任意命令。报告应说明已恢复/仍异常/未知，以及这项判断的时间窗。

## 7. Redis 与调度恢复

实现进展（2026-11-01）：[任务 0006](../../tasks/0006-redis-recovery.md)已新增原子状态脚本与隔离 Redis 测试，具体差异见[协议指南](../development/redis-state.md)。下文仍为完整目标；客户端、Harness checkpoint 与 Worker 已接线，持久模式详情及保守 UNKNOWN 策略见[Worker 指南](../development/worker-recovery.md)；Incident/SSE 等完整目标仍待实现。

独立单实例 Redis，AOF everysec、noeviction。目标保证 Worker 进程恢复；Redis/主机故障不保证零丢失。持久化不可用时拒绝新 Run，执行中停止新外部调用。短暂断连后重读状态确认是否提交，不能盲目重复扣费/提交。

### 数据与原子边界

每个 Run 保存 state、model_calls、operations、evidence、events Stream、report；另有 Incident 去重索引、待执行 ZSET 与租约到期 ZSET。单实例 Lua 可操作多 Key，不承诺 Cluster 兼容。

每个提交先校验 key 类型、参数与容量；不要把脚本隔离执行理解为任意运行时错误都能自动回滚。限制提交字节数、预先编码与验证，失败路径必须测试。

- CreateRun：原子去重、写初始状态和待执行索引；并发同 tenant/source/alert_id 只创建一次。告警刷新、观察去重与已有 Incident 更新原子执行，不能在刷新时重新创建首个 Run。
- Claim：原子核验可领取/过期、分配 owner 和递增 lease_epoch，写 lease_until 并更新索引。
- Renew：使用 Redis 时间，默认 15s 租约/5s 续租；只改租约，不增加业务 state_version。
- BeginCall：所有权、epoch、租约、state_version、取消、deadline、预算一次校验，再预留或扣费。
- CommitResult：同样核验所有权并幂等检查 op_id/attempt，再写证据、结果、状态和事件。已提交调用重复确认不重复产生证据或扣费。
- Complete：同一原子操作写报告、终态、事件与保留期限，清理调度索引。
- Cancel：排队 Run 可原子 CANCELLED；运行中先写标志并阻止新 BeginCall/Complete，由当前或接管 Worker 收尾。完成先赢则返回已完成；取消先赢则不能再完成报告。

### 任务如何重新被发现

领取时从 ready 移出并加入 leases，续租同步 leases 分数；调度器同时扫描到期 ready 和过期 leases，接管时重新读取真实状态。索引扫描与原子 Claim 共同保证候选可发现且只能有一个有效 owner。启动恢复和周期扫描均覆盖过期任务；终态条目及时清除。

到期或取消的任务必须有无模型的收尾路径。运行中取消且 Worker 全部停止时，API 只能承诺“取消已请求”，待 Worker 恢复/接管后终结，不虚称已撤销外部请求。

### 中断窗口

| 中断位置 | 恢复行为 |
| --- | --- |
| 模型预留后、结果未持久 | ModelCall 标 UNKNOWN，预留保守消耗；剩余预算允许才再决策 |
| 模型决策已持久、PLANNED 未执行 | 复用已保存决策与参数，不重新问模型 |
| IN_FLIGHT 无持久结果 | 原 attempt 标 UNKNOWN；同 op_id 新 attempt 最多重查一次，再扣预算 |
| 结果原子提交成功、响应丢失 | 按 op_id/attempt 读回提交结果，不再次查询 |
| 结果已提交、下一轮前 | 使用已保存证据和快照继续 |
| 最终报告已提交、HTTP/SSE 返回前 | 直接读取原报告，不重新生成 |
| 旧 Worker 在接管后返回 | 拒绝写入；可写自身进程审计日志，不改终态或证据 |

IN_FLIGHT 可有 SUCCEEDED/FAILED/UNKNOWN 的 attempt；新尝试不覆盖旧尝试历史。只读查询可能因迟到数据不同，保存每次结果和 collected_at。op_id 不会让外部 API 去重，不承诺 Exactly Once 或绝不重复模型计费。

### 容量和清理

活跃 Run 不自动 TTL。终态 Run/证据/事件/报告统一保留 7 天，Incident 去重标记至少保留 30 天；仍持续观测到的 episode 刷新该标记保留期，避免一次长告警因标记过期重新开 Run；历史详情过期时返回明确的已过期状态，不偷偷新开 Run。每 Run 证据最多 2MiB、事件 500 条、报告 64KiB，事件容量为终态保留位置。当前 Worker 切片使用单 JSON 状态，额外 checkpoint 上限 3MiB、整体编码状态 7MiB、40 条调用操作；事件 Stream 尚未实现。

全局容量阈值为活跃 Run 收尾预留余量；noeviction 不能替代容量控制。当前 CreateRun 按活跃数上限和每活跃 Run 8MiB 内存预留原子拒绝新 Run，详见[协议指南](../development/redis-state.md#2-调用契约)。超限停止扩张，以已有证据生成说明，不能因停止记录而仍继续调用。清理涉及索引一致性，要验证终态、过期索引和重新调查。

## 8. 工作台与安全

目标页面：告警列表、Run 详情、证据详情、历史报告。默认展示报告引用的冻结快照；实时重查另建有预算的新 Run，报告／旧 Evidence 不可变。详情显示阶段/预算、查询活动、假设/反例、后端缺失、最终报告。反馈与外部处置结果另存，不修改原结论。

SSE 使用单调 sequence，事件包含开始、阶段、工具开始/成功/失败、证据新增、报告就绪、失败和取消。快照与 sequence 原子读取，从该序号后读持久 Stream；Last-Event-ID 超出保留范围发 reset，前端重拉快照。慢连接缓冲最多 64 条，断开后重连；终态报告 GET 是事实来源。

同源 Cookie 会话；创建/取消/反馈做跨站请求防护；SSE URL 不放长期 Token。日志按文本渲染，Markdown 禁止原始 HTML 和危险链接。查询/证据每次检查归属。初版没有商城多租户遥测隔离，不把这些应用检查写成企业级多租户能力。

## 9. 验收与实施

[任务 0002](../../tasks/0002-goframeshop-integration.md)保留固定版本故障数据验收；按用户暂不修改商城的要求，[任务 0003](../../tasks/0003-offline-investigation.md)先完成不依赖上游变更的离线内核。后续每个切片同时提供 Fake Provider 的程序验证与固定真实样本上的效果验证，见[评测方案](../evaluation/plan.md)。通过标准包括：完整调查、正确拒答、查询/预算约束、中断恢复、证据不丢、人工可核查、处置后可对照恢复信号。

实现后再新增对应配置与命令，并更新 README 当前状态；设计默认数字均为待验证实验初值，不是已测性能。

## 10. 告警、聚合与核查补充协议

### 10.1 精确告警归并

fingerprint 包含 source、环境、alertname 和排序后的完整标签；标签过滤须有版本，不能删除会区分症状的 service／peer／step。episode_id 由 fingerprint＋activeAt 生成。告警源只允许固定地址，客户端文本不能控制后端 URL。

每轮轮询生成 observation_id（该轮唯一 ID＋episode_id），持久刷新重试不得重复增加 observation_count；不同轮次增加的是“观测次数”，不是异常发生次数。归并到同 episode 的活跃／终态 Run 都不新开调查。不同 activeAt 生成新 episode，并受入口速率和容量控制。manual 幂等键与自动来源分开，不因摘要文字相同自动归并。

没有可靠恢复事件时 source_state 仅表示 FIRING／NOT_OBSERVED／UNKNOWN。轮询失败为 UNKNOWN；成功快照未含该告警仅为 NOT_OBSERVED，不直接标业务恢复。首版跨服务和不同告警只显示候选关联，不自动合并；通知静默尚不实现，也不能改变上述状态。

### 10.2 确定性日志聚合

实现进展（2026-10-29）：[任务 0004](../../tasks/0004-log-patterns.md)已随保存的日志查询自动生成摘要，见[使用指南](../development/log-patterns.md)。当前尚未暴露独立 summarize_log_patterns 决策；下述计数、范围与代表记录规则已应用，有界模型上下文和内存 Token 核算见任务 0005。

默认结构键为 service＋event_name＋error_type＋severity，可附加已经核验的 outcome；缺失字段保留 unknown，不从 message 猜造错误类型。高基数字段 order_id／trace_id 不作为模式键，但作为展开线索保留。模板与字段规范化必须版本化。

首版在一次限量 query_logs 结果上聚合（最多 100 条），count_basis 固定为 OBSERVED_SAMPLE，observed_count 仅计样本内记录。输出最多 10 组、每组 3 条代表记录，列出 scanned_records、omitted_groups、truncated、observed_first_at／observed_last_at；时间边界只代表样本。若以后采用后端全窗计数，先验收模板，再使用 BACKEND_WINDOW 字段，不能悄悄替换口径。

代表记录按稳定顺序选择，覆盖组内首次／末次及不同 outcome（存在时）；输出摘要预留正常或恢复组的位置，保留未展示组数量。该策略仍不能保证反例完整，必须允许按事件和关联 ID 单独查询。摘要与样本缺失不能解释为没有异常／没有恢复。

Guard 校验聚合与普通查询同样的范围和窗口；聚合每次实际后端请求、分页、重试都计入同一预算，不额外无限抓取。若展开记录已保存在当前 Run，读取缓存不消耗后端请求次数；仍计入模型上下文与时间预算。所有模型选择操作受决策次数限制。

### 10.3 来源质量与证据展开

source_quality 由 Adapter 依据真实响应生成，字段见[遥测契约](../telemetry/data-contract.md)。报告上下文保留质量缺口；模型不能把 EMPTY／UNKNOWN 升级为“数据完整且正常”。

工作台从断言跳到本 Run 的 Evidence，再显示规范化查询、版本、窗口、采集时间、脱敏快照与质量标记。快照只保证在保留期内可读；过期明确显示已过期。展示内容 hash 用于检测变动，不宣称具备防篡改审计存储。

“重新查询”首版通过显式创建新 Run 完成，复用已验证的查询参数和范围，经过当前身份／scope／预算检查。新结果保存在新 Run，页面并排显示历史和最新采集时间；不修改旧 Evidence，也不绕过预算直连后端。若申请扩窗，仍受最多一小时和登记服务范围约束。

恢复验证同样创建 purpose=verification 的新 Run，固定新窗口并关联 parent_run_id；对比相同指标口径和人工提供的负载条件。父报告只以经归属检查的只读参考加载，不把旧 Evidence ID 伪装成本 Run 引用。需要引用的父证据须再次校验当前身份、环境和服务范围，通过限量导入生成本 Run ID，保留 origin_run_id／origin_evidence_id 和原时间窗，计入证据容量；历史证据明确标为对照，不能证明当前状态。

允许父证据属于历史时间窗的唯一例外是此受控对照导入，不能据此查询 scope 外的当前数据。父证据已过期时标明不可回查；不能由摘要还原原始证据。人工反馈关联两个 Run，恢复结论记录比较窗口、缺失信号和同期其他变更。人工点击确认不自动充当评测真值。
