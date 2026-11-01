# 渐进式阅读指南

更新：2026-10-29。本文说明开发者和编码助手如何按任务加载项目文档；运行时 Investigator 的上下文装配由[调查协议 §4](architecture/investigation-design.md)维护。本文不新增模型能力或改变实现范围。

## 1. 四层阅读与停止条件

| 层级 | 加载内容 | 什么时候读 | 什么时候停止 |
| --- | --- | --- | --- |
| L0：固定约束 | [AGENTS.md](../AGENTS.md) | 每次项目工作开始时 | 已知项目边界和下一入口，不继续加载无关专题 |
| L1：任务定位 | [索引](index.md)或当前任务；状态问题读 README | 明确本次目标、范围和验收方式 | 能确定要改什么及需要哪个契约 |
| L2：专题契约 | 下表指定章节 | 实际改动会影响该协议或结论时 | 已取得输入、输出、不变量、验收依据 |
| L3：证据与实现 | 本地代码/测试、固定 SHA 源码、真实响应、版本化样本和报告 | 核实事实、实现行为、发布成绩或解决冲突时 | 证据足以支持本次结论；缺失项明确标记 |

不默认递归打开正文里的所有链接。专题引用用于追溯，只有当前问题依赖其内容时才继续读取。说明整体选型时无需先读每个字段；实现 Adapter 时则必须读对应字段和权限约束。标题可先用 `rg -n '^#{1,3} ' <文档路径>` 定位，再读取所需范围。

## 2. 按任务读取章节

下列 § 编号对应链接文档中的章节标题。跨模块修改取相关行的并集；已读内容不重复加载。

