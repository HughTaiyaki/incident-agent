# Harness checkpoint 与 Worker 恢复

更新：2026-11-01。状态：**已接入，独立 Redis 与本地模拟模型验证通过；未生产发布。** 旧 `incident-model` / `incident-investigate` 仍是内存执行；持久执行使用新的 `incident-worker`。范围见[任务 0006](../../tasks/0006-redis-recovery.md)，原子契约见[Redis 指南](redis-state.md)。

## 1. 最短使用路径

需要自行提供独立 Redis 7+，配置 `appendonly yes`、`appendfsync everysec`、`maxmemory-policy noeviction` 和非零 `maxmemory`。不要使用商城缓存 Redis。enqueue/work 启动会通过 CONFIG GET 检查这四项；ACL 不允许检查时拒绝运行。配置检查不证明服务器与商城物理独立，地址仍由操作员核对。

```sh
make build
export INCIDENT_REDIS_NETWORK=unix
export INCIDENT_REDIS_ADDRESS=/absolute/path/to/agent-redis.sock
export INCIDENT_REDIS_NAMESPACE=incidentdev

# 模型变量和密钥配置方式见 Provider 指南；不要将密钥写入任务文件。
export INCIDENT_MODEL_ENDPOINT=https://your-provider.example/v1/chat/completions
export INCIDENT_MODEL_NAME=your-configured-model
export INCIDENT_MODEL_CONTEXT_WINDOW=32000
export INCIDENT_MAX_ACTIVE_RUNS=2   # 占位；取值见下文“准入与过载”

./bin/incident-worker -mode enqueue -id investigation-001 \
  -request testdata/investigation/observed-request.json \
  -fixture testdata/telemetry/observed-20261028/bundle.json
./bin/incident-worker -mode work \
  -fixture testdata/telemetry/observed-20261028/bundle.json
```

以上地址/型号是占位，必须替换；密钥使用 `INCIDENT_MODEL_API_KEY`。enqueue 不调用模型；work 会显式调用已配置模型。任务冻结 Request、模型名、上下文窗口、遥测来源身份和 Endpoint hash；恢复时配置不匹配会停止，不替换模型或数据继续执行。

### 在线遥测模式

把 `-fixture <路径>` 换成 `-live`，二者必须且只能选一个。Fixture 和在线 Run 不能由同一个 Worker 进程混合处理，配置不匹配时 Worker 会停止。

```sh
export INCIDENT_PROMETHEUS_URL=http://127.0.0.1:9090   # 占位，必须显式设置
export INCIDENT_LOKI_URL=http://127.0.0.1:3100
export INCIDENT_JAEGER_URL=http://127.0.0.1:16686

./bin/incident-worker -mode enqueue -id investigation-002 -live \
  -request <请求文件>
./bin/incident-worker -mode work -live
```

`-live` 只发固定模板的只读 GET，没有默认地址。**work 会把遥测记录发给配置的模型，可能产生费用。** 发送前，订单、操作、事件、支付和消息 ID 会换成按 Run 加盐的假名；Trace/Span ID、服务名、时间和指标值保持原样，Request.Summary 原样发送。假名化不是匿名化，外发前需确认可接受。

后端连接没有认证头，也未拒绝非回环 HTTP，只适合受控环境。

| 情况 | 行为 |
| --- | --- |
| 后端错误或单次超时（每次 5 秒） | 保存失败证据，Run 继续，Worker 不退出 |
| 模型错误或单次超时（30 秒） | 记录失败，Run 以 INCONCLUSIVE 收尾，Worker 不退出 |
| 429/502/503/504 | 最多重试一次，每次请求都先持久扣费 |
| 查询结果超出存储额度 | 证据记 `RESULT_TOO_LARGE`，不保存记录，Run 继续 |
| Redis 错误 | Worker 退出，由监督进程重启，见第 3 节 |

`-mode evidence` 输出含真实业务 ID 的证据，只应留在操作员本地。需要留存或分享时用：

```sh
./bin/incident-worker -mode export -id investigation-002 -out data/run-002.json
```

export 输出匿名 Bundle，权限 0600，不覆盖已有文件，只有确认运行版本时才传 `-upstream-sha`。匿名化会重算记录 ID，因此报告里的 evidence_ids 不能和导出内容直接对应。

支持 Redis username/password 环境变量。当前 CLI 支持 Unix socket 或回环 IP TCP，未暴露远程 TLS 参数；Redis Go Adapter 自身支持 TLS。不默认选地址，不修改现有 Redis 配置，也不自动启动 Redis。

