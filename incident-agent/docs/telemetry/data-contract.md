# 商城遥测接入契约

版本：v2.1，2026-10-28。商城设计代码事实基于 `385d3649b4f38f1db82e65aade57b00e10badb02`；三源 Adapter 的有限首批查询、匿名结构快照和离线回放已实现，见[使用指南](../integration/query-adapters.md)。历史在线响应已验证，固定基线故障场景仍待验收，见[接入报告](../reports/0002-readonly-integration.md)。下文未交付的分页、聚合与告警入口仍为目标契约。源码证据汇总见[基线](../integration/goframeshop-baseline.md)。

## 1. 身份、范围与来源

首版允许 `gateway-h5`、`order-service`、`goods-service`、`user-service`、`payment-simulator`。环境明确传入 `local` 或 `experiment`，不得默认混查。`inventory-service` 不是实际服务名。上游没有统一 tenant 字段，初版按部署和应用归属隔离，不声称具备跨租户查询能力。

| 概念 | stdout JSON / OTel | Prometheus | Loki OTLP |
| --- | --- | --- | --- |
| 服务 | service.name | service | service_name |
| 环境 | deployment.environment.name | environment | deployment_environment_name |
| 版本 | service.version | 不作为 trade_* 标签 | service_version（实际响应待验收） |
| Trace / Span | trace_id / span_id | 不作标签 | trace_id / span_id |
| 事件 | event.name | 不作标签 | event_name |
| 错误类型 | error.type | RPC 使用 outcome 分类 | error_type |
| 订单/操作 | order_id / operation_id | 不作标签 | order_id / operation_id |

不强制不存在的 `service.instance.id` 或旧 `deployment.environment`。Loki 字段是否为索引标签或 structured metadata 需以锁定版本实际响应确认；Adapter 规范化后二者统一为明确字段，缺失不能猜测。

## 2. 日志结构和事件语义

依据：[log.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/telemetry/log.go)、[logexport.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/telemetry/logexport.go)。

stdout 每行 JSON：timestamp、severity、service.name、service.version、deployment.environment.name、event.name、trace_id、span_id、operation_id、order_id、error.type、message，另有可选 attributes/error.stack。契约字段可能是空字符串；业务 ID 不是每条日志都有。

**Loki 的 body 是 message 字符串，不是整行 stdout JSON。** OTLP 将 event.name 等放进属性，并把业务 attributes 展平，例如 `consumer`、`event_id`。所以不能默认对 Loki body 加 `| json` 再取所有字段；离线 stdout 导入和线上 Loki 响应需要不同解析入口。

| event.name | 主要信息 | 解释限制 |
| --- | --- | --- |
| order.transition | order_id；from/to/reason | 条件更新确实改到行时记录 |
| inventory.reserved | operation_id、order_id；outcome/replayed/lines 或 error.type | 看 outcome 和错误，事件名本身不代表预留成功 |
| inventory.released | 同上 | 重放不等于再次释放库存 |
| payment.observed | order_id；payment_id/outcome | 付款观察，不等于订单最终履约成功 |
| payment.refund.requested | order_id；refund_id/payment_id/kind/attempt/outcome | 请求退款不等于已经退款成功 |
| message.redelivered | consumer/event_id/event_type | Inbox 已处理事件重到，仅确认，不等于重复业务效果 |
| workflow.retry | order_id；step/state，部分路径有 attempt | attempt 可能缺失或并发重复，不能据此精确结算重试次数 |

查询日志支持服务、环境、时间窗、Trace ID、order_id、operation_id、event_id、事件名和等级，所有值白名单/类型校验及转义。例子（模板由服务端生成，模型不可提交表达式）：

```logql
{service_name="order-service", deployment_environment_name="experiment"} | event_name="order.transition"
{service_name="goods-service", deployment_environment_name="experiment"} | operation_id="reserve-<订单号>"
```

首个切片必须验证环境选择器和 structured metadata 过滤；如果部署返回形式不同，在 Adapter 内修正并保留原始字段证据。查询上限默认 100 条、单条 2KiB，截断标注，保留来源引用；分页 token 绑定 scope 和规范化查询。

## 3. 指标与实际含义

来源：[metrics.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/telemetry/metrics.go)、[订单积压查询](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/app/order/internal/repository/metrics.go)、[Collector](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/deploy/otel/collector.yaml)。所有业务数据点有 `service`、`environment`；其他业务标签只允许下表列项，Histogram 的 `le` 是桶标签。

