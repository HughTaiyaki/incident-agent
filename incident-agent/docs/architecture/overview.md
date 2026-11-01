# 架构导航

更新：2026-10-29。状态：目标架构，查询和离线内核已实现，部署运维与诊断评测待实现。为避免两份整体设计分叉，完整架构图与选型理由统一维护在[日志诊断 Agent 整体设计](../../03-日志诊断Agent详细设计.md)。

先用本页定位模块；仅讨论整体方案时读主设计 §1–4、§10，修改协议时再读下表对应专题。各类任务的章节入口和停止条件见[渐进式阅读指南](../reading-guide.md)。

## 当前决定

- 面向已完成的 goframeshop，复用 Prometheus／Loki／Jaeger；不重建商城、不自研 KV。
- Agent 使用现有 Go／net/http、手工构造注入；单 Investigator＋确定性 Harness。
- 默认固定一个真实模型，Fake Provider 用于工程测试；不宣称全模型兼容。
- 增加精确告警归并、确定性日志聚合、来源质量与证据回查；不引入第二套采集平台。
- 独立 Redis 保存有界状态、证据和事件；单活动 Worker，单 Run 顺序调用，最多两个并行 Run。
- React／SSE 展示持久结果；人工在 Agent 外处置，通过新 Run 验证恢复，保留原报告。

## 模块与维护位置

| 问题 | 权威文档 |
| --- | --- |
| 为什么做、整体组件、架构图与取舍 | [整体设计](../../03-日志诊断Agent详细设计.md) |
| 接口、状态、预算、租约、原子提交、SSE | [调查执行协议](investigation-design.md) |
| 已有服务、字段、指标、告警和来源质量 | [遥测契约](../telemetry/data-contract.md)、[源码基线](../integration/goframeshop-baseline.md) |
| 对照、真值、恢复和业务收益验证 | [评测方案](../evaluation/plan.md) |
| 当前实施与验收 | [生产工程基线 0007](../../tasks/0007-production-baseline.md)、[后续路线](../development/production-roadmap.md)； [Redis 恢复任务 0006](../../tasks/0006-redis-recovery.md)； [模型调用任务 0005](../../tasks/0005-model-boundary.md)； [日志聚合任务 0004](../../tasks/0004-log-patterns.md)、[离线任务 0003](../../tasks/0003-offline-investigation.md)；[接入任务 0002](../../tasks/0002-goframeshop-integration.md)的故障样本待验收 |
| 简历目标与证据映射 | `04-IncidentLens简历目标稿.md`（本地文件，不入库） |

当前保留健康检查 API，已增加 internal/telemetry、outbound/telemetryhttp、outbound/fixture、app/capture 与 cmd/incident-query，提供有界查询和冻结样本回放，见[使用指南](../integration/query-adapters.md)。现已增加 app/investigation、outbound/scripted 与 cmd/incident-investigate，提供[离线 Harness](../development/offline-investigation.md)；任务 0005 增加模型 HTTP Adapter、有界输入与内存预算；任务 0006 增加 Redis 原子状态脚本与隔离测试；已接入 Harness checkpoint 与可恢复 Worker；真实供应商、在线持久调查和工作台仍待验收/实现。代码、测试与实际依赖决定实现状态，设计和简历目标不作为已完成证明。
