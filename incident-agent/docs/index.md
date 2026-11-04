# 文档索引

更新：2026-11-02。当前方向是复用已完成的 GoFrame 商城，构建只读诊断 Agent；三源查询 CLI 与回放已实现，离线 Harness 已实现，模型 Provider/预算已实现、DeepSeek 已完成在线联调；[任务 0007](../tasks/0007-production-baseline.md)和[任务 0008](../tasks/0008-capacity-and-results.md)工程验收已完成（本机与远端 CI 通过），进行中的切片为[任务 0009](../tasks/0009-online-investigation.md)（已完成，诊断效果归 P5）、[任务 0010](../tasks/0010-http-entry.md)（工程部分已验证）和[任务 0011](../tasks/0011-deployment-operations.md)（部署环境待验证），日志结构聚合已接入离线 Harness，任务 0002 的故障案例验收仍未完成。

## 从这里选择入口

已读 [AGENTS.md](../AGENTS.md) 后，只选与任务匹配的一行。需要具体章节和阅读停止条件时查看[渐进式阅读指南](reading-guide.md)。

| 要解决的问题 | 首选入口 | 何时继续深入 |
| --- | --- | --- |
| 下一步怎样提升诊断效果 | [任务 0013：可信对照与证据计算](../tasks/0013-evidence-and-baseline.md)，实验规则见[第 1 阶段](evaluation/0013-preregistration.md)、[第 2 阶段](evaluation/0013-stage2-preregistration.md)、[第 3 阶段](evaluation/0013-stage3-preregistration.md)、[第 4 阶段](evaluation/0013-stage4-preregistration.md)，结果见[固定流程对照](reports/0013-fixed-flow-comparison.md)、[调用关系对照](reports/0013-call-relations-comparison.md)、[三组对照](reports/0013-hybrid-flow-comparison.md) | 第 4 阶段未通过，[结果](reports/0013-new-scenario-acceptance.md)已经用户抽检 |
| 诊断效果如何 | [固定故障集评测结果](reports/0012-fixed-fault-evaluation.md) | 方案见[任务 0012](../tasks/0012-fixed-fault-evaluation.md)，标准见[评测方案](evaluation/plan.md) |
| 本轮开发改了什么 | [任务 0011 部署与运维](reports/0011-deployment-operations.md) | DeepSeek 联调见[任务 0009 报告](reports/0009-online-investigation.md#deepseek-联调)，API 见[任务 0010 报告](reports/0010-http-entry.md) |
| 如何部署和值班 | [部署指南](operations/deployment.md)、[值班手册](operations/runbook.md) | 取舍和验收状态见[任务 0011](../tasks/0011-deployment-operations.md) |
| 如何调用调查 API | [API 指南](development/http-api.md) | 目标接口全集见调查协议 §2 |
| 当前能运行什么 | [README](../README.md) | 要修改实现时，再定位代码和当前任务 |
| 如何查询遥测、导出和回放 | [查询 Adapter 使用指南](integration/query-adapters.md) | 实测范围与缺口见[接入报告](reports/0002-readonly-integration.md) |
| 下一阶段做什么 | [任务 0014：先帮人排查](../tasks/0014-assist-and-evidence.md)，[人工辅助对照方案](evaluation/human-assist-comparison.md) | 工程部分已完成，人工对照等确认 |
| 如何适配系统、选择调查流程 | [系统配置档](development/system-profile.md) | 当前只有商城的配置档 |
| 如何配置模型与核查预算 | [Provider 指南](development/model-provider.md) | [任务 0005](../tasks/0005-model-boundary.md)明确当前生产化边界 |
| 日志怎样分组与核查 | [日志聚合指南](development/log-patterns.md) | 范围见[任务 0004](../tasks/0004-log-patterns.md)，源码与测试见改动报告 |
| 运行或修改调查内核 | [离线 Harness 指南](development/offline-investigation.md) | 开发范围见[任务 0003](../tasks/0003-offline-investigation.md)，真实供应商验收与持久化另行推进 |
| 补齐商城接入验收 | [任务 0002](../tasks/0002-goframeshop-integration.md) | 按工作项读基线、遥测、证据或 Fixture 章节 |
| 为什么这样设计 | [架构导航](architecture/overview.md) | 需要架构图、取舍时读 [03 主设计](../03-日志诊断Agent详细设计.md) |
| 商城已有什么、接入差距是什么 | [固定提交基线](integration/goframeshop-baseline.md) | 新增依赖或升级基线时核对固定提交源码 |
| 服务名、指标、日志、Trace 怎么解释 | [遥测契约](telemetry/data-contract.md) | 要实现/验证查询时检查真实后端响应 |
| 工具、预算、状态、恢复怎样执行 | [调查协议](architecture/investigation-design.md) | 按协议定位实现和回归测试；离线子集见使用指南，持久执行仍待实现 |
| 如何制作样本、判分和演示 | [评测方案](evaluation/plan.md) | 要发布成绩时查看逐次结果与原始证据 |
| 如何改动、验证和同步文档 | [工作流程与维护规则](development/workflow.md) | 按变更类型查同步清单 |
| 如何写简历与准备证据 | `04-IncidentLens简历目标稿.md`（本地文件，不入库） | 只有实现与实测可支持完成表述 |
| 最近为什么改变方向 | [changelog](changelog.md)、[ADR-0002](adr/0002-reuse-goframeshop.md) | 需要历史追溯时读被替代记录 |

当前恢复开发入口：[任务 0006](../tasks/0006-redis-recovery.md) → [Redis 协议](development/redis-state.md) → [本轮改动报告](reports/0006-redis-protocol.md)。只核查存储时读这条路径，无需通读模型或商城文档。

Worker 使用/恢复：[使用指南](development/worker-recovery.md)；验证与文件清单：[Worker 报告](reports/0006-worker-recovery.md)。

生产开发入口：[路线与门槛](development/production-roadmap.md) → [已完成任务 0007](../tasks/0007-production-baseline.md)、[0008](../tasks/0008-capacity-and-results.md) → [发布检查](development/release-checks.md)；进行中见[任务 0009](../tasks/0009-online-investigation.md)、[0010](../tasks/0010-http-entry.md)。

## 内容归属与状态

代码决定已实现行为，README 汇总可运行状态；主设计维护产品范围和选型理由；专题分别维护遥测、执行、评测契约；任务限定当前切片。导航只链接这些内容，不另外复制协议和默认值。

所有能力与成绩按以下口径标注：

- **源码已核对：**在指定版本找到实现，不等于本地启动和验收通过。
- **上游记录通过：**引用商城作者的实验记录，不记作本仓库实测。
- **本地已验证：**保留命令、版本、环境、结果或报告。
- **目标设计／待实现：**后续交付要求，不计入已实现功能。

发现冲突时先记录“代码事实、目标契约、当前任务”的差异，再更新对应内容；不能用未来设计覆盖实际状态。文档维护与同步要求集中在[维护规则](development/workflow.md)，阅读层级集中在[阅读指南](reading-guide.md)。

## 历史追溯

- [ADR-0001](adr/0001-observable-lab-before-agent.md)：已由 ADR-0002 替代。
- [任务 0001](../tasks/0001-observable-checkout.md)：已废弃，不再实施。

2026-09-07 的外部技术选型稿已整合进当前设计。旧稿的 Tempo、小型实验商城、自研 KV，以及未经代码核验的指标和场景，不作为当前事实或实施入口。