| Instrument | 类型/额外标签 | 当前口径 |
| --- | --- | --- |
| trade_order_create_total | Counter；outcome | 下单调用结果：pending_payment/reserving/rejected/duplicate/invalid/error；不是去重订单数 |
| trade_order_pending_payment | Gauge | 当前待支付订单数 |
| trade_workflow_backlog | Gauge；step | 数据库待处理工作数，不是 broker 队列深度 |
| trade_workflow_oldest_age_seconds | Gauge；step | 最老待处理工作的年龄；空积压为 0 |
| trade_rpc_duration_seconds | Histogram；peer/method/outcome | 商城封装的 gRPC 客户端调用耗时；outcome 为 ok/rejected/error |
| trade_payment_callback_delay_seconds | Histogram | 支付时刻到订单服务确认的耗时，记录于确认路径；不能覆盖所有尚未确认的付款 |
| trade_db_pool_in_use | Gauge；pool | 当前使用连接数 |
| trade_db_pool_limit | Gauge；pool | MaxOpenConnections；0 表示不设上限 |
| trade_db_acquire_timeout_total | Observable Counter；pool | **实际为 sql.DBStats.WaitCount：等待连接次数，不是超时次数** |
| trade_db_acquire_wait_seconds_total | Observable Counter；pool | 等待连接累计秒数 |

Collector 不自动添加 metric suffix；Histogram 查询仍使用 `_bucket/_count/_sum`。Prometheus 配置仅保留 `trade_*` 并移除 job/instance 等标签，因此不假定已有服务 `up`、CPU 或 broker 队列指标可用。

order 的 step：reserve、commit、close、tech_refund、after_sale_refund、outbox；goods 目前观测 outbox。后台队列与消息事件拓扑须分开建模。

### Agent metric_key 映射（固定模板已实现）

| metric_key | 查询含义 | 约束 |
| --- | --- | --- |
| order_error_rate | rate(create{outcome="error"}) / rate(create) | 分母是全部下单调用；无样本时未定义，不能填 0 |
| rpc_error_rate | error count rate / all RPC count rate | 与 TradeRpcErrorRateHigh 相同口径，保留拒绝与技术错误区别 |
| rpc_latency_p95 | Histogram quantile，按 service/peer 过滤 | 是调用方视角，不是直接证明被调服务内部原因 |
| workflow_backlog / workflow_oldest_age | 相应 Gauge，限定 step | 不映射为 RabbitMQ queue depth |
| db_pool_utilization | in_use / limit | limit=0 时不可计算，返回语义说明 |
| db_connection_wait_rate | rate(trade_db_acquire_timeout_total) | 对外使用等待命名；旧 db_acquire_timeout_rate 不启用 |
| db_connection_wait_seconds_rate | rate(trade_db_acquire_wait_seconds_total) | 和使用率/延迟结合，仍不证明连接泄漏 |
| payment_confirmation_delay_p95 | callback Histogram quantile | 对外标明只观察已确认样本；旧 payment_callback_delay 仅作语义迁移记录 |

RPC P95 的固定模板示例：

```promql
histogram_quantile(0.95,
  sum by (le) (rate(trade_rpc_duration_seconds_bucket{
    service="order-service",environment="experiment",peer="goods-service"
  }[2m])))
```

5 秒 OTLP 导出＋5 秒抓取不保证 5 秒内可见。固定窗口/step 限制总返回点与序列，默认最多 240 个点和 20 条序列；超过时调粗合法 step 或拒绝，不能只截响应而不约束后端成本。单次请求超时和总查询预算同时生效。恢复重查仍冻结 start/end，但允许迟到数据导致不同结果，保留 collected_at/attempt。

## 4. Trace 与跨链路关联

来源：[telemetry.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/telemetry/telemetry.go)、[RabbitMQ trace.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/rabbitmq/trace.go)、[outbox.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/eventbus/outbox.go)。默认 ParentBased＋TraceIDRatioBased，sampleRatio=1；Quiet 后台轮询例外，不记录 Span，日志也不填写可查询 trace_id。

- HTTP/gRPC 传播 W3C 上下文；模型只能读取标准化结果。
- 普通消息消费沿用发布链路；延迟消息新开 Trace 并 Link 到 Producer。
- 后台 `order.recover`、`order.tech_refund`、`paysim.notify` 开新 Root；关联 order.no、payment.id、messaging.message.id 和业务日志，不能强求一张完整 Trace 树。
- Jaeger Adapter 保留 references/links，不把链接伪造成 parent-child；get_trace 逐 Span 校验服务/环境，越界只返回脱敏依赖标识。
- Jaeger 2.11.0 内存存储。上游[集成测试](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/telemetry/collector_integration_test.go)使用 `/api/traces?lookback=1h&service=...`。按 ID 查询、分页及 tags 映射需由 Agent 切片实测，不假定通用游标已经存在。
- 默认返回最多 20 条 Trace 摘要、200 个 Span；超限标截断。已知 Trace 查不到，返回缺失来源，不推断“请求不存在”。

## 5. 告警输入