```sh
./bin/incident-worker -mode status -id investigation-001
./bin/incident-worker -mode evidence -id investigation-001
./bin/incident-worker -mode cancel -id investigation-001
```

status/evidence/cancel 仅需 Redis 配置，不再输出内部 `state`/checkpoint。status 与 cancel 返回版本化投影 `run-result-v1`：`status`（READY/RUNNING/COMPLETED/CANCELLED/DEADLINE，只取自 state.status）、`terminal`、`cancel_requested`、`overdue`、时间与保留期限、`budget`、不含输入和结果的 `operations`（含 UNKNOWN；后端请求的扣费记录显示为 CHARGED，不代表调用结果）、证据摘要 `evidence`（query_id、digest、记录数等）。**只有 COMPLETED 才带 `report`**；取消/到期即便 checkpoint 里留有完成报告也不展示。

evidence 模式返回 `run-evidence-v1`，含冻结查询结果和日志摘要，供核对报告引用。Run 未 COMPLETED 时这些证据来自最后已确认的 checkpoint，不是完整集合，需结合其中的 `status` 判断。投影是当前 CLI 契约，HTTP 接口待 P3 定义。

| 情况 | 退出码与错误 | 含义 |
| --- | --- | --- |
| 正常（含取消已是终态） | 0，stdout 为 JSON | cancel 在完成之后返回已完成结果，不改写 |
| Run 不存在 | 1，`run not found: never created or past 7-day retention` | 保留期后不留标记，**无法区分从未创建和已过期**，也不伪造报告 |
| 状态损坏 | 1，`run state corrupt` | JSON 无效、ID 不符、COMPLETED 缺报告或最终 checkpoint、未知 status；不据此推断结果 |
| 未知版本 | 1，`run state has unknown schema version` | 状态/Job/checkpoint 版本不受支持，不覆盖；升级工具或人工处理 |
| 过载拒绝（enqueue） | 3，`OVER_CAPACITY` 或 `OVER_MEMORY` | 未写入任何状态，可稍后用同一 ID 重试 |

`overdue` 表示 deadline 已过但尚未收尾，判断使用 Redis 时间；此时 Run 仍未终态并继续占用名额，等待 Worker 确定性收尾。

### 准入与过载

enqueue 必须显式设置 `INCIDENT_MAX_ACTIVE_RUNS`（1–1024），**没有默认值**；缺失时不写入。活跃 Run 指排队和运行中的 Run，全局名额在 Redis create 脚本内原子判定，取 ready 与 leases 索引基数之和，因此领取和接管不改变计数，重复 ID 先返回 EXISTS/CONFLICT 不占额。名额在进入终态时释放：运行中取消和到期都要等 Worker 收尾后才释放，因为之前可能仍有在途外部调用。

| 拒绝码 | 条件 | 操作员动作 |
| --- | --- | --- |
| `OVER_CAPACITY` | 活跃数 ≥ 配置上限 | 等待完成，或确认 Worker 在运行 |
| `OVER_MEMORY` | `used_memory + (活跃数+1)×8MiB > maxmemory`，或 maxmemory 为 0 | 增加内存或等待终态过期；Run 保留 7 天仍占内存 |

8MiB 由单 Run 7MiB 编码上限加索引与分配余量得出，已用内存会被重复计入，属于保守预留，**不是压测结果**。取值可从 `maxmemory ÷ 8MiB` 得到内存允许的上限，再按排队时间收紧：Worker 串行且 deadline 从 enqueue 起算，排得过长的 Run 只会以 DEADLINE 结束。尚无容量压测，不提供生产推荐值。Worker 全部停止时，活跃 Run 无人收尾，新 Run 会一直被拒绝；这是有意的保守行为。

每 Run deadline 从 enqueue 起固定 180 秒，**排队和进程停机时间也消耗窗口**。已存在 ID 不重新排队；新调查用新 ID。Run 终态保留 7 天，过期后 ID 可再次创建；尚无长期 Incident 幂等索引，不承诺永久去重。

## 2. 恢复机制

Harness checkpoint 保存 Run、next_step、phase、feedback、stop_reason、重复查询计数、去重集合、无进展计数、报告拒绝数和证据字节数。步骤开始前持久化，复用原有范围、次数和 Report Guard；恢复不重置 deadline、决策槽或后端预算。

