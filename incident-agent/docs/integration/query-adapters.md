# 只读查询、证据导出与离线回放

更新：2026-10-28。状态：**查询 CLI 和三源 Adapter 已实现，真实历史窗口已验证；任务 0002 的固定版本故障案例尚未验收。** 本文负责当前代码的使用方法；目标字段语义见[遥测契约](../telemetry/data-contract.md)，本次运行依据见[接入报告](../reports/0002-readonly-integration.md)。

## 1. 当前入口与边界

`incident-query` 是操作员本地命令，执行固定的只读 HTTP GET 查询。它不启动商城、不注入故障、不执行 SQL，也不提供公网查询 API。原 `incident-api` 仍只有健康检查；尚未实现模型、Incident/Run、持久预算或权限会话。

实现位置：

- `internal/telemetry/`：结构化查询、固定模板、范围校验、请求预算、证据与 Bundle。
- `internal/adapter/outbound/telemetryhttp/`：Prometheus、Loki、Jaeger 的响应转换。
- `internal/adapter/outbound/fixture/`：已冻结样本的离线过滤回放。
- `internal/app/capture/`：消费方接口、有限查询集合与匿名化导出。
- `cmd/incident-query/`：命令参数、进程环境、生命周期与退出码。

`telemetry.Policy` 由组装方配置白名单，CLI 使用首版五服务和 local/experiment 环境。每个查询指定单一 service、environment 和固定窗口；Trace 详情逐 Span 校验环境、时间和服务白名单，并要求请求服务在可见 Span 中出现。跨 Trace 引用只保留原始关系类型和查询线索，不证明目标 Trace 已获授权；再次读取仍执行相同范围检查。

## 2. 先运行冻结样本

在项目目录执行，需要 Go 1.26.10 或兼容的受支持工具链；校验数据库与工具版本见[发布检查](../development/release-checks.md)：

```bash
make build
mkdir -p data
./bin/incident-query -mode replay \
  -fixture testdata/telemetry/observed-20261028/bundle.json \
  -input testdata/telemetry/observed-20261028/queries.json \
  -out data/replay.json
```

样本来自真实历史遥测，但不包含已标注故障真值。输出 6 个查询结果、169 条规范化记录，后端请求数为 0。结果文件采用 0600 权限，已存在时拒绝覆盖；再次运行需选择新的 `-out` 文件名。`data/` 已被 Git 忽略。

stdout 只输出 JSON 摘要；诊断信息写 stderr。退出码：0 表示查询/写出成功，1 表示运行或数据源失败，2 表示参数/输入错误。源失败时仍尽量保存 UNAVAILABLE 和 error_code；退出 0 不代表已诊断根因或业务健康。

## 3. 在线查询

后端地址只来自进程环境，不能放进查询 JSON：

| 环境变量 | 默认地址 |
| --- | --- |
| INCIDENT_PROMETHEUS_URL | http://127.0.0.1:9090 |
| INCIDENT_LOKI_URL | http://127.0.0.1:3100 |
| INCIDENT_JAEGER_URL | http://127.0.0.1:16686 |

当前适用于受控本机环境；不支持在 URL 中嵌入密码，不读取系统 HTTP 代理，不跟随重定向。尚未实现远程认证头或只读代理部署；固定路径查询不代表后端本身已经完成权限隔离。

`-input` 是最多 16KiB、1–16 项的 JSON 数组，未知字段直接拒绝。时间使用 RFC3339，必须明确起止、最长 1 小时。下面是格式示例，在线使用前换成确有数据的固定窗口：

```json
[
  {
    "kind": "metrics",
    "service": "order-service",
    "environment": "local",
    "start": "2026-10-28T06:00:00Z",
    "end": "2026-10-28T06:05:00Z",
    "metric": "workflow_backlog",
    "step_seconds": 15
  },
  {
    "kind": "logs",
    "service": "order-service",
    "environment": "local",
    "start": "2026-10-28T06:00:00Z",
    "end": "2026-10-28T06:05:00Z",
    "limit": 100
  }
]
```

```bash
./bin/incident-query -mode live -input data/queries.json -out data/capture.json
```

只有确认运行中商城的实际构建版本时，才传 `-upstream-sha <40位SHA>`；否则保留默认 `unknown`。当前目录 HEAD 或服务版本字符串不足以证明正在运行的程序版本。

