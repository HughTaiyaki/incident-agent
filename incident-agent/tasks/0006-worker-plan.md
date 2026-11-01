# 0006 Worker 接线实施切片

2026-11-01，已获用户明确实施授权；本切片已实现并验证，结果见[改动报告](../docs/reports/0006-worker-recovery.md)。保留既有离线 Execute 行为，新加 checkpoint 入口与持久 Worker。

1. 将 Harness 循环变量显式保存为版本化 checkpoint，步骤开始保存阶段与原 deadline；复用 Guard 和计数逻辑。
2. 将存储 DTO 放入应用端口，Redis 增加 checkpoint 原子写与候选扫描。调用结果和 checkpoint 之间的间隙由幂等操作记录覆盖。
3. ModelDecider 增加调用前预留钩子与保守预算恢复。Worker 保存经过结构解码的决策，拒绝的原文不持久。查询每次实际 Reserve 均先写 Redis；冻结查询也记录操作，但不虚增后端请求数。
4. 单 Worker 顺序执行，启动及周期扫描 ready/过期 leases；5 秒续租，所有权失效停止调用；取消/到期无模型收尾。UNKNOWN 不自动重发，保守 INCONCLUSIVE。
5. 增加显式 Redis Worker CLI，冻结 Fixture 与模型配置绑定到任务；真实 Redis 验证 checkpoint、提交后响应丢失、旧 owner 隔离、恢复不重复模型/查询、取消与预算。

进程退出保留活跃 Run 等待接管，不能把进程退出写成用户取消。Redis/主机故障不承诺零丢失；工具链安全升级仍是独立发布阻断项。
