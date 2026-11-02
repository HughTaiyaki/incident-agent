# 部署指南

更新：2026-11-02。状态：**镜像和 Compose 通过 CI 冒烟测试，尚未在正式环境部署过。** 故障处理见[值班手册](runbook.md)，取舍和测量见[任务 0011](../../tasks/0011-deployment-operations.md)。

## 1. 组成

| 服务 | 作用 | 限制 |
| --- | --- | --- |
| `redis` | 独立状态存储，AOF everysec、noeviction，只通过 Unix socket 访问 | 无网络、只读根文件系统、无 capability |
| `api` | HTTP 入口，只发布到本机回环 | 256MiB，非 root，只读，无 capability |
| `worker` | 串行执行调查，访问模型和遥测后端 | 512MiB，非 root，只读，无 capability |

不要把 `redis` 换成商城缓存 Redis。Prometheus、Loki、Jaeger 复用现有实例，不在这个 Compose 里。

## 2. 首次部署

在项目目录执行：

```bash
cp deploy/.env.example deploy/.env
mkdir -p deploy/secrets
openssl rand -hex 32 > deploy/secrets/incident_api_token
```

把模型密钥写入 `deploy/secrets/incident_model_api_key`。`deploy/.env` 和 `deploy/secrets/` 都被 Git 忽略，密钥不要放进 `.env`、命令行或 Compose 文件。文件必须能被容器内的 uid 65532 读取，其他用户不可读：

```bash
sudo chown 65532:65532 deploy/secrets/incident_api_token deploy/secrets/incident_model_api_key
sudo chmod 400 deploy/secrets/incident_api_token deploy/secrets/incident_model_api_key
```

检查 `deploy/.env` 后启动：

```bash
docker compose -f deploy/compose.yaml --env-file deploy/.env up --build --detach --wait
```

确认 `curl http://127.0.0.1:8080/readyz` 返回 `{"status":"ready"}`。接口用法见 [API 指南](../development/http-api.md)。

**启动 worker 就会把遥测记录发给配置的模型，可能产生费用。** 业务 ID 会假名化，但这不是匿名化，见 [Provider 指南](../development/model-provider.md#3-上下文怎样选择)。

## 3. 容量取值

以下是 2026-11-02 在 macOS arm64、Redis 8.0.2 上的单机测量，**不是生产压测**。真实模型样本很少。

| 项目 | 结果 | 怎么用 |
| --- | --- | --- |
| 单 Run 耗时（DeepSeek，Bundle 3 次，在线 2 次） | 6–17 秒，在线 10.7 秒；接管一次约 25 秒 | deadline 180 秒且 Worker 串行，按 17 秒算最多排约 10 个 |
| 单 Run Token（DeepSeek） | 有错误日志的在线窗口约 16,600，需修正报告时约 21,500；实测约 ¥0.03/次（多为非高峰时段） | 费用预算按接近上限估算，高峰价格翻倍 |
| `INCIDENT_MAX_ACTIVE_RUNS` | 建议从 2 开始，不超过 8 | 再多的 Run 大概率排队到 DEADLINE。慢模型要下调：单次调用可到 30 秒 |
| 内存预留 | 256MiB 实测接受 31 个活跃 Run，2GiB 接受 255 个 | 符合 `maxmemory ÷ 8MiB`，超出返回 503 |
| 终态 Run 内存 | 模拟模型 40 次约 12KiB/个；真实 Run 结果 8–13KiB，未直接测总占用 | 保留 7 天；单 Run 上限 7MiB |
| 提交吞吐 | 约 4000 次/秒，p99 19ms（并发 64，含被拒请求） | 瓶颈是 Worker，不是 API |

## 4. 监控

`GET /metrics` 使用和 API 相同的 Token。Prometheus 配置示例，告警规则在 [alerts.yml](../../deploy/prometheus/alerts.yml)：

```yaml
rule_files: [alerts.yml]
scrape_configs:
  - job_name: incident-agent
    authorization:
      credentials_file: /etc/prometheus/incident_api_token
    static_configs:
      - targets: ["127.0.0.1:8080"]
```

`incident_*_total` 是尽力而为的计数：Worker 在终态提交后、计数前崩溃会少记一次，Run 状态本身不受影响。费用核对以供应商账单为准。Worker 日志是 stderr 上的 JSON，只有 Run ID、状态、停止原因和用量，不含报告正文。

## 5. 升级与回滚

```bash
git checkout <目标提交>
docker compose -f deploy/compose.yaml --env-file deploy/.env up --build --detach --wait
```

- **进行中的 Run：** Worker 收到 SIGTERM 后停止新调用，Run 在 15 秒租约到期后由新 Worker 接管。deadline 不会暂停，停机过久的 Run 以 DEADLINE 结束。
- **模型或后端配置变更：** 配置冻结在每个 Run 里，旧 Run 被新 Worker 跳过（`IncidentRunBlocked` 告警），到 deadline 后自动收尾，最多 180 秒。想避免告警，先等活跃 Run 清零再变更。
- **回滚：** 检出上一个提交重新执行上面的命令。状态 schema 只增不改时可直接回滚；发布说明标注 schema 变化的版本不能直接回滚，因为旧版本遇到未知 schema 会跳过这些 Run。
- **Harness 策略版本变化：** 进行中 Run 的 checkpoint 无法恢复，同样会被跳过并告警，到 deadline 后收尾。
- **回滚前先备份**，见[值班手册](runbook.md#备份与恢复)。

## 6. 未覆盖

- **TLS 和对外暴露：** API 是明文 HTTP，只发布到回环。对外必须在前面放校验身份的 TLS 反向代理。
- **认证：** 单一共享 Token，没有角色、限流或审计主体。轮换需要改文件后重启 `api`，有短暂中断。
- **高可用：** 单 Redis、单 Worker。主机故障可能丢失最多约 1 秒的写入。
- **后端连接：** 遥测后端没有认证头，也未拒绝非回环 HTTP，只能用于受控网络。
- **告警投递：** 规则已校验语法，投递路由由部署方配置，未演练过。
