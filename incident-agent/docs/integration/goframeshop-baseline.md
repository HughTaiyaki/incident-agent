# GoFrame 商城代码基线与接入差距

核对日期：2026-10-26。上游：[HughTaiyaki/goframeshop](https://github.com/HughTaiyaki/goframeshop)。固定提交：`385d3649b4f38f1db82e65aade57b00e10badb02`（Task 0021: fault experiments, and two fixes they found）。

本次从该提交读取业务、遥测、部署与实验代码。未启动商城、未运行其故障实验、未重新测量性能。用户确认商城已完成；本文件说明 Agent 可以依赖的实现和仍需验证的接入事项，不重开商城建设任务。

## 本机入口与运行状态

2026-10-28 用户确认商城目录为工作区同级的 `../GoframeShop/goframeshop/`（相对工作区根目录）。以下为后续只读核对结果，不替换上述 2026-10-26 设计基线，也不改写已冻结样本的来源。

| 核对项 | 本次观察 |
| --- | --- |
| 工作区 HEAD | `2f25dc5a6fd10cca19323389d39586ff794526f3`，Task 0025: simple React demo frontend |
| 未提交内容 | 已跟踪修改位于文档，另有未跟踪的 `docs/learning/`；保留用户现有内容 |
| 服务端口 | user `31001`、goods `31005`、order `31007`、paysim `31009/8399` 均已被运行进程占用 |
| 构建信息 | 通过进程可执行路径读取的四个二进制文件均为 `vcs.revision=bb921e92b39681164939aafe79ccb9cf043fa999`、`vcs.modified=true`；这不是干净提交的可复现构建证明，也不能完整追溯历史遥测 |
| 实验状态 | 未启动或停止商城服务，未运行故障注入；目前不满足实验端口空闲条件 |

继续接入时先读商城 `AGENTS.md`、`docs/index.md`，故障场景再读 `docs/development/fault-experiments.md`。不要把当前 HEAD 自动写入已有 Bundle 的 `upstream_sha`；历史样本继续保留 `unknown`。

当前 `tests/fault/cluster.go` 的端口检查还包括 gateway-h5 `8199`，即使故障实验不启动网关，也必须确认该端口空闲。准备固定版本样本时，先按 §5 完成基线比较与验证，再在独立 checkout 构建；记录实际源码、构建修改状态、后端版本和逐场景起止时间。独立 checkout 仍共享本机端口及基础设施，不能据此宣称运行隔离。

上游 `TestFaultTrade` 使用命名子测试，可在满足实验前置条件后使用 `GOFRAMESHOP_FAULT=1 go test ./tests/fault/ -run '^TestFaultTrade$/^T3$' -count=1 -timeout 20m -v` 选择 T3。这里只核对了选择入口，未执行该命令；实验仍会启动服务、写业务数据并注入故障。完整实验包含清空延迟队列的 T9，不应把 `make fault-trade` 当成无副作用的查询检查。实验真值与原始日志仍按 §3–4 隔离。

## 1. 已实现能力与源码证据

| 能力 | 源码依据 | 对 Agent 的意义 |
| --- | --- | --- |
| 订单创建、条件迁移、稳定操作号、恢复扫描 | [order.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/app/order/internal/logic/order/order.go) 的 Create/reserve/transition/Recover/drive | 调查 RESERVING、PAYMENT_CONFIRMING、CLOSING 时要区分等待重试与最终失败 |
| 库存 Reserve/Commit/Release、墓碑、结果重放 | [inventory.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/app/goods/internal/logic/inventory/inventory.go) 的 run/release | 同 operation_id 重试不等于重复扣库存；检查 outcome、replayed 与后续状态 |
| 支付通知验签后查询、支付关单、晚到付款退款 | [paymentflow.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/app/order/internal/logic/order/paymentflow.go)、[notify.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/app/paysim/internal/notify/notify.go) | 回调失败不等于付款失败；恢复/退款是商城职责 |
| 同事务 Outbox、发布确认、重试与 Inbox | [outbox.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/eventbus/outbox.go)、[inbox.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/eventbus/inbox.go) | 重投可能是正常恢复，不能直接判断为重复业务执行 |
| RabbitMQ 领域事件及队列 | [topology.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/rabbitmq/topology.go)、[contracts.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/eventbus/contracts.go) | 注册静态异步拓扑，按 event_id 关联发布与消费 |
| 统一 OTel 初始化、资源、采样、后台 Root | [telemetry.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/telemetry/telemetry.go) | 查询真实 service.name；跨后台任务不能只按一个 Trace ID |
| 七类业务日志事件、JSON/OTLP 日志 | [log.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/telemetry/log.go)、[logexport.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/telemetry/logexport.go) | stdout JSON 与 Loki 消息体不是同一种表示，须分别解码 |
| 十个 trade_* instrument 与业务标签 | [metrics.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/telemetry/metrics.go)、[积压查询](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/app/order/internal/repository/metrics.go) | 采用真实指标和语义，不沿用旧模板名 |
| Collector → Jaeger/Prometheus/Loki | [compose.yaml](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/compose.yaml)、[collector.yaml](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/deploy/otel/collector.yaml) | 复用后端，Agent 只建设查询 Adapter |
| 系统级故障实验 | [experiments_test.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/tests/fault/experiments_test.go)、[cluster.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/tests/fault/cluster.go)、[invariants.go](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/tests/fault/invariants.go) | 实验操作员可复现故障并评分，但原始报告不交给 Investigator |

上游[任务 0021](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/tasks/0021-fault-experiments.md)记载 11/11 通过及连续三次一致。这是**上游报告结果**，不是本次运行结果，也不是 Agent 诊断成绩。上游部分概述还保留“日志/指标未完成”的旧文字，本基线以实际代码、配置和对应任务为准。

## 2. 服务与部署事实

遥测服务名直接核对各 `app/*/main.go` 的 `telemetry.Start`。首版调查范围只启用表中前五项，其余按场景显式加入白名单。

| 目录/进程 | 遥测 service.name | 首版作用 |
| --- | --- | --- |
| app/gateway-h5 | gateway-h5 | 用户 HTTP 入口，调用 order/goods/user 等后端 |
| app/order | order-service | 交易编排、恢复任务、事件发布和到期消费 |
| app/goods | goods-service | 商品、库存、购物车和订单事件消费；不是 inventory-service |
| app/user | user-service | 用户与收货地址 |
| app/paysim | payment-simulator | 外部支付模拟器与通知 |
| app/gateway-admin | gateway-admin | 后台 HTTP 入口 |
| app/admin | admin-service | 管理员身份 |
| app/file | file-service | 文件元数据 |
| app/search | search-service | 商品搜索 |
| app/banner | banner-service | 轮播与手工位、商品变更消费 |
| app/interaction | interaction-service | 点赞、收藏、评论 |

业务框架：[go.mod](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/go.mod) 的 Go 1.24.5、GoFrame 2.10.2、gRPC/Protobuf、OTel。Agent 独立维护工具链版本（见[发布检查](../development/release-checks.md)）与 net/http 骨架，不跟随商城框架迁移。

| 组件 | 上游锁定版本 | 默认宿主机入口/限制 |
| --- | --- | --- |
| OTel Collector | 0.130.0 | OTLP gRPC 4317、HTTP 4318；指标导出 8889 |
| Jaeger | 2.11.0 | 查询/UI 16686；内存存储，重启丢 Trace |
| Prometheus | v3.5.0 | 9090；保留 7 天；每 5 秒抓取和评估 |
| Loki | 3.5.3 | 3100；文件存储、保留 7 天、auth_enabled=false |
| 商城 Redis | 7.4.2 | 6379；AOF/RDB 关闭，/data tmpfs，仅缓存 |
| MySQL / RabbitMQ | 8.4.11 / 4.2.0-management | 商城业务依赖，不向 Agent 开放管理或写权限 |

配置依据：[Compose](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/compose.yaml)、[Prometheus](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/deploy/prometheus/prometheus.yml)、[Loki](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/deploy/loki/loki.yaml)。端口可由环境变量覆盖。Compose 编排基础设施，不能写成已经一键启动全部商城业务与 Agent。现有端口映射也不是已落地的 Agent 网络隔离方案。

## 3. 已有故障实验如何复用

| 商城实验 | 已实现注入/操作 | 适合验证什么；不能直接推出什么 |
| --- | --- | --- |
| T1/T2 | 并发购买、重复 requestId | 正常业务拒绝/幂等对照；不是技术故障准确率样本 |
| T3 | 预留提交后返回错误 | 已提交但调用方结果未知、重试恢复；不是网络丢包实测 |
| T4/T6 | 迟到预留、已支付订单收到到期事件 | 正确拒绝迟到工作；不能把拒绝误报为系统故障 |
| T5 | 付款记账前 pause，与取消竞争 | 支付晚到与技术退款时间线 |
| T7 | 消费事务后 ACK 前 exit | 消费重投与幂等恢复；仅凭重投日志不能确认精确崩溃点 |
| T8 | 发布确认后标 SENT 前 exit | Outbox 重发、同 event_id 去重；同样需要限制诊断粒度 |
| T9 | 清空到期延迟队列 | 数据库到期扫描兜底；“没有消费日志”本身不证明队列被清空 |
| T10 | Protobuf 快照兼容检查 | 工程测试，不属于在线故障诊断场景 |
| C1 | 订单写入后 exit | 商城进程中断后恢复，不是 Agent Checkpoint 恢复测试 |

源码：[实验](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/tests/fault/experiments_test.go)、[failpoint](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/utility/failpoint/failpoint.go)、[报告结构](https://github.com/HughTaiyaki/goframeshop/blob/385d3649b4f38f1db82e65aade57b00e10badb02/tests/fault/report.go)。failpoint 每进程触发一次；pause 最多两分钟；exit 不执行 defer，不保证日志/Trace 缓冲已导出。

实验程序启动 user、goods、order、paysim 四个进程，设 `environment=experiment`、支付超时 8s、恢复周期 1s，端口占用会拒绝运行。它不启动 gateway-h5；不能把此实验报告解释为浏览器→网关完整链路验收。

报告目录 `reports/fault/<时间>/` 有 report.json、report.md 和 work；Result 记录 ID、标题、故障、耗时、订单和不变量等，当前结构没有逐场景起止时间和 Trace ID 字段。Agent 评测导出需补自己的采集清单，不假设报告已有全部关联信息。

## 4. 接入差距与处理决定

| 发现 | 后续处理与验收 |
| --- | --- |
| DB timeout 命名实际记录 sql.DBStats.WaitCount | Adapter 暴露为“连接等待次数/速率”；禁用无条件“超时”结论；见遥测契约 |
| trade_workflow_backlog 是业务待处理行数 | 不当成 RabbitMQ 队列长度；消费者暂停根因需额外可信信号 |
| 已有两条告警没有 Alertmanager 推送 | Agent 轮询 Prometheus /api/v1/alerts 或人工发起，均待实现 |
| 短时故障与告警持续阈值不匹配 | 一次性故障走人工/离线调查；自动告警需持续负载与独立场景，不偷偷改规则提高命中率 |
| metrics 5s 导出＋5s 抓取，Trace/log 异步批量导出 | 查询前做有界可见性等待；超时标 PARTIAL/UNAVAILABLE；不得把缺失填零 |
| failpoint 向 stderr 打印注入点名字；cluster 合并 stdout/stderr | 禁止原始 work/logs 整包入模；OTLP 数据和离线导出都做泄题检查 |
| order.reserve 普通 WARN 文本有订单号但不保证结构化 order_id | 订单字段查询不覆盖所有日志，需按相关 Trace 回查；不要假定每行都有业务 ID |
| Quiet 日志没有可查 Trace；恢复与延迟消费另开 Trace | 关联 order_id/operation_id/event_id、Span Link 与受控标签；空 Trace 不代表业务未发生 |
| get_deployment_events 没有已存在的统一数据源 | 首版不暴露空壳工具；后续配置版本记录准备好再启用 |
| 旧 E1–E5 故障类型不全有注入器 | DB 争用、稳定延迟、配置错误等标待补，不宣称现成 12 类诊断集 |
| 后端无已验证的只读认证/租户隔离 | 单租户受控环境；部署查询代理/网络限制并验证，不能写成已完成权限能力 |
| 商城 Redis 不持久 | Agent 单独部署 Redis；不得更改商城缓存语义来复用它 |

补充：stdout JSON 中 message 会清洗，但 attributes/stack 不应被当作对任意输入都已彻底脱敏；Agent 采用字段白名单与二次脱敏，只保存支持结论所需片段。不能把上游的脱敏实现宣传成全字段隐私保证。

## 5. 更新基线的维护步骤

1. 记录新的上游 SHA 与日期，比较 app/*/main.go、utility/telemetry、utility/eventbus、utility/rabbitmq、deploy、tests/fault 和 go.mod。
2. 对照实际代码更新服务/字段/指标语义，特别是 DB 等待、业务 outcome、积压口径、日志传输格式与 Trace Link。
3. 在受控环境验证三个后端，保存脱敏原始响应、模板版本和关联例子；只读代码核对不能标“集成通过”。
4. 重跑 Adapter 契约和冻结 Fixture；若语义变化，版本化旧 Fixture，不静默改已有评测真值。
5. 同步[遥测契约](../telemetry/data-contract.md)、[评测方案](../evaluation/plan.md)、当前任务及 changelog。
