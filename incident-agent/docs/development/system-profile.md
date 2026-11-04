# 系统配置档

更新：2026-11-03。适用于 `incident-api`、`incident-worker`、`incident-model` 和 `incident-eval`。

Agent 对被诊断系统的了解都放在一份版本化的配置档里，不写在代码和通用提示词中。要换一个系统，先写一份配置档。

## 配置

`INCIDENT_SYSTEM_PROFILE_FILE` 指向配置档，必须显式设置。Compose 部署用 `INCIDENT_SYSTEM_PROFILE=<名字>` 选择 `deploy/profiles/<名字>.json`。当前只有 [goframeshop](../../deploy/profiles/goframeshop.json)。

| 字段 | 含义 |
| --- | --- |
| `version` | 配置档版本，随任务写入 Job |
| `source` | 这些知识的来源，比如上游版本 |
| `services` | 允许调查的服务，1 至 5 个 |
| `environments` | 允许调查的环境 |
| `facts` | 解释这个系统记录的说明，可以为空 |
| `expectations` | 文档记载的调用期限和来源，可以省略。只用来和记录比较，不代表实际发生的情况 |

`facts` 的 `kind` 只能是 `service_map`、`field_meaning`、`business_outcome`、`business_contract`、`event_meaning`、`error_code`、`deadline`。

**配置档只能解释记录，不能代替本次事实。** 比如“1062 是重复键”可以写，“这次重复请求正常完成、没有影响”不能写，后者必须由本次证据证明。提示词会告诉模型这些说明不是证据。

未知字段、缺字段、重复或不安全的名字都会拒绝启动，避免拼写错误悄悄丢掉知识。

## 冻结和恢复

提交任务时，说明文字和版本写入 Job。Worker 接管时用 Job 里的内容，所以中途改配置档不会改变运行中的任务要告诉模型的内容。Worker 处理任务时还会检查请求是否仍在当前配置档的范围内，不在范围内的任务会被阻塞。

## 调查流程

`INCIDENT_INVESTIGATION_FLOW` 决定新任务由谁选查询，也会写入 Job：

| 值 | 查询 | 报告输入 |
| --- | --- | --- |
| `agent`（默认） | 程序先扫全部服务日志，之后由模型选 | 现有内容 |
| `hybrid` | 程序扫日志，查错误率、延迟、积压和 Trace，再按证据缺口补查，不靠模型选 | 加上[调用关系](../reports/0013-call-relations-comparison.md)、指标摘要和覆盖情况，先放不同种类的警告；校验时长、百分比和“没有错误” |

默认仍是 `agent`。`hybrid` 是开发主线，但[新场景验收](../reports/0013-new-scenario-acceptance.md)没有通过，所以需要显式开启。这不代表 `agent` 更可靠。

## 还和商城绑定的部分

配置档还没有覆盖这些，换系统时要改代码：

- **指标：** PromQL 模板和提示词里的指标名（比如 `order_error_rate`、`payment_confirmation_delay_p95`）。
- **采集工具：** `incident-query` 和 `incident-eval capture` 仍用 `telemetry.DefaultPolicy`。
- **字段约定：** 补查规则读 `severity_text`、`rpc.method`，重试规则读商城日志的 `attempt`。系统没有这些字段时规则不会触发，不会报错。
- **提示词的说明：** 报告规则里的 `order_id`、`operation_id`、`event_id` 是查询过滤字段名。
- **最多 5 个服务：** 请求和配置档都有这个上限。