| 任务 | 最小阅读集 | 触发进一步阅读的条件 |
| --- | --- | --- |
| 生产开发进度、工具链与发布 | [生产路线](development/production-roadmap.md)、[发布检查](development/release-checks.md)、[任务 0007](../tasks/0007-production-baseline.md) | 容量/终态读[任务 0008](../tasks/0008-capacity-and-results.md)和 Worker 指南“准入与过载”；在线模式与真实联调读[任务 0009](../tasks/0009-online-investigation.md)和 Worker 指南“在线遥测模式”，再加载供应商/遥测文档 |
| HTTP 入口、认证与就绪状态 | [任务 0010](../tasks/0010-http-entry.md)、[API 指南](development/http-api.md) | 调整接口读调查 §2；告警入口再读下方告警行 |
| 运行或解释现有骨架 | [README](../README.md)“当前状态／运行现有骨架” | 改命令或配置：核对 Makefile、go.mod 和入口代码 |
| 在线查询、匿名导出、离线回放 | [查询使用指南](integration/query-adapters.md) §1–4 | 验证现有样本读[接入报告](reports/0002-readonly-integration.md)；修改查询语义再读遥测专题 |
| 评审整体方案 | [架构导航](architecture/overview.md)；[主设计](../03-日志诊断Agent详细设计.md) §1–4、§10 | 讨论恢复、预算、权限：再读调查协议对应章节 |
| 实施当前接入切片 | [任务 0002](../tasks/0002-goframeshop-integration.md)“目标／范围／验收” | 具体后端按下面三行展开；本切片不自动扩展到 Harness |
| Prometheus Adapter | [遥测](telemetry/data-contract.md) §1、§3、§6–7；[基线](integration/goframeshop-baseline.md) §2、§4 | 核验告警输入再读遥测 §5、§8；实现自动入口需另有任务范围 |
| Loki Adapter | [遥测](telemetry/data-contract.md) §1–2、§6–7；[基线](integration/goframeshop-baseline.md) §4 | 实现日志聚合时再读调查协议 §10.2；当前任务仅固定字段契约 |
| Jaeger Adapter | [遥测](telemetry/data-contract.md) §1、§4、§6–7；[基线](integration/goframeshop-baseline.md) §2、§4 | 跨 Trace 关联不清楚时核对固定提交的传播/消息代码和真实响应 |
| 离线调查 CLI 与脚本 | [离线 Harness 指南](development/offline-investigation.md) §1–4、[任务 0003](../tasks/0003-offline-investigation.md) | 修改权限、预算或报告再读调查协议 §2–6；不默认加载 Redis |
| 模型调用与上下文预算 | [任务 0005](../tasks/0005-model-boundary.md)、[Provider 指南](development/model-provider.md) | 调整调用/usage 规则读调查 §3–4；实现恢复时再读 §7 |
| 日志聚合 | [任务 0004](../tasks/0004-log-patterns.md)、[日志聚合指南](development/log-patterns.md) | 调整统计口径时读调查 §10.2；接模型时再读预算与上下文协议 |
| 工具、Harness 或上下文 | [调查](architecture/investigation-design.md) §2–6；[评测](evaluation/plan.md) §1、§6 | 涉及日志摘要再读调查 §10.2；涉及持久提交再读 §7 |
| Redis、Scheduler、取消与接管 | [任务 0006](../tasks/0006-redis-recovery.md)、[Worker 指南](development/worker-recovery.md)、[已实现状态协议](development/redis-state.md)； [调查](architecture/investigation-design.md) §2–4、§7；[评测](evaluation/plan.md) §6、§8 | 讨论故障保证或替换存储，再读主设计 §4、§8 |
| 告警入口与去重 | [遥测](telemetry/data-contract.md) §5、§8；[调查](architecture/investigation-design.md) §10.1 及 §7 的 CreateRun/容量规则；[评测](evaluation/plan.md) §8 | 需要真实 firing 演示时读评测 §4 |
| 证据报告、SSE、人工反馈与恢复验证 | [调查](architecture/investigation-design.md) §2、§6、§8、§10.3；[评测](evaluation/plan.md) §8 | 修改来源质量时读遥测 §6–7；新增执行或预算行为再读调查 §3–4、§7 |
| Fixture 采集与脱敏 | [评测](evaluation/plan.md) §2–4；[遥测](telemetry/data-contract.md)对应数据源章节及 §6–7 | 更新上游时按基线 §5 核对版本；真值始终隔离 |
| 诊断对照、消融、发布成绩 | [评测](evaluation/plan.md) §1、§5–8 | 根因粒度争议回到 §2 与冻结证据；简历表述再读本地目标稿 §3 |
| 文档维护与任务交接 | [维护规则](development/workflow.md)；本次受影响专题 | 改整体范围再读主设计与 ADR；不因改导航重新实测全部后端 |

## 3. 当前任务的最短路径

例如接入 Loki：先读 AGENTS → 任务 0002 的范围与验收 → 遥测 §1–2、§6–7 → 按需核对基线引用的日志导出源码和真实 Loki 响应。涉及冻结样本时补读评测 §3，防止实验真值进入调查数据。此时不需要加载 Redis 租约与 SSE 细节。

例如只修改 README 的命令：读取 AGENTS、README 和 Makefile，按维护规则检查命令存在、依赖与状态描述，验证链接即可；不顺带实施任务 0002。

例如实现 Worker 恢复：先确认该工作已纳入任务范围，再读调查 §2–4、§7 与恢复验收。仅凭架构图不足以实现预算守恒、结果未知和旧 Worker 拒写。

## 4. 发现缺口或冲突

1. 先区分“已实现事实”和“目标行为”；用[索引的状态口径](index.md)标注，不能把实现缺口改写成能力已交付。
2. 当前任务决定本次实施范围，专题决定该范围内的协议；任务遗漏相关不变量时补任务，不默默绕过协议。
3. 上游事实有疑问时回到基线 SHA 和对应源码；源码无法证明运行结果时，保留待验收，不猜测线上响应。
4. 新的用户决定可以更新范围；同步所属文档和受影响入口。只有当前资料无法解决且影响实施的歧义才提问，其余工作继续。

读到足以完成当前任务时停止扩展上下文。交接记录本次采用的版本、相关章节、已验证项和未解决问题，不复制整份设计。
