# 离线调查脚本

`observed-script.json` 是人工编写的开发脚本：读取[已有历史 Bundle](../telemetry/observed-20261028/bundle.json)中的六类查询结果，再提交带记录引用的 INCONCLUSIVE 报告。它用于验证 Harness 的范围、引用和保存流程，没有模型调用，不计入诊断准确率。

脚本只使用已有匿名关联 ID，不包含故障注入点、实验真值或原始查询 ID。原始历史 Bundle 与采集口径保持不变；运行结果的 upstream_sha 继续为 unknown。

操作方式与限制见[离线 Harness 指南](../../docs/development/offline-investigation.md)。新增故障脚本必须继续隔离评分真值，不能把答案包装成模型已取得的诊断结果。
