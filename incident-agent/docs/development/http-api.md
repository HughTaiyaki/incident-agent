# HTTP 调查 API

更新：2026-11-01。状态：**已实现，本地验证通过；未部署，不宣称生产就绪。** 范围见[任务 0010](../../tasks/0010-http-entry.md)，目标接口全集见[调查协议 §2](../architecture/investigation-design.md#2-输入接口与持久模型)。

## 1. 启动

`incident-api` 只创建和读取 Run，实际调查由 `incident-worker -mode work -live` 执行。两者必须使用相同的模型和后端配置，否则 Worker 取到任务时会因身份不符而停止。

除下表外，还需要 [Worker 指南](worker-recovery.md#1-最短使用路径)中的 Redis 变量和 `INCIDENT_MAX_ACTIVE_RUNS`，以及模型名称、地址、上下文窗口和三个后端地址。API 进程不调用模型或后端，不需要 `INCIDENT_MODEL_API_KEY`。

| 环境变量 | 含义 |
| --- | --- |
| INCIDENT_API_TOKEN | 至少 32 字节的操作员凭据，例如 `openssl rand -hex 32` 生成；不设置时不启用调查路由 |
| INCIDENT_API_BIND | 监听地址，默认 `127.0.0.1` |
| PORT | 端口，默认 `8080` |

不要把 Token 写入脚本、Git 或报告。API 本身是明文 HTTP，暴露到回环以外时必须由 TLS 代理终止，这部分尚未实现和验证。

## 2. 接口

除健康检查外都要 `Authorization: Bearer <token>`。接口只接受固定形状的字段，不接受查询表达式或地址。

| 接口 | 成功 | 说明 |
| --- | --- | --- |
| GET /healthz | 200 | 仅表示进程存活 |
| GET /readyz | 200；否则 503 | 不需要 Token，只返回原因代码 |
| POST /v1/incidents | 新建 202，已存在 200 | 返回 `run_id`、`created`；请求最大 16KiB |
| GET /v1/runs/{id} | 200 | `run-result-v1` 投影，字段见 [Worker 指南](worker-recovery.md#1-最短使用路径) |
| GET /v1/runs/{id}/report | 200 | 未完成 409 `NOT_READY`；取消或到期 409 `NO_REPORT` |
| GET /v1/runs/{id}/evidence/{query_id} | 200 | 冻结证据，query_id 来自投影的 `evidence`；**含真实业务 ID** |
| POST /v1/runs/{id}/cancel | 200 | 幂等，返回已持久状态；已完成的 Run 不会被改写 |

```json
{
  "alert_id": "alert-1",
  "service": "order-service",
  "environment": "local",
  "started_at": "2026-10-28T06:01:00Z",
  "summary": "下单失败率升高",
  "initial_window_minutes": 10
}
```

未知字段直接拒绝。service 必须在服务端白名单内，environment 为 local 或 experiment。调查窗口是 `started_at` 前 `initial_window_minutes`（默认 10，最大 50）到后 5 分钟，并裁到当前时刻。summary 最多 2,000 字符，含疑似凭据的内容被拒绝；它会原样发给模型。

## 3. 重试与幂等

同一 alert_id 只对应一个 Run，并发重试也一样。内容相同返回已有 Run（200），包括已结束的；内容不同返回 409 `IDEMPOTENCY_CONFLICT`。要重新调查，换一个 alert_id。

遇到 503 `STORE_UNAVAILABLE` 时写入可能已提交，用相同内容重试是安全的。

**幂等只持续到 Run 过期：** 终态保留 7 天，之后同一 alert_id 会新建 Run。调查协议的 30 天 Incident 标记和 episode 去重尚未实现。

## 4. 错误

响应形如 `{"code":"..."}`，不包含内部细节。

| 状态 | code | 处理 |
| --- | --- | --- |
| 400 | INVALID_REQUEST | 修正请求 |
| 401 | UNAUTHORIZED | 检查 Token |
| 404 | RUN_NOT_FOUND、EVIDENCE_NOT_FOUND | 从未创建，或已过保留期，二者无法区分 |
| 409 | IDEMPOTENCY_CONFLICT、NOT_READY、NO_REPORT | 见上文 |
| 413 / 415 | REQUEST_TOO_LARGE、UNSUPPORTED_MEDIA_TYPE | 修正请求 |
| 500 | STATE_CORRUPT、STATE_UNKNOWN_SCHEMA | 人工处理，不会据此生成报告 |
| 503 | OVERLOADED（带 Retry-After）、STORE_UNAVAILABLE | 稍后重试 |

## 5. 就绪状态

| reasons | 含义 |
| --- | --- |
| not_configured | 没有设置 Token，只运行健康骨架 |
| redis_unavailable | Redis 连不上或响应异常 |
| redis_misconfigured | AOF、fsync、noeviction 或 maxmemory 不符合要求 |
| worker_absent | 15 秒内没有 Worker 心跳 |

readyz 不检查模型和遥测后端，它们的故障会记录在各 Run 的证据和报告中。心跳说明 Worker 进程存活，不说明单个 Run 正在推进；`-once` 不发送心跳。

## 6. 验证与限制

```sh
go test ./internal/adapter/inbound/httpapi ./cmd/incident-api
make test-redis
```

- 只有一个共享凭据，没有角色、审计主体或限流；不具备多租户隔离。
- 没有 TLS、CORS 或 Cookie 会话，工作台接入时需要另行设计。
- evidence 接口返回真实业务 ID，持有 Token 即可读取。
