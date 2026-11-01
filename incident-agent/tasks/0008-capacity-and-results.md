# 任务 0008：Run 容量与终态查询

日期：2026-11-01。状态：工程验收完成，本机与[远端 CI](https://github.com/HughTaiyaki/incident-agent/actions/workflows/agent-release-check.yml) 均通过。依赖[0007 工程基线](0007-production-baseline.md)，对应[路线 P1](../docs/development/production-roadmap.md)。

## 目标

- 增加全局活跃 Run 准入上限，在 Redis 原子创建中校验；终态回收名额，接管不重复占额，重复 ID 不增加容量。
- 定义明确的拒绝码、部署配置和过载行为；保留在途收尾空间，不因 Redis 达到 noeviction 内存阈值才被动失败。
- 增加版本化终态查询投影，从 state.status 决定结果；取消/到期不展示中间 checkpoint 的完成报告。
- 对损坏/过期/未知版本状态给出明确错误，不伪造报告；保留操作预算和证据的可核查入口。
- 以并发创建、取消/到期回收、接管、迟到完成、损坏快照测试验收；同步 Worker status CLI 和使用指南。

不引入消息队列、HTTP 公网入口或多租户；先完成单部署场景。容量具体默认值需要结合每 Run 7MiB 编码上限和部署内存预算确定，不能把未压测数字写成生产推荐值。

## 实施决定

- 活跃数直接取 ready 与 leases 索引基数之和，不另设计数器，避免计数与索引漂移。
- 上限由 `INCIDENT_MAX_ACTIVE_RUNS` 显式提供，没有默认值。内存按每活跃 Run 8MiB 预留，并要求 maxmemory 非零。拒绝码、退出码和取值方法见 [Worker 指南](../docs/development/worker-recovery.md#准入与过载)。
- status/cancel 改为 `run-result-v1` 投影，新增 evidence 模式。这是对原 CLI 输出的不兼容变更。

## 验收结果与交接

2026-11-01 本机 `make release-check` 退出 0。并发创建、名额回收、接管、内存预留、迟到完成、损坏状态和 CLI 全流程均有测试，结果与文件清单见[改动报告](../docs/reports/0008-capacity-and-results.md)。远端 ubuntu-24.04 同样通过（提交 `42c8f3f`）。没有做容量压测。

遗留：

- 保留期过后无法区分“已过期”和“从未创建”，统一报 NOT_FOUND 并说明两种可能；明确的过期状态需要长期标记，随 Incident 去重（P3）一起考虑。
- 损坏或未知版本的活跃 Run 会让 Worker 报错停止，修复前阻塞其他 Run；隔离策略留待 P4 运维切片评估。

下一阶段是[任务 0009](0009-online-investigation.md)。
