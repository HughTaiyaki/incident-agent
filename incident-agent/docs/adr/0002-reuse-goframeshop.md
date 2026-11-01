# ADR-0002：复用已完成商城与遥测，取消重复实验场及自研 KV

状态：**已接受**。日期：2026-10-26。范围依据：用户明确确认商城完成，旧小型实验场废弃，不需要自研 KV。

## 背景与证据

商城固定提交 `385d3649b4f38f1db82e65aade57b00e10badb02` 包含交易、OTel、Prometheus/Loki/Jaeger 和故障实验实现，详见[代码基线](../integration/goframeshop-baseline.md)。Agent 仓库仍是健康检查骨架。历史文档的 Tempo、自建 checkout 与自研 KV 方向和当前需求冲突。

## 决定

1. 商城作为独立上游系统接入，不复制或改写它的交易服务。
2. Trace 使用商城已配置的 Jaeger；指标与日志使用 Prometheus/Loki。
3. 保留单 Investigator、确定性 Harness、受控查询、证据报告与恢复目标。
4. Agent 使用独立的持久 Redis；商城 Redis 关闭 AOF/RDB 且 `/data` 是 tmpfs，不能用于 Checkpoint。不开发或安排自研 KV。
5. Agent HTTP 层保留现有 net/http；GoFrame 是商城已使用的框架，不自动成为 Agent 的实现事实或强制依赖。
6. 首个接入切片先验证实际查询与语义，冻结脱敏样本；后续才实现完整调查和持久恢复。
7. 商城 T1–T10/C1 是交易协议实验，不直接等同于 Agent 诊断测试集或准确率。真值与调查数据必须隔离。

## 影响

节省重复商城开发，把工作集中在真实证据关联、调查效果和恢复边界。需要维护上游 SHA、字段映射和只读访问配置；现有短时故障、告警阈值、遥测缺失可能限制诊断，必须在评测中显式说明。

替代 [ADR-0001](0001-observable-lab-before-agent.md) 和[旧任务 0001](../../tasks/0001-observable-checkout.md)。当前任务为 [0002](../../tasks/0002-goframeshop-integration.md)。
