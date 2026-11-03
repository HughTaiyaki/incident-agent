# 文档维护记录

## 2026-11-03 · 留出批次

- [留出结果](reports/0012-fixed-fault-evaluation.md#留出批次没用来调优的场景和真-bug)：用没调优过的场景和真 bug 测试，Top-1 8/21，正常行为误判 0/9。有明显错误信号的问题能定位，静默 bug 查不出来。下一步按缺口补证据，再用新样本复测。

## 2026-11-03 · 模型对比

- 用户想换 `deepseek-v4-pro`。关闭思考时，它在开发集上没有优于 `flash`，默认模型不换。开启思考的尝试 10/11 次因模型调用失败中断，原因未查明，没有结论；余额不足，已停止。详见[评测报告](reports/0012-fixed-fault-evaluation.md#模型对比开发集)。

## 2026-11-02 · 固定故障集第五轮评测

- [第五轮结果](reports/0012-fixed-fault-evaluation.md#第五轮复测)：Top-1 10/18（D1 8/9，D3 2/9），N1 误判 3/9，未达门槛。D3 的报告都写对了事实，但多数不下结论。

## 2026-11-02 · 程序保证日志覆盖（harness-v8）

- 第四轮 D3 退步的原因是 8/9 没查 goods-service。决定：调查开始时由程序查一遍所有服务不带过滤的日志（`log_sweep`），不花模型调用，HTTP 入口和评测默认开启。规则对所有运行相同，不泄露答案。
- 时间线的时间改为取自被引用的记录，不信模型写的值；以前差几毫秒就会拒绝整份报告。引用不存在的记录仍被拒绝。
- 查询被拒时给出固定的改正说明。

## 2026-11-02 · 固定故障集第四轮评测

- [第四轮结果](reports/0012-fixed-fault-evaluation.md#第四轮复测)：Top-1 6/18（D1 5/9 达到单项门槛，D3 1/9），N1 误判 6/9，未达门槛。8/9 的 N1 能认出重复键，但多数仍说成故障。全部发出内容未检出已知形态的标识符。

## 2026-11-02 · 放开脱敏后的错误文本（harness-v7）

- 用户决定：WARN/ERROR 和带事件名日志的正文、出错 Span 的错误描述，脱敏后可发给模型。这放宽了“不发日志正文”的边界，因为这些内容是区分故障和正常行为的关键证据。普通 INFO/DEBUG 正文、SQL、主机和 IP 标签继续不发。
- 试采核查：64 次调用约 33 万字节中，没有检出订单号、UUID、邮箱、IP、手机号、长数字或 SQL。这只覆盖实验数据和已知形态，接生产数据前要重新评估。

## 2026-11-02 · 固定故障集第三轮评测

- [第三轮结果](reports/0012-fixed-fault-evaluation.md#第三轮复测)：Top-1 3/18，N1 误判 7/9，未达门槛。D1 组件全对，但错误 Span 到达模型后，正常重复提交被误判成故障。
- 根因：商城已上报错误描述和日志正文（如 `reserve outcome unknown`、`already processed`、`Error 1062`），是 Agent 适配层没有保留。不需要改商城，但要扩大外发范围，待用户决定。

## 2026-11-02 · 让错误证据优先（harness-v6）

- 第二轮分析发现错误 Span 从未到达模型：Trace 搜索结果不显示错误，打开后也不优先。现在 Trace 摘要带 `error_spans`，失败记录优先发送。
- 查询被拒的原因是模型漏掉 `environment`，现在环境和时间窗默认取请求范围，不能放宽。

## 2026-11-02 · 固定故障集第二轮评测

- [第二轮结果](reports/0012-fixed-fault-evaluation.md#第二轮复测)：新样本上 Top-1 3/18，未达门槛，不放量。无依据断言率、N1、N2 达标。单次 Token 涨到约 29,000。
- 主要剩余问题：错误 Span 从未被引用，找到信号却不敢下结论，查询常被拒绝。

## 2026-11-02 · 调查策略修复（harness-v5）

- 针对[首轮失败原因](reports/0012-fixed-fault-evaluation.md#失败分析)：查询清单显示过滤条件，列出还没查过的服务，提示先看不带过滤的日志和下游服务，首次查看某服务和类型的空结果不算没有进展，没有值的指标点只计数。
- 决定：规则按阶段发送，新增 `{"kind":"synthesize"}` 让模型主动结束调查；总额度从 24,000 升到 36,000（调查 27,000，报告预留 9,000），因为实测每次调查只要约 ¥0.02，而额度不够查到 Trace。Lua 存储上限同步。
- 在首轮样本（开发集）上调参后锁定，用新样本复测，首轮样本不计入门槛。

## 2026-11-02 · 固定故障集首轮评测

- [首轮结果](reports/0012-fixed-fault-evaluation.md)：Top-1 0/18，无依据断言率 11.5%，未达门槛，不放量。N1、N2 达标，但 Agent 全部拒答，这不能说明它会区分。
- 主要原因：模型总先查 ERROR 日志，查询清单不显示过滤条件，几乎不查下游服务和 Trace，连续空结果过早停止。证据本身足以支撑 D1、D3。
- 决定：单场景实验只在临时 clone 中让服务多活 90 秒，否则缓冲中的遥测会丢失；首轮 9 份不完整的样本已作废。用户把可诊断性判定和判分委托给 Claude，属于非盲判定，已标明。

## 2026-11-02 · P5 评测准备

- 用户确认[任务 0012](../tasks/0012-fixed-fault-evaluation.md)的决定：评测商城 `2f25dc5`（工作区只有文档改动），用户判真值，授权运行实验，同意外发冻结样本，门槛按方案。
- 把日志过滤参数写入提示词，查询里的假名由程序还原；回放包条目上限升到 128；新增 `incident-eval`。试运行中模型首次给出 DIAGNOSED，准确性未判定。

## 2026-11-02 · 调查质量修复

- 修复重复查询、空报告和上下文不足，同窗口带引用的事实从 0–3 条升到 5–6 条，详见[改动报告](reports/0009-online-investigation.md#调查质量修复)。结果仍是 INCONCLUSIVE，诊断效果未评测。
- 决定：已成功的相同查询由 Harness 直接拒绝；报告最多修正两次；模型漏掉外层 `kind` 时只补外层，内容校验不放宽。
- 决定：报告预留升到 9,000，估算改为字节数的一半，Harness 策略版本升到 `harness-v4`。总用量没有大幅下降，因费用很低，暂不继续压缩。

## 2026-11-02 · 部署与运维

- [任务 0011](../tasks/0011-deployment-operations.md)：镜像、Compose、`/metrics`、告警规则、部署指南和值班手册；发布检查增加密钥扫描，CI 增加整栈冒烟测试。[远端 CI](https://github.com/HughTaiyaki/incident-agent/actions/runs/41326433875) 通过，尚未在正式环境部署。
- 决定：常驻 Worker 跳过损坏、未知版本和配置不匹配的 Run 并告警，不再退出。之前的“停止并阻塞”会让一个坏 Run 拖垮整个服务；Run 仍保留并占名额，由人处理。
- 决定：`INCIDENT_MAX_ACTIVE_RUNS` 建议从 2 开始、不超过 8，依据是单机测量和串行 Worker，不是生产压测，见[部署指南](operations/deployment.md#3-容量取值)。

## 2026-11-02 · DeepSeek 联调

- 完成[任务 0009](../tasks/0009-online-investigation.md)：操作员选定 DeepSeek，密钥只从环境变量读取。在线后端加真实模型的完整运行、`kill -9` 恢复和业务 ID 遮蔽都已验证；诊断效果未评测，结果都是 INCONCLUSIVE，遗留问题见[改动报告](reports/0009-online-investigation.md#在线完整运行)。
- 决定：Provider 增加显式 profile，而不是按地址判断或另写 Adapter，因为只有请求字段不同；profile 计入 Provider 身份。
- 决定：报告预留从上限改为下限并升到 6,000，未查询前的报告不接受；行为变化详见[改动报告](reports/0009-online-investigation.md#deepseek-联调)。

## 2026-11-01 · HTTP 调查入口与 Token 结算

- [任务 0010](../tasks/0010-http-entry.md)：Bearer Token、按 alert_id 幂等提交、Run 查询/报告/证据/取消，readyz 改为检查 Redis 和 Worker 心跳，默认只监听回环。
- 修复持久模式从不退还 Token 预留的问题，使其符合调查协议 §3；投影字段改名为 `charged_tokens`。
- 决定：P2 的供应商联调要等外部配置，P3 不依赖它，所以并行推进，P2 门槛不放宽。
- 在本机真实后端上配合脚本模型跑通全流程，详见[改动报告](reports/0010-http-entry.md)。未调用真实供应商；[远端 CI](https://github.com/HughTaiyaki/incident-agent/actions/runs/41315442725) 在提交 `75c2f49` 上通过。

## 2026-11-01 · 重建远端仓库

- 用户删除并重建公开仓库，以清除仍可通过旧提交号访问的简历目标稿；旧提交 `df36eee` 的页面和文件链接已确认返回 404。本地 main 历史和提交号不变，但重建前的 Actions 运行记录已不存在，此前文档记录的远端通过结果无法再回看。
- 决定：大功能完成并通过检查后推送远端，提交信息使用 Conventional Commits 格式。

## 2026-11-01 · 改写历史移除简历目标稿

- 用户决定从 Git 历史中移除简历目标稿，并强制推送 main；所有提交号因此改变，文档中的提交号已更新。早期 CI 运行记录显示的是改写前的提交号。
- GitHub 上旧提交在清理前仍可按提交号访问，彻底清除需要 GitHub 支持或重建仓库。

## 2026-11-01 · 在线持久调查工程切片

- [任务 0009](../tasks/0009-online-investigation.md)：incident-worker 增加 `-live` 和匿名 export，后端地址显式配置并冻结身份。
- 修复接入在线数据才暴露的问题：单次模型/查询超时、超大查询结果都不再让 Worker 退出；Harness 生成的失败结果来源标为 `harness`。
- 决定：发往模型的业务 ID 按 Run 假名化，Trace/Span ID 保留以便继续查询；存储额度为后续模型决策和失败记录预留。
- 仅完成本地模拟验证，见[改动报告](reports/0009-online-investigation.md)；真实后端与真实供应商未联调。

## 2026-11-01 · 简历目标稿移出仓库

- 用户决定简历目标稿不公开：文件保留在本地并被 Git 忽略，入库文档中的链接改为文件名说明。

## 2026-11-01 · 首次推送与远端 CI

- 首次提交 `42c8f3f` 推送到公开仓库 [HughTaiyaki/incident-agent](https://github.com/HughTaiyaki/incident-agent) 的 main 分支；[远端 CI](https://github.com/HughTaiyaki/incident-agent/actions/workflows/agent-release-check.yml) 在 ubuntu-24.04 通过，完成 P0 与 P1 的远端门槛。
- 决定：仓库公开，本机路径改为相对路径；新增根目录 `.gitignore`；提交者邮箱使用 noreply 地址。

## 2026-11-01 · Run 容量与终态查询

- 完成[任务 0008](../tasks/0008-capacity-and-results.md)本机验收：create 原子校验活跃数上限和每活跃 Run 8MiB 内存预留，终态释放名额，接管和重复 ID 不占额。
- 决定：活跃数取索引基数之和，不另设计数器；`INCIDENT_MAX_ACTIVE_RUNS` 无默认值，因尚无压测依据；maxmemory 改为必需。
- status/cancel 改为 `run-result-v1` 投影，只有 COMPLETED 带报告；新增 evidence 模式 `run-evidence-v1`；区分 CORRUPT、UNKNOWN_SCHEMA、NOT_FOUND。均为不兼容的 CLI 变更，状态 schema 不变。
- 完整 `make release-check` 通过。未压测、未运行远端 CI、未调用真实供应商；详见[改动报告](reports/0008-capacity-and-results.md)。
- 新增[任务 0009](../tasks/0009-online-investigation.md)为下一切片，修正 README 中过时的当前任务链接。

## 2026-11-01 · 生产工程基线与后续路线

- 完成[任务 0007](../tasks/0007-production-baseline.md)本机验收：Go 1.26.10、固定 lint v2.15.0/govulncheck v1.9.0、只读格式检查和统一 release-check。
- 完整发布检查通过，含 race、真实 Redis 恢复、五个命令构建；普通与 Redis 标签扫描均无已知可达漏洞报告，解决旧工具链扫描阻断。
- 新增仓库根 GitHub Actions，固定 Action 提交与只读权限；actionlint 本地通过，远端 CI 尚未运行。
- 新增[生产路线](development/production-roadmap.md)、[发布指南](development/release-checks.md)、[改动报告](reports/0007-production-baseline.md)和[下一阶段容量/终态任务](../tasks/0008-capacity-and-results.md)，维护渐进式入口。
- 未改变运行依赖、调查状态协议或预算；未修改商城、全局 Go 环境，未调用真实供应商，未推送或部署。

## 2026-11-01 · Harness checkpoint 与 Worker 恢复

- 完成任务 0006 本切片：共享 Execute/Resume、版本化 checkpoint、持久调用账目、保守预算恢复、启动/周期扫描与续租。
- 新增 incident-worker 的 enqueue/work/status/cancel，冻结模型/Fixture 配置；旧内存入口保留。
- 验证确认丢失、模型提交后的真实进程退出及租约到期接管、未知调用不退款/不重发、CLI 与取消/到期收尾。
- 新增[Worker 指南](development/worker-recovery.md)和[改动报告](reports/0006-worker-recovery.md)。商城未改动；真实供应商、在线持久调查、全局容量和工具链安全更新仍待验收。

## 2026-11-01 · Redis Go 客户端

- 用户授权后固定 go-redis/v9 v9.20.1，新增 go.sum，保持 Go 1.25。
- 新增显式配置、TLS 约束、有界连接与超时、无自动重试的客户端；保留原子协议响应码和不确定写入语义。
- 功能验证通过；govulncheck 报告当前 Go 1.25.3 有 29 项可达标准库漏洞，生产安全验收未通过。
- 增加客户端隔离、幂等、接管、超时读回及重启测试；Harness 与 Worker 接线仍待实现。详见[增量报告](reports/0006-redis-client.md)。

## 2026-10-31 · Redis 原子状态协议切片

- 启动[任务 0006](../tasks/0006-redis-recovery.md)，新增 Lua 状态协议、embed 入口与独立 Unix socket Redis 测试。
- 覆盖租约、epoch/version、调用预留、幂等结果、取消/到期终结、容量拒绝及 AOF 正常重启读回。
- 新增[协议指南](development/redis-state.md)与[改动报告](reports/0006-redis-protocol.md)，同步当前入口。Go 客户端和 Harness 恢复尚未接线，未新增依赖或改动商城。

## 2026-10-31 · 模型调用边界、输入选择与 Token 预算

- 完成[任务 0005](../tasks/0005-model-boundary.md)：新增 ModelDecider、Provider 接口、有界上下文和内存调用账目；输入和输出共同预留，调查保留报告额度，失败/未知 usage 不退款，超预留停止后续调用。
- 新增固定地址的 Chat Completions HTTP Adapter：显式 HTTPS/回环配置，无代理、重定向或应用重试，超时/响应有界；密钥和原始错误正文不落盘。
- 新增 `incident-model` 命令、请求示例和构建目标；原有 `incident-investigate` 保持完全离线。策略为 harness-v3，Run 增加 model_calls。
- 定向测试、make verify、make build、go mod verify 通过；构建后的命令对本地模拟服务完成两次模型调用、四条日志、零遥测后端请求，模拟 usage 合计 1,000 Token，报告 INCONCLUSIVE/UNKNOWN。
- 新增[Provider 指南](development/model-provider.md)与[改动报告](reports/0005-model-boundary.md)，同步 README、AGENTS、任务和渐进式入口；收尾核对任务 0004 的既有运行产物。
- 无新增依赖，未修改商城或历史 Bundle，未调用真实供应商。调用账目尚不持久；真实效果、身份认证、独立 Redis 与恢复等生产门槛明确保留待验收。
- 启用模型模式前按指南配置四项环境变量，确认模型上下文和匿名数据外发范围；默认不选择供应商、不自动读取既有密钥。

## 2026-10-29 · 确定性日志聚合

- 完成[任务 0004](../tasks/0004-log-patterns.md)：随已保存 logs 查询生成最多十组结构摘要，保留样本口径、首末时间、三个代表引用、来源质量与省略组数；不新增后端请求。
- Run/View 增加 log_patterns，摘要计入容量并提供隔离副本；策略升级为 offline-harness-v2。原始脚本和历史 Bundle 保持兼容。
- 定向测试、make verify、make build、go mod verify 通过；离线示例扫描四条日志，生成一组、三个有效引用，仍为七次决策、169 条记录和零后端请求。
- 新增[使用指南](development/log-patterns.md)和[改动说明](reports/0004-log-patterns.md)，同步任务、状态与阅读入口。无需调整商城；真实模型、Token 预算与效果评测仍未实现。

## 2026-10-29 · 补充任务 0003 改动说明

- 按用户要求新增[本轮改动报告](reports/0003-offline-investigation.md)，解释修改前后行为、文件职责、执行流程、既有验证结果与未完成范围。
- 在索引、离线使用指南和任务 0003 中补充入口。本次仅修改文档，核对文件链接与围栏，不重跑 Go 测试或商城实验；工程测试结果引用本轮此前的已执行记录。

## 2026-10-29 · 离线单次调查 Harness 与报告 Guard

- 按用户“暂不修改商城，继续 Agent”的范围，实现[任务 0003](../tasks/0003-offline-investigation.md)。任务 0002 的固定版本故障数据仍待验收；将商城改动需先提醒用户审查写入 AGENTS。
- 新增 `app/investigation`：冻结查询范围、决策 schema、同查询最多两次、无进展检测、12 次决策/16 次实际请求/时间与容量上限、取消和确定性 INCONCLUSIVE 收尾。
- 新增 Report Guard：本 Run 记录引用、排序时间线、假设支持/反例、有限敏感标记筛选及一次修正；恢复判断暂只接受 UNKNOWN，引用存在不等于语义成立。
- 新增脚本 Decider 与 `incident-investigate`，输出本次证据、步骤、报告和来源摘要；只运行离线 Fixture，文件不覆盖。同步[使用指南](development/offline-investigation.md)、README、设计状态和渐进式入口。
- 新包定向测试与 `make verify`、`make build`、`go mod verify` 通过；构建后的 CLI 实测 7 次脚本决策、169 条记录、0 次后端请求，输出 INCONCLUSIVE/UNKNOWN，4 条事实引用核查通过。
- 不新增依赖，不修改商城代码或运行服务，不改写既有历史 Bundle；未执行在线故障实验或调用模型。Token 预算、输入压缩、持久 Run/恢复、告警和工作台尚未实现，本切片不是模型效果验收。
- 手动体验：按使用指南执行离线 CLI，选择新的输出路径；无需调整商城或启动基础设施。

## 2026-10-28 · 确认本机商城入口与实验前置条件

- 将用户提供的商城路径加入项目 AGENTS，并在[基线文档](integration/goframeshop-baseline.md#本机入口与运行状态)集中记录本机核对结果；同步任务 0002 的剩余条件。
- 当前仓库 HEAD 为 `2f25dc5`，运行进程对应二进制记录 `bb921e9` 且构建时存在修改；区分工作区版本、二进制元数据与历史遥测来源，沿用旧设计基线和样本 `unknown`。
- 核对实验子测试入口与端口检查：四个业务服务正在运行；实验还检查 gateway-h5 `8199`。记录 T3 单场景选择方式，提示完整 T9 的队列清空行为。
- 只读核对 Git、进程、构建元数据和实验源码，并检查 Markdown 链接；仅修改 Agent 文档，未运行商城实验、停止服务、修改上游文件或重跑 Go 测试。后续固定版本采样需先完成基线核对与实验运行环境准备。

## 2026-10-28 · 实现三源只读查询与冻结证据回放

- 新增 `internal/telemetry` 查询/证据契约、三源 HTTP Adapter、Fixture Replay、采集应用层与 `incident-query` CLI；使用标准库，不新增第三方依赖。
- 实现服务/环境/时间/字段白名单、固定 PromQL/LogQL、请求前扣预算、取消/超时、有限重试、响应大小限制、来源质量与未定义值处理；保留 Trace 引用并裁剪越界 Span。
- 首版省略日志正文和异常栈，导出时一致匿名化关联 ID；快照限定字段并校验摘要，拒绝覆盖既有输出，离线按冻结数据过滤。当前没有分页游标或持久 Run。
- 新增 build 的查询二进制和显式 `make test-integration`，同步[使用指南](integration/query-adapters.md)、[接入报告](reports/0002-readonly-integration.md)及项目状态入口。
- 本机三源历史查询得到 169 条结构记录，保存于 [observed-20261028](../testdata/telemetry/observed-20261028/README.md)；回放返回相同条数、0 次后端请求。`make verify`、`make build`、显式在线集成测试及回归测试通过。
- 商城工作区存在后续提交和未提交修改，历史遥测运行版本未证实，样本标记 unknown；未修改商城或运行故障实验。任务 0002 继续保持进行中，固定版本 D1/D3/N1 与证据不足对照仍待验收；不宣称模型诊断、恢复或效果成绩。
- 使用时在项目目录 `make build`，按指南执行回放或准备真实查询窗口；在线集成测试需要额外指定 `INCIDENT_INTEGRATION_QUERIES`，不启动依赖也不自动跳过。

## 2026-10-28 · 维护 AGENTS 与渐进式文档入口

- 阅读现有 15 份项目 Markdown，沿用项目 `AGENTS.md` 作为规则入口；在工作区根目录添加短入口，避免从父目录工作时漏读项目约束。
- 整理 [AGENTS.md](../AGENTS.md) 为项目状态、任务路由、架构边界与不变量；明确文档请求不自动启动当前开发任务。
- 新增[渐进式阅读指南](reading-guide.md)：固定约束 → 任务定位 → 专题契约 → 实现/证据；按任务列出章节、深入条件和停止条件。
- 新增[工作流程与维护规则](development/workflow.md)，落实按复杂度选择流程、按风险验证、内容归属、变更同步与交接要求。
- 同步索引、README、架构导航和任务 0002，将全量必读清单改为按工作项读取。保留当前开发切片和全部待验收状态。
- 本次只改 Markdown；18 份 Markdown 的 162 个本地链接、代码围栏、路由涉及的章节编号和 8 个现有 Makefile 命令检查通过，并复核当前状态与任务范围一致。未运行 Go 测试、商城实验或模型评测，未重新核验上游源码。沿用既有固定提交基线，不新增功能或实测成绩。

## 2026-10-28 · 统一整体设计、简历目标与产品闭环

- 重写用户指定的 [03 整体设计](../03-日志诊断Agent详细设计.md)，统一产品范围、选型理由、代价与实施顺序，加入产品闭环图、整体架构图和执行时序图。
- 新增 `04-IncidentLens简历目标稿.md`（本地文件，不入库），包含完整稿、一页压缩稿及证据要求；全部标为待实现，不填写虚构成绩。Agent 框架采用现有 net/http。
- 采纳参考项目的产品启发，补充精确告警归并、日志结构聚合、来源质量、快照回查与人工恢复验证；不复制其采集栈或未经核验的 HA／模型能力。
- 同步调查协议、遥测契约 v2.1、评测、任务边界、README 与索引。原 overview 改为导航，避免两套整体设计分叉。
- 本次只改 Markdown；未开发 Agent、运行商城实验或取得诊断成绩。已检查 15 份 Markdown 的代码围栏与 87 个本地链接；主设计 3 张图通过 Mermaid 语法解析。未运行业务测试，文档校验不代表功能验收。

## 2026-10-27 · 完成文档复核

确认 13 份文档的入口与状态一致，58 个相对链接、36 个固定提交源码链接检查通过；旧商城建设任务和自研 KV 不再出现在有效实施路线中。当前任务仍为 0002，尚未开始 Adapter 开发或在线接入验收。

## 2026-10-26 · 接入已完成商城，替换旧实验场规划

### 范围与依据

用户确认 GoFrame 商城已完成、旧小型实验场废弃、不需要自研 KV。本次读取上游提交 `385d3649b4f38f1db82e65aade57b00e10badb02` 的业务、遥测、Compose 与故障实验代码，并更新 Agent 文档；未改 Agent Go 代码或上游商城。

### 变更

- 更新 README、AGENTS 与索引，区分商城实现、Agent 骨架和目标设计。
- [ADR-0002](adr/0002-reuse-goframeshop.md) 替代旧实验场决定；旧 ADR/任务保留废弃标记，当前入口改为[商城接入任务](../tasks/0002-goframeshop-integration.md)。
- [代码基线](integration/goframeshop-baseline.md) 记录固定源码链接、部署版本、可复用能力与接入差距。
- [遥测契约 v2](telemetry/data-contract.md) 采用实际 11 个服务名、10 个指标与 7 类日志事件，复用 Jaeger，取消 Tempo 路线。
- 明确 DB 等待指标的误导性名称、业务积压与 MQ 队列的区别、Loki body/metadata 与 stdout JSON 的差异、跨 Trace 关联及告警持续阈值。
- [调查设计](architecture/investigation-design.md) 补齐任务重新发现、续租与业务版本分离、一次提交结果、模型预算审计、取消/截止与确定性收尾。
- [评测方案](evaluation/plan.md) 分开诊断、Harness 和在线验证；隔离 failpoint stderr/实验真值；补充人工反馈和恢复验证闭环。
- Agent 保留 net/http 骨架，独立持久 Redis 为待实现设计；移除自研 KV 的未来任务。

### 验证与后续操作

- `make test`：通过，现有 httpapi/app 测试通过，cmd 无测试。
- Markdown 相对链接、代码围栏、固定提交源码路径和遗留规划一致性检查通过；指标/事件/服务名称已与该 SHA 的源码交叉核对。
- 未启动商城、未运行 `make fault-trade`、未实测遥测查询或 Agent 诊断效果；上游实验成绩仅按来源引用。
- 本次不需要迁移数据库或调整运行环境。下一步按任务 0002 在独立商城环境验收只读查询与脱敏 Fixture，再推进 Agent 实现。
