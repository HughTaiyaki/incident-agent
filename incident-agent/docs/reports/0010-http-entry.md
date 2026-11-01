# 阶段 0010：HTTP 调查入口改动报告

日期：2026-11-01。对应[任务 0010](../../tasks/0010-http-entry.md)与[生产路线 P3](../development/production-roadmap.md)。同轮还修复了持久模式的 Token 结算，并在真实遥测后端上运行了任务 0009 的流程。代码随提交 `28f474e`（结算）和 `75c2f49`（入口）入库，[远端 CI](https://github.com/HughTaiyaki/incident-agent/actions/runs/41315442725) 在 ubuntu-24.04 通过。

## 修改前后

| 方面 | 之前 | 现在 |
| --- | --- | --- |
| HTTP 入口 | 只有健康检查 | 带 Token 校验的提交、查询、报告、证据、取消 |
| readyz | 固定返回 ready | 检查 Redis 配置/可用性和 Worker 心跳；未配置时 503 |
| 监听地址 | 所有网卡 | 默认回环，`INCIDENT_API_BIND` 可改 |
| 重复提交 | 无入口级幂等 | alert_id 派生 Run ID，内容摘要判断是否同一请求 |
| 持久模式 Token | 提交后也不退还预留 | 有合法 usage 时按实际用量结算，其余全额计费 |
| 投影字段 | `reserved_tokens` | `charged_tokens`（不兼容的字段改名） |

**Token 结算的原因：** 真实后端运行中，4 次模型调用就耗尽 24,000 预算，只完成 3 次查询，因为按字节估算的预留偏高且从不退还。这与[调查协议 §3](../architecture/investigation-design.md#3-状态停止与预算)和内存模式都不一致。修复后，未知或失败的调用仍全额计费，提交确认丢失时也保持全额，因此仍是保守方向。

## 文件与职责

| 文件 | 改动 |
| --- | --- |
| [router.go](../../internal/adapter/inbound/httpapi/router.go) | 路由、认证、错误映射 |
| [submit.go](../../internal/app/worker/submit.go) | 幂等提交、窗口计算、配置身份 |
| [worker.go](../../internal/app/worker/worker.go)、[calls.go](../../internal/app/worker/calls.go) | 心跳；结算 |
| [state.lua](../../internal/adapter/outbound/redisstate/state.lua)、[client.go](../../internal/adapter/outbound/redisstate/client.go) | commit 结算；心跳和 Readiness |
| [main.go](../../cmd/incident-api/main.go)、[app.go](../../internal/app/app.go) | 配置组装、默认回环；服务器超时 |
| [router_test.go](../../internal/adapter/inbound/httpapi/router_test.go)、[main_test.go](../../cmd/incident-api/main_test.go) | 认证、校验、错误映射、配置失败路径 |
| [api_integration_test.go](../../internal/adapter/outbound/redisstate/api_integration_test.go) | 真实 Redis 上的并发幂等、过载、生命周期、readyz、结算 |

没有新增依赖，没有修改商城，状态 schema 不变。

## 验证记录

环境：macOS arm64，Go 1.26.10，独立 Redis 8.0.2。

**工程测试（模拟后端和模型）：**

| 场景 | 结果 |
| --- | --- |
| 5 条路由缺 Token、错 Token、前缀 Token、Basic | 均 401，存储调用 0 次 |
| 非法字段、服务、环境、窗口、含凭据摘要、超大请求 | 400/413/415，存储调用 0 次 |
| 8 个并发相同提交 | 恰好 1 个 202，其余 200，同一 run_id，活跃数 1 |
| 同 alert_id 不同内容 | 409，不新建 |
| 名额已满 | 新 alert_id 503 带 Retry-After；已接受的重试仍 200 |
| 取消 | 排队取消后 report 返回 NO_REPORT；已完成的 Run 不被改写 |
| readyz | 无 Worker、Worker 运行、Redis 配置错误、心跳消失、Redis 停止，结果都正确 |
| 结算 | 超预留或负值拒绝；重放不重复退还；未知调用不退还；非模型操作不能结算 |

完整 `make release-check` 退出 0：格式、模块校验、vet、race、lint（0 issues）、Redis 回归 175.6s、五个命令构建，普通与 Redis 标签漏洞扫描均为 `No vulnerabilities found.`。之后只改了文档。

**真实遥测后端（本机 Prometheus/Loki/Jaeger，模型为回环脚本）：** 编译后的 `incident-api` 和 `incident-worker`，临时独立 Redis，历史窗口 2026-10-28。没有数据外发，也没有费用。

| 项目 | 结果 |
| --- | --- |
| 健康与认证 | healthz 200；无 Worker 时 readyz 503 `worker_absent`；无 Token 401；Worker 启动后 200 |
| 提交与重试 | 首次新建，重试 `created:false` 且同一 run_id；完成前 report 409 |
| 查询 | logs 4 条、metrics 150 和 25 条、traces 4 条、trace 7 条，全部 SUCCEEDED，共 5 次后端请求 |
| 结果 | COMPLETED，6 次模型调用，stop_reason=REPORT_ACCEPTED，report 与 evidence 均 200 |
| 其他 | Worker 正常退出；API 日志不含 Token；未留下进程 |

脚本模型的 usage 是固定值，所以这里的 Token 数不代表真实开销。运行产物在 `data/online-real-backend-20261101/`，属于 git 忽略的本地文件。

## 未验证与限制

- 真实供应商仍未联调，模型诊断质量和实际 Token 开销未知。
- 遮蔽只在模拟数据上被实际触发：逐字段核查过的运行没有业务 ID 字段，含 trace 查询的最后一次运行没有做这项核查。
- 只有一个共享 Token，没有 TLS、角色、限流或审计主体；未在部署环境验证。
- 幂等在终态保留 7 天后失效；没有告警入口、episode 去重或 SSE。
- API 与 Worker 配置不一致时，Worker 会在取到任务时停止，这是有意的保守行为，但会阻塞其他 Run，需要 P4 的运维说明。
- Worker 仍可能被单个损坏 Run 阻塞，见[任务 0008](../../tasks/0008-capacity-and-results.md)的遗留项。
