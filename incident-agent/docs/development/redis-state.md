# Redis 原子状态协议 v1

适用：开发存储 Adapter、验证租约和调用提交。状态：脚本、Go 客户端及隔离集成测试已实现，**已接入新的 incident-worker；旧内存 CLI 的既有 Run 不能导入恢复**。本轮范围见[任务 0006](../../tasks/0006-redis-recovery.md)，完整目标见[执行协议 §7](../architecture/investigation-design.md#7-redis-与调度恢复)。

## 1. 运行测试

安装本机 `redis-server`、`redis-cli` 后，在项目目录运行：

```sh
make test-redis
```

测试启动自己的临时 Redis：`--port 0`，仅临时 Unix socket（0700），独立数据目录、AOF everysec、noeviction。不会读取 Redis 环境变量或连接 6379，不要求停用商城。测试清理只针对自身进程和目录。默认 `make verify` 不启动 Redis；集成测试使用 `redisintegration` build tag，缺少可执行程序时失败。

## 2. 调用契约

[脚本](../../internal/adapter/outbound/redisstate/state.lua)通过 Go embed 暴露为 `redisstate.Script`。`EVAL` 传三个互异 Key：当前 Run 的 state string、公共 ready ZSET、公共 leases ZSET；一个参数：JSON 命令。适配层负责固定命名空间和 Run ID 到 Key 的映射，不能由模型或客户端直接提交 Key/脚本/命令。

响应为 JSON：`code`、成功时的 `state` 和 Redis 当前时间 `now`（Unix 毫秒，校验命令格式前失败时为 0）。存储状态 schema=1，与当前调查 Run 的 JSON schema 不同；后续必须显式映射，不可直接反序列化为调查 Run。运维和 CLI 读取使用[结果投影](worker-recovery.md#1-最短使用路径)，不直接消费 `state`。

state 无法解析或 ID 不符返回 CORRUPT，schema 不是 1 返回 UNKNOWN_SCHEMA；两者都不写入，create 和 cancel 也不覆盖。

| action | 必要参数（均另有 id） | 行为 |
| --- | --- | --- |
| create | request（冻结请求 JSON 字符串，≤16KiB）、deadline（Unix 毫秒）、max_active（1–1024） | deadline 必须在 Redis 当前时间后 180 秒内；同 ID/请求/deadline 返回 EXISTS，不延长窗口；已存在的 ID 不做容量判断。新 ID 活跃数（ready＋leases 基数）达上限返回 OVER_CAPACITY；`used_memory+(活跃数+1)×8MiB` 超 maxmemory 或 maxmemory 为 0 返回 OVER_MEMORY；拒绝时不写入 |
| read | 无 | 返回冻结状态；不存在为 NOT_FOUND，不自动新建 |
| claim | owner | 排队或租约到期才可领取；epoch 和 version 递增，15 秒租约；未完成操作标 UNKNOWN。state 不存在时返回 NOT_FOUND 并清理残留索引条目，释放其占用的名额 |
| renew | owner、epoch | 必须仍持有有效租约；只续租，不改业务 version；调用方每 5 秒续租 |
| begin | owner、epoch、version、operation、kind、cost、input | 原子预留并记录 IN_FLIGHT；tool 消耗 0 个实际请求，query 消耗 1 个；input 为已校验的冻结调用参数字符串（≤64KiB） |
| commit | owner、epoch、version、operation、result，可选 settled | 当前 epoch 的 IN_FLIGHT 才能提交；结果与状态原子保存。settled 为模型调用的实际用量（1 到预留额），差额退回；缺省保持全额，非模型操作传入返回 INVALID |
| checkpoint | owner、epoch、version、checkpoint（≤3MiB） | 原子保存当前执行快照 |
| cancel | 无 | READY 立即取消；RUNNING 置标志并改变 version，阻止后续调用 |
| complete | owner、epoch、version、report | 正常完成保存报告；取消或到期仅确定性终结，丢弃传入报告 |

ID、owner、operation 为 1–128 字节字母数字、下划线或连字符。所有时间判断使用 Redis TIME。active 状态无 TTL，终态 7 天并移出索引。调用方扫描 ready 和过期 leases，再逐个 Claim；索引只是候选，Claim 才是所有权判定。

## 3. 故障与边界

- Begin 的 OK 才允许首次发起外部请求；ALREADY 表示之前已预留，**不能据此再发请求**。重复 operation 的 kind/cost/input 不同为 CONFLICT。断连时先读回，不盲目重试外部调用。
- 接管递增 epoch；旧 Worker 的 Renew/Commit/Complete 被 FENCED。UNKNOWN 仍消耗预算，不能提交迟到结果。当前不自动重试 UNKNOWN；后续 Harness 必须定义新 attempt 及次数限制。
- Commit 重放同结果返回 ALREADY，不重复增加 version/结果字节；不同结果返回 CONFLICT。终态报告不可覆盖。
- 存储只做保守额度：模型合计 24,000、最多 12 次，查询最多 16 次，每个实际请求扣 1。未提交或 UNKNOWN 的调用不退还；报告预留、阶段/决策次数、输入估算仍须由后续 Harness 在 Begin 前校验，不能只依赖存储总额。
- 操作结果合计 ≤2MiB，报告 ≤64KiB，checkpoint ≤3MiB，编码后的完整状态 ≤7MiB；参数和状态先编码再写。Key 类型在第一次写入前检查，避免 WRONGTYPE 导致可预见的部分更新。OOM、主机崩溃等仍不是数据库回滚保证。
- result/report/request/input 是受信任应用写入的字符串；脚本不验证证据引用、脱敏或报告语义。接线时仍必须调用现有 Guard，不能把脚本当公共 API。
- 当前同一 JSON string 保存状态、checkpoint、调用审计和操作结果，尚无独立 Evidence、Incident 去重和 events Stream。不要据此实现 SSE 或宣称与完整 §7 全部兼容。

AOF everysec 的重启读回测试不代表主机故障零丢失；此次是正常关闭后的持久重载和模拟租约过期接管，脚本阶段本身不含 Worker 故障；后续 Worker 集成测试已覆盖模型提交后子进程直接退出及真实租约到期接管。Lua 执行会阻塞其他 Redis 活动，状态/结果必须有界；参见 [Redis 官方脚本说明](https://redis.io/docs/latest/develop/programmability/eval-intro/)。

## 4. Worker 接线

已接入 checkpoint、保守持久调用账目、续租、候选扫描和无模型收尾；运行入口及恢复窗口见[Worker 指南](worker-recovery.md)。旧内存 CLI 不自动改为持久模式。当前不自动重试 UNKNOWN，同一操作不会因确认丢失而重发；完整设计中的有限重试留待后续验收。

## 5. Go 客户端（2026-11-01）

[client.go](../../internal/adapter/outbound/redisstate/client.go)提供 `New(Config)`、`Execute(ctx, Command)` 和 `Close()`；构造时不联网。配置必须显式指定 Network、Address、Namespace，不读取环境变量，也不默认连接 localhost:6379。独立 Redis 仍由部署者提供，命名空间不能替代独立实例。

- 固定 `github.com/redis/go-redis/v9 v9.20.1`，校验和在 go.sum；间接运行依赖为 xxhash/v2 v2.3.0、atomic v1.11.0。初次接入时选择兼容 Go 1.25 的版本；任务 0007 独立升级工具链至 Go 1.26.10，未升级客户端。发布说明见[官方 v9.20.1](https://github.com/redis/go-redis/releases/tag/v9.20.1)。
- 支持绝对路径 Unix socket；TCP 明文仅允许回环 IP，远程必须配置校验证书的 TLS，最低 TLS 1.2。禁用 InsecureSkipVerify。
- 固定 RESP2、两条活动连接上限、连接 1 秒、读写 2 秒、单次操作最长 3 秒；遵守调用者更短的 deadline。
- 关闭自动命令重试、维护通知和 CLIENT SETINFO。每次发送固定 EVAL，不使用 EVALSHA fallback；Key 由内部 namespace/id 构造。
- `Reply.Code` 保留 OK/ALREADY/CONFLICT/FENCED 等业务结果，调用方必须检查；不能仅以 Go error==nil 判定允许外部调用。
- 发出后出错返回 ErrUnavailable；可能已提交，不包含原始 Redis 错误正文或凭据。deadline/cancel 可通过 errors.Is 检查。下一步先 read 核对状态；传入前已取消则直接返回 context 错误。
- `Heartbeat` 写入 15 秒过期的 `<namespace>:worker:heartbeat`，`Readiness` 返回固定原因代码，供 [HTTP API](http-api.md) 使用。
- Worker 已增加扫描与续租；客户端本身仍不自动重试。持久 CLI 的配置和完整限制见 Worker 指南。

客户端集成测试复用临时 Redis，覆盖结果保存、UNKNOWN 接管、跨命名空间隔离、超时结果不确定及后续读取、重建客户端后的 AOF 读回。`make test-redis` 同时运行脚本和客户端测试。
