# 结构化日志聚合

更新：2026-10-29。对应[任务 0004](../../tasks/0004-log-patterns.md)。本功能已接入离线 Harness；设计依据为[调查协议 §10.2](../architecture/investigation-design.md#102-确定性日志聚合)。文件级变化与验证结果见[改动说明](../reports/0004-log-patterns.md)。

## 1. 解决的问题与运行方式

一次日志查询可能包含多个订单的同类事件。逐条阅读容易被重复记录淹没；只给出一句摘要又会丢失计数口径、来源缺口和原始证据。本功能使用已有结构字段分组，同时保留代表记录 ID。

运行方式不变，见[离线调查指南](offline-investigation.md#1-运行)。每个已保存的 logs 查询自动生成一份摘要，输出到 `run.log_patterns`，下一轮 Decider 的 View 也包含这些摘要。原始 `run.evidence` 完整保留。未新增命令参数或可选决策类型，query/report 脚本继续兼容。

在现有历史样本中，四条 INFO 日志生成一组，保留三个代表记录 ID；其 event_name/error_type 缺失，明确标为 unknown。来源仍是 PARTIAL，不会因成功生成摘要而升级为数据完整。

## 2. 分组与展示规则

| 规则 | 实现 |
| --- | --- |
| 输入 | 本 Run 单次已验证 logs 查询，最多 100 条记录；不合并不同查询或窗口 |
| 分组键 | service、event_name、error_type、severity_text |
| 缺失字段 | 输出 unknown；不解析正文、不猜测错误类型 |
| 关联 ID | order_id、trace_id 不作为分组键；代表记录可回到原始证据查看 |
| 计数 | 固定 OBSERVED_SAMPLE；observed_count 是返回样本内的记录条数，不是整个时间窗的事件总量，也不是独立故障次数 |
| 展示上限 | 最多 10 组，每组最多 3 个原始记录 ID |
| 组排序 | 样本记录数降序，同数量按序列化分组键排序 |
| 正常信号候选 | 若前十组没有候选，用首个未展示候选替换第十组；候选要求 error_type=unknown、等级 INFO/DEBUG，且至少一条 outcome 为 success/replayed/recovered |
| 代表记录 | 按事件时间、记录 ID 排序；优先首条和末条，再选不同 outcome；仍有位置时补最早的其他记录 |

候选组只是为了避免低频成功信号被大量错误记录挤出，不证明整组正常或业务恢复；同组仍可能有失败记录。最多三个代表无法覆盖所有 outcome，也不能保证反例完整。需要时仍应按事件或关联 ID 单独查询。

## 3. 输出如何解释

- `aggregation_version=log-patterns-v1`：分组与选择规则版本。
- `query_id`、`result_digest`、`query`：对应准确的冻结查询结果；同一查询重复执行可能有不同 digest。
- `source_status`、`source_quality`：复制该查询原有的来源状态、覆盖、新鲜度、缺失原因和后端截断信息。
- `scanned_records`：本次输入记录条数。
- `omitted_groups`：因为十组上限未展示的分组数量。
- 顶层 `truncated`：来源被截断或存在未展示分组；只选三个代表记录属于固定展示规则，不改变其组计数。
- `observed_first_at/observed_last_at`：组内样本首末事件时间，不是故障开始/结束时间。
- `record_ids`：本 Run 原始记录引用，不是新证据 ID。

EMPTY 或 UNAVAILABLE 也生成保留来源状态的空摘要；零个分组不能解释为“没有异常”。摘要是派生视图，Report Guard 仍只接受原记录 ID，不能用摘要代替原始支持证据。

## 4. 执行边界

聚合不调用后端；所依附查询的实际请求仍使用原有预算。生成摘要不增加决策次数，也不重置无进展计数。摘要与证据的紧凑 JSON 大小共同受 Run 的 2MiB 限制，超限时整体不保存该次结果并进入汇总。

摘要和原始证据都提供隔离副本，Decider 修改自己的 View 不会覆写已保存内容。Harness 策略版本变为 `offline-harness-v2`；既有脚本格式不变，旧落盘产物不会被改写。

聚合本身保留全量有界证据。任务 0005 已新增独立[模型输入选择与预算](model-provider.md)，优先选日志代表记录，但尚未取得真实供应商成本或效果成绩，不能将聚合称为已验证的 Token 成本优化。

## 5. 验证入口

```bash
go test ./internal/app/investigation ./cmd/incident-investigate
make verify
make build
go mod verify
```

定向测试在 [patterns_test.go](../../internal/app/investigation/patterns_test.go)，包括分组、代表记录、多组省略、候选保留、缺失字段、来源质量、非法输入、稳定排序、引用和 Harness 集成。已有测试继续覆盖容量、无进展、取消与报告引用规则。
