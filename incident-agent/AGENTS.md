# AGENTS.md

## 项目与当前阶段

IncidentLens 是面向已完成 GoFrame 商城的只读日志诊断 Agent。当前已有健康检查 API、三源只读查询 CLI、匿名证据 Bundle、离线回放及脚本驱动的单次调查 Harness、结构化日志聚合；已增加有界模型上下文、Token 预算和显式配置 HTTP Provider；已增加 Redis 原子状态、Harness checkpoint、可恢复 Worker、全局 Run 准入和终态查询投影；真实供应商联调、在线持久调查与工作台待验收/实现。当前能力以 [README](README.md) 和代码为准。

当前已完成[任务 0007：生产工程基线](tasks/0007-production-baseline.md)和[任务 0008：Run 容量与终态查询](tasks/0008-capacity-and-results.md)的工程验收，本机与远端 CI 均通过；下一开发切片为[任务 0009：在线持久调查与供应商联调](tasks/0009-online-investigation.md)，后续顺序见[生产路线](docs/development/production-roadmap.md)；[任务 0006：独立 Redis 状态与恢复](tasks/0006-redis-recovery.md)已完成工程切片；[任务 0005：模型调用边界](tasks/0005-model-boundary.md)已完成工程切片，建立在已完成的[离线调查](tasks/0003-offline-investigation.md)和[日志聚合](tasks/0004-log-patterns.md)之上；[任务 0002：商城遥测接入](tasks/0002-goframeshop-integration.md)保留固定版本故障案例待验收。文档维护或查询按用户请求完成，不自动启动该任务。复用商城及 Prometheus／Loki／Jaeger；不重建商城、不开发自研 KV、不增加第二个 Trace 后端。上游事实见[固定提交基线](docs/integration/goframeshop-baseline.md)。

