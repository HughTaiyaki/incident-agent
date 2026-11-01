# 阶段 0007：生产工程基线改动报告

日期：2026-11-01。对应[任务 0007](../../tasks/0007-production-baseline.md)；后续按[生产路线](../development/production-roadmap.md)推进。本轮先处理工具链漏洞与可重复检查，不改变调查协议、预算或 Redis 状态格式。

## 修改前后

此前使用 Go 1.25.3，历史扫描报告 29 项可达标准库漏洞；本轮把模块最低工具链升级为 Go 1.26.10，普通与 Redis 集成标签源码扫描均报告 `No vulnerabilities found.`。这是当前工具链、源码和漏洞数据库下的结果，不代表没有未知漏洞。保留[旧扫描报告](0006-redis-client.md)作为历史记录。

此前 lint 依赖本机安装版本，缺少统一发布门槛。现在通过 `make release-check` 顺序执行格式、模块校验、vet、race、带 Redis 标签的 lint、真实 Redis 恢复测试、五个命令构建和两种源码漏洞扫描；任一步失败即停止。开发用 `make verify` 仍会格式化源码，发布检查只检查格式。

## 文件与职责

| 文件 | 改动与原因 |
| --- | --- |
| [go.mod](../../go.mod) | 最低工具链 Go 1.26.10；保留 go-redis/v9 v9.20.1 及原有运行依赖 |
| [Makefile](../../Makefile) | 固定 golangci-lint v2.15.0、govulncheck v1.9.0；新增发布检查及独立入口；子进程启用公开校验数据库 |
| [.golangci.yml](../../.golangci.yml) | 迁移 v2 配置，保留原检查意图；staticcheck 明确使用 SA 检查范围 |
| [格式检查器](../../internal/checks/format/main.go) | 使用所选 Go 工具链的 go/format，只读检查；os.Root 拒绝越界符号链接读取 |
| [根目录 CI 工作流](../../../.github/workflows/agent-release-check.yml) | Ubuntu runner 执行相同检查；固定 Action 提交、只读权限、超时、模块 tidy 差异检查 |
| cmd 与相关 Adapter、契约文件 | 按 lint 修复只读资源 Close 返回值处理、测试请求 context、包和导出成员注释；操作员文件路径的 gosec 例外逐处解释 |
| AGENTS、README、索引和专题 | 更新版本、实际能力与最小阅读路径；新增生产路线、发布指南和下一阶段任务 |

没有新增运行时依赖，没有修改系统默认 Go、全局 go env、商城代码或商城 Redis。固定版本检查工具通过 go run 下载到缓存，不覆盖系统安装的 lint。

## 验证记录

环境：macOS arm64，项目 Go 1.26.10，独立 Redis 8.0.2。Redis 测试创建临时 Unix socket 实例，TCP port=0，自行清理。没有调用真实供应商或商城在线遥测。

| 检查 | 结果 |
| --- | --- |
| 普通和 redisintegration 标签 govulncheck | 均通过，无已知可达漏洞报告 |
| 完整 make release-check | 退出 0；格式、模块校验、vet、race、lint、Redis 恢复、五个命令构建和两种扫描全部通过 |
| Redis 恢复与二进制核查 | Redis 回归用时 66.226s；incident-worker 构建元数据确认为 go1.26.10 |
| 格式检查失败路径 | 不合格式、无效语法均返回非零且不改文件；目录外符号链接被拒绝；临时样本已清理 |
| 工作流静态校验 | actionlint v1.7.7 通过，未启用额外 ShellCheck |
| 远端 GitHub Actions | 本轮未运行；后续首次推送后[通过](https://github.com/HughTaiyaki/incident-agent/actions/runs/41297113779)（提交 `df36eee`）。没有发布或部署 |

完整本机发布日志保存于 `data/production-baseline-20261101/release-check.log`，属于 git 忽略的运行产物。工作区尚未提交，不填写虚构提交号。工具版本、命令和校验数据库说明集中在[发布指南](../development/release-checks.md)。

## 下一阶段与剩余门槛

下一阶段为[任务 0008](../../tasks/0008-capacity-and-results.md)：原子全局 Run 准入、终态名额回收和状态查询投影，避免过载及展示中间 checkpoint 中的旧报告。此能力本轮尚未实现。

远端 Linux CI、真实供应商兼容性、在线持久调查、认证入口、部署恢复演练和固定故障集仍待验收。P0 工程检查通过不等于项目已经可以公开部署；本轮不引入消息队列。