已有规则：[trade.yml](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/deploy/prometheus/rules/trade.yml)。

| 告警 | 条件 | 持续时间 |
| --- | --- | --- |
| TradeRpcErrorRateHigh | 1 分钟 RPC 技术错误率 >5%，且 increase 调用数 ≥20 | 30s |
| TradeWorkflowStuck | 最老待处理工作年龄 >30s | 15s |

没有 Alertmanager。目标 AlertSource 使用 `/api/v1/alerts`，仅自动接收 firing，规范化 labels、annotations.summary、activeAt；按告警名＋排序标签＋activeAt 生成稳定 episode ID，重复轮询不无限创建 Run，重新激活可新建 episode。该轮询器尚未实现。

短暂 T3/C1 可能自动恢复且从未达到阈值；人工请求应明确标 manual，不伪称真实告警。告警、用户摘要和模型上下文不能包含 fault_id、failpoint 名称、预期答案或测试标题。

## 6. 证据、缺失与权限

统一 ToolResult 记录来源、规范化查询、固定窗口、采集时间、截断、源状态、source_quality 和脱敏片段。EMPTY=成功查询但无结果；UNAVAILABLE=后端不可用；PARTIAL=截断或已知采集/采样缺失；AVAILABLE=本次返回可用，不承诺现实世界完整覆盖。

已查询到的证据可保存快照，原后端过期后仍可核查。日志已脱敏不免除 Agent 的白名单与二次脱敏；不要将 raw config、异常栈全文、用户资料或 SQL 结果直接输入模型。

首版工具只有只读 HTTP 查询能力；禁止任意地址、自由查询表达式及绕过 Guard 的管理 API。API 资源归属检查、后端访问限制和前端安全渲染分别验收。商城后台管理身份与 Agent 身份不共享。

## 7. 来源质量与聚合口径（新增目标契约）

ToolResult.source_status 表示本次返回可用程度；另外保存 source_quality，避免单一状态掩盖采集缺口。没有信号支持的字段使用 UNKNOWN，不伪造检查结果。

| 字段 | 值或含义 | 解释边界 |
| --- | --- | --- |
| reachability | REACHABLE／UNREACHABLE／UNKNOWN | 成功到达后端不代表查询有结果 |
| query_outcome | SUCCEEDED／FAILED | 成功响应还要验证返回格式 |
| returned_records | 已返回记录数 | 不是全窗口业务发生次数 |
| truncated | 是否明确截断 | 未截断仍可能被源采样／保留期影响 |
| freshness | FRESH／STALE／UNKNOWN，附判据与 checked_at | 仅在有已核验的采集／时间信号时判断；安静服务无日志不能直接判 STALE |
| coverage | COMPLETE_FOR_QUERY／PARTIAL／UNKNOWN，附 missing_reasons | COMPLETE_FOR_QUERY 仅表示源契约内查询覆盖，不表示现实完整采集 |
| latest_event_at | 实际返回记录中的最新时间，可空 | 与采集时间分开，不能独自证明源健康 |

初始没有可靠采集覆盖探针时 coverage 默认 UNKNOWN；已知截断、采样缺失或保留期缺口为 PARTIAL。EMPTY 可以同时具有 UNKNOWN 覆盖，报告不得由空结果断言无故障。多后端质量分别保存，不将一个源健康传播给其他源。

summarize_log_patterns 使用 query_logs 的同一白名单、Loki 字段映射与脱敏策略。首版最多扫描一次 100 条结果，在本地按 service／event_name／error_type／severity 和已验收 outcome 分组，最多返回 10 组、每组最多 3 条代表记录。缺失字段保留 unknown；日志 body 不按完整 stdout JSON 解析。

每组保存 observed_count、observed_first_at、observed_last_at、代表记录引用与关联 ID；顶层保存 count_basis=OBSERVED_SAMPLE、scanned_records、omitted_groups、聚合版本与来源质量。组数上限、原查询截断、未展开记录都要显示。样本计数不能写成全窗口故障数量；query_logs 后续展开也不保证补齐所有采样遗漏。

## 8. 告警身份与去重补充

fingerprint = hash(source＋环境＋alertname＋排序规范化标签)；episode_id = hash(fingerprint＋activeAt)。同 episode 的 observation 刷新不新建 Run，观察次数仅代表轮询次数；相同 observation_id 的持久化重试不重复计数。不同 peer／step／告警名不能仅因 service 相同归并。

成功轮询未返回某告警仅标 NOT_OBSERVED；查询失败标 UNKNOWN，均不直接表示业务恢复。持续 firing 的去重标记刷新保留期限，避免长告警重复创建；恢复后再激活使用新的 activeAt。轮询中断可能遗漏短暂事件，本设计不承诺完整告警历史。人工请求使用独立幂等键和 source=manual。
