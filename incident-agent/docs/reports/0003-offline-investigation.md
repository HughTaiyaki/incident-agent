# 任务 0003 改动说明：从查询回放到离线调查

日期：2026-10-29。对应[任务 0003](../../tasks/0003-offline-investigation.md)。本文回答“这次改了什么、为什么改、如何核查”；操作细节集中在[离线调查使用指南](../development/offline-investigation.md)。

## 1. 这次带来的变化

之前的 `incident-query` 接收一组查询，执行后保存遥测 Bundle；它没有调查决策循环，也不校验诊断报告。

这次新增 `incident-investigate`：读取已有 Bundle 和人工编写的决策脚本，在固定范围内逐步查询，记录每一步结果，最后保存带证据引用的报告。程序负责检查能否查询、是否继续、引用是否存在，以及失败时如何结束。

目前的“决策者”是本地脚本，尚未接入大模型。此次完成的是将来承载模型调查的执行内核，不是自动根因诊断能力。

| 对比项 | 修改前 | 修改后 |
| --- | --- | --- |
| 输入 | 查询列表 | 调查范围＋查询/报告决策脚本 |
| 执行方式 | 顺序采集查询 | 每轮检查决策、执行查询或提交报告 |
| 调查停止 | 查询完成或采集失败/取消 | 增加决策上限、重复限制、无进展、时间和证据容量限制 |
| 结果 | 冻结遥测 Bundle | 本次 Run 的报告、冻结证据、步骤记录和来源信息 |
| 报告引用 | 没有报告校验入口 | 引用必须对应本 Run 已保存的成功查询记录 |
| 运行依赖 | 查询可在线或离线 | 新调查 CLI 仅离线，复用已有匿名 Bundle |

原有查询 CLI 和健康检查 API 保留；本次没有给 API 增加调查接口。

## 2. 具体新增和修改的文件

| 文件 | 改动及作用 |
| --- | --- |
| [types.go](../../internal/app/investigation/types.go) | 新增 Request、Decision、Run、Report 等数据结构；在使用方定义 Decider/Querier 接口，将调查规则与决策来源、遥测来源分开 |
| [runner.go](../../internal/app/investigation/runner.go) | 新增单次调查执行循环，冻结范围、限制调用、处理取消/超时、保存证据、检测无进展并确定性收尾 |
| [guard.go](../../internal/app/investigation/guard.go) | 新增输入/决策/报告校验，检查引用、时间线、假设支持与反例，以及有限的敏感标记 |
| [script.go](../../internal/adapter/outbound/scripted/script.go) | 新增脚本决策来源，逐次返回人工写好的 JSON 决策；用于开发验证，不冒充真实模型 |
| [main.go](../../cmd/incident-investigate/main.go)、[run.go](../../cmd/incident-investigate/run.go) | 新增离线命令入口、信号取消、输入组装、JSON 摘要和完整运行产物写入；输出文件权限 0600，拒绝覆盖 |
| [Makefile](../../Makefile) | `make build` 增加构建 `bin/incident-investigate` |
| [observed-script.json](../../testdata/investigation/observed-script.json) | 新增可运行示例，使用已有匿名历史样本执行六次查询并提交一份证据不足报告 |
| [runner_test.go](../../internal/app/investigation/runner_test.go)、[guard_test.go](../../internal/app/investigation/guard_test.go) | 新增执行规则、失败路径、容量和报告引用测试 |
| [script_test.go](../../internal/adapter/outbound/scripted/script_test.go)、[run_test.go](../../cmd/incident-investigate/run_test.go) | 新增脚本解析、取消、CLI 离线闭环和不覆盖测试 |

文档方面新增任务 0003、离线使用指南与脚本说明；同步 README、AGENTS、索引、渐进式阅读指南、架构状态、任务 0002 的遗留项和 changelog。本说明补充文件级变更入口，不另建一套执行协议。

## 3. 一次运行怎样完成

1. CLI 读取脚本和 Bundle，验证输入与 Bundle 摘要。
2. Harness 创建 Run，固定服务白名单、环境、时间窗和截止时间。
3. 脚本提交一个 query 或 report 决策；Harness 先检查结构与权限。
4. 合法查询交给 Fixture Adapter，从已有冻结数据中筛选记录；结果保存为本 Run 的独立副本。
5. 后续决策可读取证据副本，但不能修改已保存的证据。
6. 报告 Guard 检查草稿；不合格时最多给一次有剩余额度的修正机会。
7. 保存报告与步骤；无法继续时生成 INCONCLUSIVE，取消时记录 CANCELLED。

执行内核默认最多 12 次决策、16 次实际后端请求、同一规范化查询两次。连续三次无新记录或拒绝会进入汇总。Run 最长 180 秒，证据紧凑 JSON 总量最多 2MiB。完整规则与边界以[使用指南 §3](../development/offline-investigation.md#3-执行与停止)及代码为准。

例如，同一日志仅因再次采集而得到新的快照 ID，不会被算成新进展；报告引用了其他 Run 才有的记录，则不能通过当前 Run 的引用检查。

## 4. 实际验证结果

以下为本轮开发时已执行的检查；补写本文时未重新运行 Go 测试。

| 检查 | 结果 |
| --- | --- |
| 新增包的定向测试 | 通过 |
| `make verify` | fmt、vet、race 测试和 lint 通过 |
| `make build` | API、查询和调查三个二进制构建通过 |
| `go mod verify` | 通过；本轮未增加第三方依赖 |
| 构建后的调查 CLI 实测 | 7 次脚本决策、6 个查询结果、169 条结构记录、0 次后端请求 |
| 报告核查 | INCONCLUSIVE；恢复 UNKNOWN；事实中的 4 条日志引用均存在于本 Run |

测试覆盖越界决策不触发查询、伪造引用、一次报告修正、重复查询、采集时间去重、末次决策保留、实际请求预算、来源失败、迟到结果丢弃、取消、截止、容量上限、共享内存隔离与输出不覆盖。

样本结论是人工写入脚本的，不是模型自主推断；169 条记录只说明读取与保存闭环跑通，不能用于计算诊断准确率。既有历史 Bundle 保持不变，运行源码版本仍标为 unknown。

## 5. 本次没有完成什么

- 没有真实模型调用、Token 预算、Provider usage 核算或模型上下文压缩。
- 没有 Redis 持久 Run、Worker 调度、租约、崩溃恢复或断点续跑。
- 没有自动告警、HTTP 调查接口、SSE 或前端工作台。
- 没有固定版本故障集、真实模型效果评测或恢复对照；任务 0002 的对应验收仍未完成。
- 没有修改商城代码、配置和运行服务，也没有执行商城故障注入。

报告 Guard 只能检查结构与引用归属，不能证明“证据确实支持这句话”。当前恢复状态只接受 UNKNOWN。下一步接入真实模型前，需要补齐 Token 预算、上下文装配，并使用独立样本验证语义效果。

商城相关代码继续保持不动；若后续确需调整，先说明原因和改动范围，提醒用户审查。

## 6. 从哪里开始看代码

只想运行：读[离线使用指南 §1](../development/offline-investigation.md#1-运行)。

想理解本轮实现：先看 types.go 的输入输出，再看 runner.go 的循环，最后看 guard.go 的校验与对应测试。涉及模型接入时再读[调查协议 §3–4](../architecture/investigation-design.md)，不需要先通读 Redis 恢复设计。
