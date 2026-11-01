# 任务 0010：受控 HTTP 调查入口

日期：2026-11-01。状态：工程切片已实现，本机验证与[远端 CI](https://github.com/HughTaiyaki/incident-agent/actions/runs/41315442725) 均通过，部署入口待 P4。对应[路线 P3](../docs/development/production-roadmap.md)。

[任务 0009](0009-online-investigation.md)的真实供应商联调当时要等外部配置，而本任务不依赖它，所以并行推进。P2 的门槛不因此放宽。

## 范围

- 手动提交 Incident、查询 Run、读取报告和证据、取消，全部要求身份校验。
- 按 alert_id 幂等：重试不重复建 Run，同 ID 不同内容明确拒绝。
- readyz 反映 Redis 配置和 Worker 实际可用性。
- 不暴露 Redis 命令、Shell、SQL 或任意查询；范围由服务端模板决定。

不包含：Prometheus 告警入口、episode 去重、30 天 Incident 标记、Incident 列表、SSE、反馈、新建恢复验证 Run、工作台和 Cookie 会话。这些仍是调查协议的目标设计。

## 实施决定

- 身份校验用单个 Bearer Token，匹配当前单租户范围，按摘要定长比较。没有 Token 时不注册调查路由，readyz 报 `not_configured`。
- Run ID 由 alert_id 派生，并发重试落在同一 Run；用提交内容摘要判断是否为同一请求，因为窗口终点被裁到当前时刻，每次重试的冻结请求可能不同。
- 默认只监听回环地址，因为明文 HTTP 会传输 Token；TLS 终止和认证代理归 P4。
- Worker 心跳独立于 Run 处理，单个 Run 可持续数分钟，不能让它显得像 Worker 已退出。

## 验收门槛

| 门槛 | 状态 |
| --- | --- |
| 未授权请求被拒绝且不访问存储 | 通过 |
| 服务、环境、窗口、摘要和字段范围校验 | 通过 |
| 重复提交不重复建 Run，含并发和终态后重试 | 通过 |
| readyz 反映 Redis 配置/可用性和 Worker 存活 | 通过 |
| 不暴露底层命令和内部状态 | 通过 |
| 实际部署环境的入口与认证 | **未验证**，归 P4 |

结果与文件清单见[改动报告](../docs/reports/0010-http-entry.md)，使用方法见 [API 指南](../docs/development/http-api.md)。
