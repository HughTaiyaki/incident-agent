# 值班手册

更新：2026-11-02。下面的命令假设在项目目录执行。先定义：

```bash
alias dc='docker compose -f deploy/compose.yaml --env-file deploy/.env'
alias rcli='dc exec redis redis-cli -s /run/redis/redis.sock'
```

**先看这里：** `curl http://127.0.0.1:8080/readyz` 给出固定原因码；`dc ps` 看容器状态；`dc logs --tail 100 worker` 看 Worker。

| readyz 原因 | 含义 | 跳转 |
| --- | --- | --- |
| `redis_unavailable` | 连不上 Redis | [Redis 不可用](#redis-不可用) |
| `redis_misconfigured` | AOF、淘汰策略或 maxmemory 不符合要求 | [Redis 不可用](#redis-不可用) |
| `worker_absent` | 15 秒内没有心跳 | [Worker 缺失](#worker-缺失) |
| `not_configured` | API 没有配置 Token | 检查 `deploy/secrets/` 和部署 |

## API 不可用

`dc ps api`，再 `dc logs --tail 50 api`。配置错误时进程启动即退出，日志说明缺哪一项；日志不含 Token。已接受的 Run 不依赖 API，Worker 会继续执行。

## Redis 不可用

`dc ps redis`、`dc logs --tail 50 redis`。Worker 会以非零退出并被自动重启，这是预期行为；Redis 恢复前新 Run 返回 503。

- **内存不足或 OOM：** 先看[内存接近上限](#内存接近上限)。
- **AOF 损坏导致起不来：** 不要直接运行 `redis-check-aof --fix`，它会截断数据。先备份数据卷，再按[备份与恢复](#备份与恢复)判断是修复还是恢复。
- **配置不符合（`redis_misconfigured`）：** 有人改过运行时配置。`dc restart redis` 恢复 Compose 中的参数。

`deadline` 不因停机暂停，超过 180 秒的 Run 会以 DEADLINE 收尾，报告不展示。用相同 `alert_id` 重试只会拿到这个旧结果，需要重新调查时换一个 `alert_id`。

## Worker 缺失

`dc ps worker`、`dc logs --tail 50 worker`。容器会自动重启；反复重启时看日志最后的错误。

| 日志 | 处理 |
| --- | --- |
| `redis state: ...` | 看 [Redis 不可用](#redis-不可用) |
| 模型或配置校验错误 | 检查 `deploy/.env` 和密钥文件权限 |

Worker 恢复后，到期的租约会被接管。接管时状态不明的模型调用不会重发，该 Run 通常以 INCONCLUSIVE 收尾，这是为了不重复计费。

## 排队过久

Worker 串行，Run 从提交起就消耗 180 秒。`/metrics` 的 `incident_runs_active` 高且 `incident_oldest_ready_age_seconds` 持续上升时：

1. 确认 Worker 活着，且日志里有 `run finished`。
2. 模型变慢时，单 Run 可到 180 秒。此时调低 `INCIDENT_MAX_ACTIVE_RUNS`，让调用方早点收到 503 重试。
3. 不要多开 Worker 来解决：多 Worker 不违反租约，但没有验收过。

## Run 被阻塞

日志：`run blocked`，带 `run_id` 和原因。Worker 跳过它继续服务其他 Run，但它一直占一个名额。

| 原因 | 处理 |
| --- | --- |
| `worker configuration does not match frozen job` | 配置变更造成，到 deadline 自动收尾，一般不用处理。若持续出现，检查 API 和 Worker 是否用了同一份 `deploy/.env` |
| `investigation` 输入无效（常见于升级后 Harness 版本不同） | 到 deadline 自动收尾，一般不用处理 |
| `claim CORRUPT`、`claim UNKNOWN_SCHEMA`、`unreadable job/checkpoint` | 不会自动收尾，按下面处理 |

人工处理（**会丢弃该 Run，先备份**）：

```bash
rcli GET incident:run:<run_id> > run-<run_id>.json
rcli ZREM incident:ready <run_id>
rcli ZREM incident:leases <run_id>
rcli DEL incident:run:<run_id>
```

保存的文件可能含业务 ID，按证据数据管理，不要放进工单或仓库。`UNKNOWN_SCHEMA` 多半是回滚造成的，优先升级回新版本，而不是删除。

## 内存接近上限

超过预留后新 Run 返回 503 `OVERLOADED`，已接受的 Run 不受影响。终态 Run 保留 7 天后自动释放。

1. 调大 `INCIDENT_REDIS_MAXMEMORY`，容器限制 `INCIDENT_REDIS_CONTAINER_MEMORY` 要留出 AOF 重写的余量（建议至少 2 倍），然后 `dc up --detach redis`。
2. 不要改成其他淘汰策略：Worker 和 readyz 都会拒绝，淘汰状态会破坏恢复保证。

## 费用异常

每 Run 上限 36,000 Token、12 次模型调用，所以费用约等于 Run 数乘上限。先看 `incident_runs_finished_total` 是否异常增长，是否有调用方不断换 `alert_id` 提交。紧急止血：`dc stop worker`。已接受的 Run 会到 deadline，下次启动时直接收尾，不再调用模型。上限是本地估算，账单以供应商为准。

## 备份与恢复

**备份**（Redis 运行中可做，最多丢失约 1 秒写入）：

```bash
rcli BGREWRITEAOF
rcli INFO persistence | grep aof_rewrite_in_progress   # 等到 0
sleep 2
docker run --rm -v incident-agent_redis-data:/data:ro -v "$PWD/backups:/backup" alpine:3 \
  tar czf /backup/redis-$(date +%Y%m%d%H%M%S).tgz -C /data appendonlydir
```

备份含调查证据和业务 ID，按敏感数据存放，保留期不超过业务要求。

**恢复**（会覆盖当前状态，先备份当前数据）：

```bash
dc stop worker api redis
docker run --rm -v incident-agent_redis-data:/data -v "$PWD/backups:/backup:ro" alpine:3 \
  sh -c 'rm -rf /data/appendonlydir && tar xzf /backup/<备份文件> -C /data && chown -R 999:1000 /data/appendonlydir'
dc up --detach --wait
```

恢复后，终态 Run 仍可查询；deadline 已过的活跃 Run 会被收尾为 DEADLINE。备份之后接受的 Run 会丢失，调用方重试同一 `alert_id` 会新建 Run。

**验证范围：** 备份和恢复的核心步骤（重写、复制目录、新 Redis 加载、读回和继续处理）有集成测试；`tar` 和数据卷这层命令没有实际跑过，第一次使用前请在非正式环境演练。
