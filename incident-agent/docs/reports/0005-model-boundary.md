# 任务 0005 改动说明：接入有预算的模型决策

日期：2026-10-31。本轮将脚本调查内核扩展为可通过显式配置调用模型，优先补齐上下文边界、费用控制和失败可核查能力。商城未修改；这是一轮生产化基础建设，不是整个系统的生产就绪声明。

## 从什么变成什么

之前只能从本地脚本读取 query/report 决策，也没有 Token 核算。现在独立的 incident-model 命令可将选择后的证据交给 Chat Completions 兼容端点，并使用模型返回的决策继续受控调查。原有完全离线命令保持独立。

每次调用先预留预算，报告有独立保留额度；输入不是全量 Run，而是范围、质量与有限记录。Provider 返回决策后仍受原来的参数/范围/引用校验，不能扩张权限。

## 文件改动

| 文件 | 作用 |
| --- | --- |
| [model.go](../../internal/app/investigation/model.go) | Provider 接口、单 Run ModelDecider、调用前预留、usage 结算、预算超额停止与隔离审计副本 |
| [model_context.go](../../internal/app/investigation/model_context.go) | 固定系统规则、有界输入、最近来源质量、代表记录优先和省略标记 |
| [client.go](../../internal/adapter/outbound/modelhttp/client.go) | HTTP Provider、TLS/回环配置、密钥注入、响应上限、无代理/重定向/重试、有限响应解析 |
| [runner.go](../../internal/app/investigation/runner.go)、[types.go](../../internal/app/investigation/types.go) | 预算不足转汇总、模型错误停止原因、Run 调用审计；策略升级为 harness-v3 |
| [run.go](../../cmd/incident-model/run.go)、[main.go](../../cmd/incident-model/main.go) | 显式联网模型命令，读取配置与请求，调用 Fixture 查询，保存报告/调用账目 |
| [Makefile](../../Makefile)、[请求示例](../../testdata/investigation/observed-request.json) | 构建第四个二进制；提供不含脚本答案的调查请求 |
| model_test.go / model_context_test.go / modelhttp/client_test.go / incident-model/run_test.go | 预算、上下文、HTTP 边界与命令闭环回归测试 |

另新增[任务 0005](../../tasks/0005-model-boundary.md)及[使用文档](../development/model-provider.md)，同步项目入口、架构状态与 changelog。任务 0004 的聚合代码已完成收尾；历史 Bundle 未修改，无新增第三方依赖。

## 主要边界与限制

总额度 24,000，调查保留 3,000 给报告，单次输出最多 1,500；汇总输出目前最多 512。UTF-8 字节估算带固定开销，不能承诺账单硬上限。失败/缺 usage 消耗预留，发现超过预留则停止后续调用，不继续使用超额结果。

最多保留三次近期查询质量与 24 条记录，摘要截断与遗漏可见；该选择策略尚未做效果评测。没有完整反例保留保证，也未实现持久化模型上下文。

HTTP 非流式 JSON 子集已用本地模拟服务验证；没有真实供应商联调或准确率成绩。所有调用账目仍在内存中，正常收尾后落盘，崩溃时不支持恢复。入口认证、调度恢复、工作台和固定故障集仍待验收。

## 验证依据

新增测试覆盖：模拟 Provider 查询→Fixture 证据→带引用报告、计费审计、既有输出不触发付费调用、缺配置、不足/未知/负值/超额 usage、报告预留、超时、HTTP 429/503、拒答、输出截断、重定向和响应上限。真实模型效果不计入这些工程测试结果。

`make verify`、`make build`、`go mod verify` 全部通过。构建后的命令对本地模拟 Provider 实测两次调用、四条日志、零遥测请求，模拟 usage 为 1,000 Token，报告 INCONCLUSIVE/UNKNOWN。该数值仅为测试核算。

完成前验证结果与实际命令记录在[任务验收](../../tasks/0005-model-boundary.md)。运行步骤、字段含义和完整生产门槛见[Provider 使用指南](../development/model-provider.md)。

## 下一步

先对选定模型做显式配置后的受控联调，冻结请求、策略与样本版本；同时推进独立 Redis 的 Run/ModelCall 持久化与崩溃恢复，避免已经付费的请求因进程中断丢失审计。商城数据集仍沿用“必要改动先提醒用户审查”的约束。
