# 任务 0004 改动说明：日志结构摘要

日期：2026-10-29。本轮目标是在已有调查闭环中加入确定性日志聚合，为后续模型上下文装配准备可回查的摘要；商城代码与运行服务未改动。

## 修改前后

此前 Run 只保存查询返回的日志记录。现在每个已保存的 logs 查询还会生成最多十组结构摘要，每组包含样本数量、首末时间和最多三个代表记录引用。下一轮决策可以读取摘要，同时原始证据仍可核查。

分组不依赖大模型，不读取日志正文，不按订单号等高基数字段拆组。相同结构、不同订单的日志可进入同组；缺失字段保留 unknown。摘要明确是样本统计，不能用来声称整个时间窗有多少异常。

## 文件变化

| 文件 | 变化与目的 |
| --- | --- |
| [patterns.go](../../internal/app/investigation/patterns.go) | 新增分组、稳定排序、代表记录选择、正常信号候选保留和来源质量复制 |
| [types.go](../../internal/app/investigation/types.go) | Run 与 View 增加 log_patterns；Harness 策略升级为 offline-harness-v2 |
| [runner.go](../../internal/app/investigation/runner.go) | 保存日志查询时同步生成摘要；摘要计入证据容量，不追加查询或调查进展 |
| [patterns_test.go](../../internal/app/investigation/patterns_test.go) | 新增分组与集成回归测试，验证省略、代表性、来源失败、越界、摘要引用和副本隔离 |
| [任务 0004](../../tasks/0004-log-patterns.md)、[使用说明](../development/log-patterns.md) | 明确目标、验收、输出语义与后续边界 |

README、AGENTS、索引、阅读指南、调查协议、架构状态与 changelog 同步维护。原有脚本、历史 Bundle、查询 Adapter、商城仓库和依赖未修改。

## 如何避免误导

保留 OBSERVED_SAMPLE 计数口径和原始 source_quality；空摘要、未知覆盖、查询失败都不升级为正常。代表记录优先保留首末及不同 outcome，展示超限时为显式成功信号候选预留一组；这仍不是完整反例保证或恢复判断。

聚合没有创建新事实。报告仍引用本 Run 已保存的原始记录，重复采集与摘要生成不会重置无进展计数。摘要与证据共用容量上限，Decider 获得隔离副本。

## 验证结果

- 新增定向测试与 `make verify` 的 fmt、vet、race、lint 通过。
- `make build` 和 `go mod verify` 通过。
- 使用原有脚本和历史 Bundle 运行构建后的 CLI：仍为 7 次决策、169 条记录、0 次后端请求；新增一份日志摘要，扫描 4 条日志，输出 1 组、3 个有效原始引用。
- 报告仍为 INCONCLUSIVE、恢复 UNKNOWN；来源 PARTIAL 和覆盖缺口保持不变。

这些结果验证程序行为，不是模型诊断成绩，也没有执行商城故障实验。

## 下一步目标

下一开发切片应完成**有界模型上下文装配与 Token 预算**：在保留范围、来源缺口、证据引用和反例线索的前提下选择输入；调用前预留预算，单独保留报告额度，并测试失败/未知 usage 的保守计费。真实 Provider 接入和固定故障样本效果评测随后实施。

本轮尚未实现这些预算机制，也没有启用模型、Redis、恢复或工作台。运行和字段说明见[日志聚合指南](../development/log-patterns.md)。
