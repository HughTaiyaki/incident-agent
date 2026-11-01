# 离线调查 Harness

更新：2026-10-29。适用于 `incident-investigate` 和[任务 0003](../../tasks/0003-offline-investigation.md)。这是已实现的内存执行内核与脚本决策回放；完整产品协议仍见[调查设计](../architecture/investigation-design.md)。没有真实模型调用，也不提供诊断准确率成绩。

本轮前后变化、文件清单和验证结果见[任务 0003 改动说明](../reports/0003-offline-investigation.md)。

## 1. 运行

在 Agent 项目目录执行，无需启动商城或遥测后端：

```bash
make build
mkdir -p data
./bin/incident-investigate \
  -fixture testdata/telemetry/observed-20261028/bundle.json \
  -script testdata/investigation/observed-script.json \
  -out data/investigation.json
```

每次使用新的输出文件名。输出文件权限为 `0600`，已有文件不覆盖。stdout 只输出 Run ID、执行状态、诊断结果、停止原因、决策数和实际后端请求数；完整报告与证据保存在 `-out`。输入摘要和报告文本只用于本地核查，仍应由操作者保证不含秘密或个人隐私。

退出码 `0` 表示已保存完成的 Run，包含正常的 INCONCLUSIVE 或受限收尾；`1` 表示取消或运行/文件错误；`2` 表示参数或输入错误。`COMPLETED` 不表示“诊断正确”或“业务恢复”。进程被强制终止时可能留下不完整文件，当前没有断点恢复或原子发布保证。

现有样本执行 7 次脚本决策（6 次查询＋1 次报告），保存 169 条结构记录，实际后端请求 0 次；报告引用 4 条日志，结果 INCONCLUSIVE，恢复 UNKNOWN。引用对应的断言是人工编写的脚本内容，不是模型自主发现。该普通历史样本不替代固定版本故障集。

## 2. 输入与范围

脚本包含 `request` 与 `decisions`，完整可运行格式见[示例脚本](../../testdata/investigation/observed-script.json)。输入最多 256KiB、1–12 个决策；每个决策最多 64KiB。未知字段、非法 JSON 和尾随数据被拒绝；单个非法决策由 Harness 消耗一次决策额度后记录拒绝。

`request` 包含入口 `service`、显式 `services` 白名单、`environment`、冻结 `start/end` 和最多 2,000 字符的 `summary`。服务必须属于部署白名单，入口服务必须在本次范围内，窗口最多一小时。该范围由本机操作者选择，未来 HTTP 接口不能直接把客户端列表当成授权范围。

决策只允许两种形状：

```json
{"kind":"query","query":{"kind":"logs","service":"order-service","environment":"local","start":"2026-10-28T06:00:00Z","end":"2026-10-28T06:05:00Z"}}
```

```json
{"kind":"report","report":{"result":"INCONCLUSIVE","facts":[],"timeline":[],"alternatives":[],"unknowns":["现有证据不足。"],"next_actions":[],"recovery_assessment":"UNKNOWN"}}
```

查询复用现有 telemetry schema，时间窗只能缩小，不能扩展服务和环境；Trace 返回的 Span 也必须在本次白名单内。CLI 只组装 Fixture Adapter，没有 URL、在线模式、Shell、SQL、故障注入或商城源码读取入口。匿名关联 ID 不能用于真实后端重查。

## 3. 执行与停止

| 限制 | 实现行为 |
| --- | --- |
| 12 次决策 | 非法、被拒绝、超时和失败也计数；第 12 次只允许报告 |
| 16 次实际请求 | 使用共享 telemetry Budget，Adapter 在每次请求/重试前预留；离线回放不消耗后端额度 |
| 同一查询最多两次 | 规范化默认值及窗口后比较；不同采集时间不改变查询身份 |
| 连续三次无进展 | 无新记录或连续拒绝转 SYNTHESIZING；还有额度时给一次汇总机会 |
| 180s Run 截止 | 父 context 更短时沿用父截止；决策最多 30s，查询最多 5s；剩余 30s 转汇总 |
| 证据最多 2MiB | 按证据及其日志摘要的紧凑 JSON 累加；超限响应不保存，停止新查询并尝试报告；缩进文件本身体积可能更大 |
| 报告修正 | 最多一次且受剩余决策/时间限制；修正阶段只允许报告，否则确定性收尾 |

新进展按记录规范化内容去重，忽略记录 ID 与采集时间；重复读取同一事实不会因新快照 hash 被算为新进展。这是启发式记录去重，不证明不同记录是独立根因证据。

内核保存脱离 Adapter/Decider 共享内存的证据副本；传给 Decider 的 View 也是副本。拒绝的原始决策、错误正文与私有思维过程不落盘，仅保留固定结果码。来源失败保留 UNAVAILABLE 查询结果；取消后不再调用 Decider/Querier，迟到结果不进入报告。

所有实现必须配合 context；当前不通过额外 goroutine 强行终止不合作的 Provider。脚本没有 Token 费用，脚本模式不涉及 Token；独立模型模式已实现预留、usage 核算与有界输入，见[Provider 指南](model-provider.md)，仍不具备持久预算和恢复保证。

## 4. 报告与核查

`facts` 和 `timeline[].claim` 是带 `evidence_ids` 的断言；ID 必须精确匹配本 Run 已保存、成功查询返回的记录。时间线必须排序，时间戳必须匹配其引用记录之一。主假设与备选假设包含 `claim`、`contradicting_ids`、`strength`；强度仅 LOW/MEDIUM/HIGH，同一记录不能同时作为该假设的支持和反例。

DIAGNOSED 至少有事实与带引用主假设；INCONCLUSIVE 必须列出未知项。当前恢复状态只接受 UNKNOWN；跨窗口恢复判断留待后续切片。事实/时间线各最多 20 条、备选最多 3 项、未知项 20 条、建议 10 条；单个文本最多 2,000 字符，单个断言最多 20 个引用。

Harness 检查 schema、引用存在、已保存证据范围、时间线及常见敏感标记。敏感标记检查是有限规则，不保证任意自由文本完全脱敏，也不能证明引用语义成立。不要将此 Guard 当成事实验证器或模型安全评测结果。

`run.evidence` 始终保留各来源原始规范化结果中的 query、source_status、source_quality、count_basis、采集时间及摘要，报告无法覆写这些质量信息。即使草稿通过，人工仍需检查引用是否支持断言、是否忽略反例与未知覆盖。

落盘顶层包含 `mode=scripted_replay`、输入 Bundle digest、upstream_sha、Run、步骤和报告。Run ID、停止原因与策略版本由 Harness 写入，决策不能指定。摘要 hash 用于核对内容，不等于签名或防篡改存储。

日志查询的派生摘要见[日志聚合指南](log-patterns.md)。当前策略为 `harness-v3`，输出新增 `run.log_patterns`，原始脚本继续兼容；摘要与证据合计受容量上限约束。

## 5. 验证与后续

```bash
go test ./internal/app/investigation ./internal/adapter/outbound/scripted ./cmd/incident-investigate
make verify
make build
go mod verify
```

测试覆盖正常闭环、非法/越界决策、伪造引用、一次修正、重复查询、采集时间去重、无进展、实际请求预算、末次决策保留、来源失败、迟到结果、取消、截止、容量、共享内存隔离和 CLI 不覆盖。

任务 0002 的固定版本故障案例仍未验收。独立模型模式已具备有界输入和预算，真实供应商与效果仍待验收；Redis 恢复、租约、自动告警、HTTP 调查 API 和 SSE 仍未实现。商城代码暂不修改，确需调整时先说明原因、范围并提醒用户审查。
