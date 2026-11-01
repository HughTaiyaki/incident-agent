# IncidentLens · 日志诊断 Agent

面向已完成的 [GoFrame 商城](https://github.com/HughTaiyaki/goframeshop)，通过受控查询关联指标、Trace 和日志，输出带证据的故障假设、未知项与下一步建议。目标闭环是：告警/人工发起 → 调查 → 人工核查 → 外部处置 → 恢复验证与反馈。

## 当前状态

2026-11-01 更新。商城设计代码基线为 `385d3649b4f38f1db82e65aade57b00e10badb02`；本次历史遥测的运行版本未证实，快照标为 `unknown`。

- **外部商城已有代码：**交易闭环、库存预留、支付关单、Outbox/Inbox、恢复任务、OTel → Prometheus/Loki/Jaeger、交易故障实验。详见[代码基线与接入差距](docs/integration/goframeshop-baseline.md)。本次是源码核对，不是重新运行商城验收。
- **本仓库已实现：**Go／net/http 健康检查 API，以及 Prometheus/Loki/Jaeger 只读 Adapter、固定查询模板、范围/预算限制、结构字段筛选、匿名证据 Bundle、离线回放和 `incident-query` CLI，以及脚本驱动的内存调查 Harness、Report Guard 与 `incident-investigate` 离线 CLI，以及随日志查询生成的结构摘要。另有 `incident-model`：显式 HTTP Provider、有界上下文、Token 预留与调用审计；供应商联调仍待验证。`readyz` 仍仅返回固定状态，不检查外部依赖。
- **本地已验证：**三源历史窗口查询和回放，冻结 169 条结构记录；fmt、vet、race、lint、构建与显式在线集成测试通过。详见[接入与人工核查报告](docs/reports/0002-readonly-integration.md)。这不是固定版本故障集或模型诊断成绩。
- **离线内核已验证：**[任务 0003](tasks/0003-offline-investigation.md)的执行闭环；支持范围与次数限制、无进展停止、取消、证据引用校验及确定性收尾。脚本决策不代表真实模型诊断。
- **恢复能力：**[任务 0006](tasks/0006-redis-recovery.md)已接入独立 Redis、Harness checkpoint 与可恢复 Worker；运行 `make test-redis`，见[协议与限制](docs/development/redis-state.md)。持久入口为 `incident-worker`，见[运行与恢复指南](docs/development/worker-recovery.md)；旧 CLI 仍为内存模式。
- **工程基线已验证：**[任务 0007](tasks/0007-production-baseline.md)已升级至 Go 1.26.10，完整 `make release-check` 通过，普通与 Redis 标签漏洞扫描均无可达漏洞报告；远端 CI 尚未运行。
- **容量与终态已验证：**[任务 0008](tasks/0008-capacity-and-results.md)增加全局 Run 准入、内存预留和版本化结果投影，本机 `make release-check` 通过；尚未压测，不提供生产容量推荐值。下一阶段为[任务 0009：在线持久调查与供应商联调](tasks/0009-online-investigation.md)，后续按[生产路线](docs/development/production-roadmap.md)推进 API 和运维。
- **待完成：**任务 0002 的固定版本故障样本与人工验收，以及真实供应商兼容性/效果、Incident 去重、在线持久调查验收、容量压测、效果评测与 React/SSE 工作台。商城代码暂不修改，必要调整先提醒用户审查。

不再建设另一套小型商城；不开发自研 KV；Trace 后端统一复用商城 Jaeger，不部署 Tempo。Agent 的持久 Redis 必须独立于商城的无持久化缓存 Redis。

## 持久调查与恢复

新增 `incident-worker` 提供 enqueue/work/status/evidence/cancel，支持启动扫描、租约续期、已保存决策和证据复用。enqueue 需显式设置 `INCIDENT_MAX_ACTIVE_RUNS`，Redis 需配置 maxmemory。只连接显式配置的独立 Redis；当前执行模型＋Fixture，详情见[Worker 指南](docs/development/worker-recovery.md)和[改动报告](docs/reports/0006-worker-recovery.md)。当前正在按[生产路线](docs/development/production-roadmap.md)完善工程与发布门槛，暂不宣称生产就绪。

发布前执行 `make release-check`；版本、校验数据库设置、CI 和未验收项见[工程发布检查](docs/development/release-checks.md)。

## 运行现有骨架

在本目录运行，需要 Go 1.26.10 或兼容的受支持工具链：

```bash
make test
make run
```

另一个终端验证：

```bash
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
```

API 支持环境变量 `PORT`（默认 `8080`）。商城启动说明在上游仓库；本仓库尚无 Agent Compose；模型调查命令需显式配置，见下文。

现有验证入口：`make build`、`make fmt`、`make vet`、`make test`、`make test-race`、`make lint`、`make verify`、`make test-integration`。`lint/verify` 使用 Makefile 中固定版本的 golangci-lint，初次运行需联网下载；在线集成测试要求运行中的三源后端和明确的查询输入。`eval-offline`、`eval-online`、`demo` 尚未实现。

## 运行只读查询与回放

无需后端即可回放仓库中的真实历史样本：

```bash
make build
mkdir -p data
./bin/incident-query -mode replay \
  -fixture testdata/telemetry/observed-20261028/bundle.json \
  -input testdata/telemetry/observed-20261028/queries.json \
  -out data/replay.json
```

已有输出文件不会被覆盖，重复执行需换文件名。在线地址配置、查询 JSON、权限边界和测试方法见[查询 Adapter 使用指南](docs/integration/query-adapters.md)。日志自由文本目前全部省略；回放匿名 ID 不能用于真实后端重查。

## 运行离线调查闭环

```bash
./bin/incident-investigate \
  -fixture testdata/telemetry/observed-20261028/bundle.json \
  -script testdata/investigation/observed-script.json \
  -out data/investigation.json
```

先执行上面的 `make build` 和 `mkdir -p data`。命令不联网；现有示例执行 7 次脚本决策、读取 169 条记录，保存带引用的 INCONCLUSIVE 报告与全部查询证据。输出不覆盖。输入、停止条件及实现限制见[离线 Harness 指南](docs/development/offline-investigation.md)。

日志查询现在自动生成 `run.log_patterns`，保留样本数量、代表记录引用和来源缺口；现有示例产生 1 组摘要、3 条代表引用。规则与限制见[日志聚合指南](docs/development/log-patterns.md)，本轮变化见[任务 0004 改动说明](docs/reports/0004-log-patterns.md)。

## 受控模型调用

新增 `incident-model` 通过显式配置的模型端点驱动 Fixture 调查。它会外发选中的请求和证据，与完全离线的 `incident-investigate` 分开；没有默认供应商或自动密钥发现。配置、预算、审计与生产门槛见[Provider 使用指南](docs/development/model-provider.md)，文件级变化见[任务 0005 改动报告](docs/reports/0005-model-boundary.md)。当前仅完成本地模拟 HTTP 验证，尚未调用真实供应商；此命令仍为内存模式，Redis 恢复通过 `incident-worker` 提供。

## 设计与实施入口

先读 [AGENTS.md](AGENTS.md)，再按任务选择专题；只查运行方式时读本页即可。文档按“固定约束 → 任务定位 → 专题契约 → 实现与证据”逐层展开，不要求每次通读完整设计。

- [文档索引](docs/index.md)
- [渐进式阅读指南：各类任务的最小阅读集](docs/reading-guide.md)
- [工作流程与文档维护规则](docs/development/workflow.md)
- [整体架构、架构图与选型理由](03-日志诊断Agent详细设计.md)
- [简历目标稿（尚未实现，不能作为完成经历）](04-IncidentLens简历目标稿.md)
- [架构导航](docs/architecture/overview.md)
- [商城代码基线与接入差距](docs/integration/goframeshop-baseline.md)
- [遥测数据契约](docs/telemetry/data-contract.md)
- [受控调查、证据与恢复设计](docs/architecture/investigation-design.md)
- [评测与交付方案](docs/evaluation/plan.md)
- [当前 Agent 切片：在线持久调查与供应商联调](tasks/0009-online-investigation.md)
- [待验收：接入现有商城遥测](tasks/0002-goframeshop-integration.md)

修改前读取 [AGENTS.md](AGENTS.md)。文档必须区分代码事实、上游记录的实验结果、本地实测与尚未实现的设计；维护记录见 [changelog](docs/changelog.md)。