步骤 checkpoint 与外部调用之间通过 operations 衔接：

| 窗口 | 恢复行为 |
| --- | --- |
| Begin 前退出 | 没有预留与调用记录，恢复后才允许开始 |
| Begin 成功但调用结果未提交 | 接管后 UNKNOWN；模型保守 INCONCLUSIVE 收尾，查询记为失败证据；不自动重发该操作 |
| 模型结果已提交、查询尚未开始 | 复用已保存且通过结构/范围检查的决策，不重新问模型 |
| 查询结果提交后、checkpoint 未推进 | 读取冻结结果，经 Harness 校验后继续，不重查后端 |
| 下一步骤 checkpoint 已保存 | 从保存的 step 和计数继续 |
| Complete 已提交、确认响应丢失 | Run 已终态，不被候选扫描重新执行；通过 status 核查 |
| 旧 Worker 返回 | epoch/owner/version/lease 检查拒绝提交 |

模型原始拒绝输出不持久，保留不含原文的拒绝占位决策，仍走原有拒绝/修复计数。持久模式按预留额保守扣费，即使 usage 较少也不退款；usage 超预留会阻止后续模型调用。实际 usage 与预留均可核查，仍不保证供应商账单硬上限。

查询操作 `tool` 不计实际网络请求；每次 `Budget.Reserve()`（包括 Adapter 重试）先持久化独立 `query` 请求扣费。Fixture 消耗零后端请求。在线模式已用本地模拟后端验证此契约，真实后端尚未验收。

## 3. 生命周期与异常

Worker 串行处理 Run（并发 1，小于设计上限 2），启动及每秒扫描 ready/expired leases，15 秒租约、5 秒续租。索引只提供候选，Claim 才授予所有权。运行中取消可由下一次 Begin 或续租发现；在途请求只能尽力取消，不能宣称撤销供应商计费。

SIGINT/SIGTERM 使进程停止新调用并退出，活跃 Run 留给租约到期后的接管；**进程退出不写成用户取消**。取消/到期任务接管时不创建 Provider，以确定性状态收尾。Redis 错误或配置不匹配使 Worker 返回非零，需监督进程报告/重启；当前不在内部无限重试。`-once` 处理一次候选扫描后退出，适合验证，不会等到下一次租约到期。

每个 API 操作只有一次 EVAL，没有自动写重试。遇到未确认提交先停止外部工作；Worker 收尾前 read 确认状态，重启后重新读取 checkpoint/operations。若已提交终态，即便原进程返回非零，status 仍是事实来源。

当前容量：调用结果合计 2MiB、checkpoint 3MiB、完整编码状态 7MiB、请求/响应编码上限 8MiB；checkpoint 中的实际证据仍受 Harness 2MiB 限制。40 条操作上限对应最多 12 次模型、12 次工具以及 16 次实际请求。容量拒绝停止调用并保留可恢复状态；全局准入见上文“准入与过载”。新增快照冗余会增加 Redis 内存需求，未做吞吐/容量基准。

损坏或未知版本的活跃 Run 不会被自动丢弃：它继续占用名额，Worker 扫描到时返回带 Run ID 和响应码的非零错误并停止，修复前会阻塞其他 Run，需操作员人工处理。没有状态的残留索引条目在下次 Claim 时清理并释放名额。

## 4. 验证与生产限制

```sh
make verify
make test-redis
make build
```

`test-redis` 自建临时 Unix socket Redis（TCP=0），含模型提交后的进程退出与真实租约到期接管，因此约需两到三分钟。覆盖幂等结果复用、UNKNOWN 不退款/不重发、取消/到期、实际请求预留、续租、进程退出、配置检查、并发准入、名额回收、接管占额、内存预留、迟到完成、损坏状态和编译后 CLI 的 enqueue/work/status/evidence/cancel；本地 HTTP 模型仅是程序协议验证。

完整结果见[Worker 改动报告](../reports/0006-worker-recovery.md)和[容量与终态报告](../reports/0008-capacity-and-results.md)。在线模式仅通过[本地模拟验证](../reports/0009-online-investigation.md)。尚未验收真实供应商、真实遥测后端、HTTP 入口认证、Incident 去重、容量压测、SSE 或高可用。任务 0007 已升级至 Go 1.26.10，普通与 Redis 标签的已知可达漏洞扫描通过，见[工程基线报告](../reports/0007-production-baseline.md)。Redis/主机故障不保证零丢失；不能据此宣称整个项目生产就绪。