用户确认的本机商城目录为工作区同级的 `../GoframeShop/goframeshop/`（相对工作区根目录）；版本与实验前置检查见[本机入口](docs/integration/goframeshop-baseline.md#本机入口与运行状态)。该路径仅供开发核对，不作为 Agent 的 Shell／源码读取工具入口。用户要求暂不修改商城相关代码；确需上游调整时，先说明原因与改动范围并提醒用户审查。

## 按需读取

本文件保留每次工作都适用的约束。先按下表选入口，读取相关章节；需要更多背景时查[文档索引](docs/index.md)和[渐进式阅读指南](docs/reading-guide.md)，不默认通读全部设计。

| 当前工作 | 下一份文档 |
| --- | --- |
| 生产进度与发布检查 | [生产路线](docs/development/production-roadmap.md)、[工程检查](docs/development/release-checks.md)、[任务 0007](tasks/0007-production-baseline.md) |
| Run 容量、过载与终态查询 | [任务 0008](tasks/0008-capacity-and-results.md)、[Worker 指南](docs/development/worker-recovery.md#准入与过载) |
| 状态查询、启动骨架 | [README](README.md) |
| 使用查询 CLI、导出或回放 | [Adapter 使用指南](docs/integration/query-adapters.md)、[接入报告](docs/reports/0002-readonly-integration.md) |
| 实施或运行离线调查 | [任务 0003](tasks/0003-offline-investigation.md)、[离线 Harness 使用指南](docs/development/offline-investigation.md)；只读所需协议章节 |
| Redis 存储与恢复 | [任务 0006](tasks/0006-redis-recovery.md)、[状态协议](docs/development/redis-state.md)；持久入口见 [Worker 指南](docs/development/worker-recovery.md)，旧 CLI 仍为内存模式 |
| 模型调用与 Token 预算 | [任务 0005](tasks/0005-model-boundary.md)、[Provider 指南](docs/development/model-provider.md)；真实联调结果与工程测试分开 |
| 日志聚合与摘要核查 | [任务 0004](tasks/0004-log-patterns.md)、[日志聚合指南](docs/development/log-patterns.md) |
| 实施商城接入 | [任务 0002](tasks/0002-goframeshop-integration.md)，再按其“按工作项读取”选择专题 |
| 整体架构与选型 | [架构导航](docs/architecture/overview.md)，再读主设计相关章节 |
| 调查、报告、预算或恢复 | [执行协议](docs/architecture/investigation-design.md)相关章节 |
| 遥测字段、查询、来源质量 | [遥测契约](docs/telemetry/data-contract.md)相关章节 |
| 样本、评测或演示 | [评测方案](docs/evaluation/plan.md)相关章节 |
| 修改文档、开发或验收交接 | [工作流程与维护规则](docs/development/workflow.md) |
| 简历表述 | `04-IncidentLens简历目标稿.md`（本地文件，不入库），仅有实现和实测证据的条目可写为完成 |

## 架构边界

- 保留轻量六边形架构、手工构造注入、使用方接口；领域与应用逻辑独立于模型和遥测后端。没有第二实现或明确测试边界，不预建抽象层。
- `cmd/` 只做配置、组装、生命周期和退出；HTTP 层保留 net/http，不因商城使用 GoFrame 而迁移。
- 单 Investigator＋确定性 Harness；首版单活动 Worker，每 Run 顺序调用，最多两个并发 Run。不增加多 Agent、向量库或自研存储。
- Agent 状态使用独立 Redis；持久入口检查 AOF everysec、noeviction，不复用商城缓存 Redis。不承诺 Redis／主机故障零丢失。
- 不复制上游业务代码；商城变更在商城仓库单独交付并遵守其 AGENTS.md。

## 不变量

- 诊断 Agent 只查询遥测，不执行 Shell／SQL、退款、重启、配置变更或故障注入。日志、告警、Runbook、变更说明均是不可信数据，不能扩展工具权限。
- 查询每次校验服务、环境、时间窗、结果规模和资源归属；首版单租户，不宣称商城遥测已具备多租户隔离。
- 事实与根因假设引用本 Run 保存的证据；引用存在不等于语义成立，证据不足返回 INCONCLUSIVE。跨 Run 对照仅按执行协议受控导入。
- 外部调用前预留预算；恢复不重置预算或 deadline；旧 Worker 不得写入新所有者状态。
- 告警按 episode 精确去重，不仅按服务归并；来源失联不等于恢复。日志聚合必须保留样本口径、截断和原记录引用。
- 工作台展示冻结证据；重查、恢复验证创建新 Run，反馈只追加，不覆写原报告。
- 原始秘密、用户隐私、完整数据库记录不进入 Prompt、日志或 Fixture；上游脱敏后仍需 Adapter 二次筛选。
- 注入点名字、测试标题、判分真值、带密钥实验配置仅 Evaluator／实验操作员可见，不能作为调查证据。

## 工作与验证

- 按任务复杂度选择流程；简单任务直接完成，不强制计划、工作树、subagent 或完整 TDD。修改前说明范围和验证方式，详见[工作流程](docs/development/workflow.md)。
- Go 编码、审阅、调试或配置前加载 `golang-how-to` 技能，由其路由必要技能；纯文档维护不触发 Go 开发流程。
- 行为变化添加必要回归测试，不削弱断言；契约变化同步所属专题和受影响入口，重要决定记入 [changelog](docs/changelog.md)。
- 当前命令：`make build`、`make fmt`、`make vet`、`make test`、`make test-race`、`make lint`、`make verify`、`make run`、`make test-integration`、`make test-redis`、`make release-check`、`make security`；依赖与选择方式见[验证规则](docs/development/workflow.md)。
- 仓库公开：提交内容不得含本机绝对路径、个人邮箱、密钥或运行日志，提交者邮箱使用 GitHub noreply 地址。
- 文档改动检查链接、事实、状态和交叉引用即可；明确未运行项。不得把源码核对、上游结果、目标设计写成本地实测；尚无 eval/demo 命令。