| kind | 可用选择字段 | 返回上限 |
| --- | --- | --- |
| metrics | metric；按模板可选 peer、workflow_step、pool；step_seconds 默认 15 | 每次最多 20 条序列，每序列最多 240 个点；超大请求先拒绝 |
| logs | trace_id、order_id、operation_id、event_id、event_name、severity | 最多 100 条结构记录；不会解析 body 为 stdout JSON |
| traces | service、environment、时间窗 | 最多 20 个摘要；Jaeger 搜索使用微秒时间戳 |
| trace | 另需 32 位小写十六进制 trace_id | 最多 200 个可见 Span，保留 CHILD_OF/FOLLOWS_FROM |

metric 名称对应遥测契约中的 9 个 metric_key，不接受自由 PromQL 或 LogQL。DB WaitCount 对外使用等待命名；pool limit=0 通过模板排除；NaN/Inf 保存为 `value_status=UNDEFINED`，不转换为零。

每次采集共享最多 16 次实际请求，每次最多 5s、响应最多 2MiB，整个采集最多 180s。429/502/503/504 最多退避重试一次，重试再次扣预算。未实现分页游标：只读取有限首批，达到上限保守标 truncated，不假装得到全窗口计数；不接受 cursor 等未知字段。

## 4. 脱敏、证据与回放语义

首版**省略全部日志自由文本和异常栈**，只保存事件、状态、结果、关联 ID 等白名单结构字段；同时记录 `free_text_omitted` 和源记录摘要 hash。未知属性不进入快照。缺少结构化 event_name 的普通日志，只能作为有限的关联线索，不能还原其正文或据此补造业务事实。

导出为 Bundle 前，用本次随机盐对订单、操作、事件、Trace/Span 等关联 ID 一致匿名化，保留同一包内跨源关系；盐不写入包。规范化查询中的 ID 同样替换，服务、时间与非 ID 条件保持原样。匿名 ID 用于回放，**不能用于重新查询真实后端**。原查询文件若含真实业务 ID，应只存于被忽略且受限的本地目录。

Bundle 保存 schema/策略版本、origin、upstream_sha、采集时间、每次查询/来源质量、记录及 SHA-256 摘要；记录保留原始事件时间和采集时间。hash 用于完整性检测，不是防篡改签名。入口验证 schema、字段白名单和摘要，不能用合法 hash 绕过导入约束。

回放对覆盖请求窗口的冻结记录执行字段/时间过滤，不为几个预设查询返回固定答案；不访问网络、不消耗后端预算。原始采集时间保留在记录中，回放结果顶层时间表示本次回放时间。查询超出采集范围或改变无法重算的指标维度时返回 `FIXTURE_QUERY_NOT_COVERED`。样本内无匹配与未采集窗口有明确区别，覆盖程度保持保守；回放无法补齐采样或截断缺口。

EMPTY、UNAVAILABLE、PARTIAL 与 AVAILABLE 分开；freshness 和 coverage 没有可信信号时为 UNKNOWN。业务状态、诊断结论、告警是否存在仍需单独判断。

## 5. 验证与后续验收

```bash
make verify
make build
INCIDENT_INTEGRATION_QUERIES="$PWD/data/live-queries.json" make test-integration
```

在线集成测试显式启用，要求输入含 metrics、logs、traces、trace 四类查询且每类能返回已知记录；缺少环境、文件或数据直接失败，不以跳过冒充通过。测试依赖运行中的后端和保留期内的真实 Trace ID，不能直接使用匿名化回放 queries.json。

尚待任务 0002 完成：固定运行版本的故障案例采集、D1/D3/N1 与证据不足对照、异常/恢复人工结论。自动告警轮询属于后续切片；持久调查的在线模式见 [Worker 指南](../development/worker-recovery.md#在线遥测模式)。

协议参考：[Prometheus HTTP API](https://prometheus.io/docs/prometheus/latest/querying/api/)、[Loki HTTP API](https://grafana.com/docs/loki/latest/reference/loki-http-api/)、[Jaeger 2.11 APIs](https://www.jaegertracing.io/docs/2.11/architecture/apis/)。本次实现使用已部署版本验证过的接口子集；Jaeger JSON 查询 API 存在版本耦合，升级时重跑契约和在线验收。
