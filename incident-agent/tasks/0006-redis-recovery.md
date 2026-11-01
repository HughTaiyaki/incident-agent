# 任务 0006：独立 Redis 状态与恢复

状态：**本切片完成（2026-11-01），生产发布未验收。** 已交付原子协议、Go 客户端、Harness checkpoint 和可恢复 Worker。持久入口为 incident-worker，旧调查 CLI 仍为内存模式。

## 实施顺序

1. **已实现并测试** 原子协议：创建、领取、续租、调用前扣费、结果幂等提交、取消和终态；Redis 时间、epoch 与版本隔离。
2. **已实测** 独立 Redis：过期接管、旧 Worker 拒写、未知调用不退预算、重复提交、错误类型不产生部分写入、AOF 重启读回。
3. **已实现并测试（2026-11-01）** Go Redis 客户端及 Harness checkpoint：持久化调用审计和已保存决策，重建进度，不重置 deadline/预算。
4. **已实现并测试** Worker 启动/周期扫描、取消与到期无模型收尾、进程故障端到端测试。

## 本轮范围

第 1–4 项本切片已实现，存储脚本作为 Adapter 的固定协议。单 Run 的状态与操作结果存入一个有界 JSON string，ready/leases 为 ZSET；尚不提供 Incident 去重、SSE Stream 或生产部署编排。终态 7 天，活跃状态无 TTL。模型失败/未知结果保守保留全部预留；本切片不实现 usage 退款。

不修改商城代码、不连接商城缓存 Redis、不调用真实模型。测试自行启动 Unix socket 临时 Redis，端口关闭，测试后终止自己启动的进程。

验收和后续缺口见[阶段报告](../docs/reports/0006-redis-protocol.md)。

2026-11-01 增量：用户授权后新增 go-redis/v9 v9.20.1、显式配置客户端和集成测试；详情见[客户端改动报告](../docs/reports/0006-redis-client.md)。随后完成 checkpoint 与 Worker；见[Worker 改动报告](../docs/reports/0006-worker-recovery.md)及[使用指南](../docs/development/worker-recovery.md)。

本轮实施记录见[Worker 实施切片](0006-worker-plan.md)。UNKNOWN 不自动重试，持久模式保守不退模型预留；在线遥测、真实供应商、全局准入和工具链安全更新仍待验收。
