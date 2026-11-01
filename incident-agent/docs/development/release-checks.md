# 工程发布检查

更新：2026-11-01。对应[任务 0007](../../tasks/0007-production-baseline.md)与[生产路线 P0](production-roadmap.md)。这里的通过只代表程序与已知漏洞检查通过，不代表真实供应商或诊断效果已验收。

## 版本和环境

| 项目 | 固定值 | 来源 |
| --- | --- | --- |
| Agent 最低 Go 工具链 | 1.26.10 | go.mod；采用仍受支持分支的安全修复版本 |
| golangci-lint | v2.15.0 | Makefile；配置 schema v2 |
| govulncheck | v1.9.0 | Makefile；按当前漏洞数据库检查 |
| 应用依赖 | go-redis/v9 v9.20.1 及原有间接依赖 | go.mod/go.sum，未因工具升级连带升级 |
| Redis 恢复测试 | 本机实测 8.0.2；需 redis-server/redis-cli，7+ | 测试自建临时 Unix socket 实例，不使用商城 Redis |

Go 版本和支持政策核对自[官方下载列表](https://go.dev/dl/)及[发布记录](https://go.dev/doc/devel/release)。Lint 配置按[官方迁移指南](https://golangci-lint.run/docs/product/migration-guide/)迁移；staticcheck 保留原 SA 检查范围，不在本轮混入新的风格规则。revive、gosec、errcheck 等原有检查继续启用，新增告警逐点处理。

`make` 命令在子进程中设置 `GOSUMDB=sum.golang.org`，校验公开模块及自动下载的工具链。没有执行 go env -w、替换系统 Go 或改动商城。若本机全局 GOSUMDB=off，直接运行 go 命令可能无法选择新工具链，应显式使用：

```sh
GOSUMDB=sum.golang.org go version
GOSUMDB=sum.golang.org go test ./...
```

初次检查会下载固定版本工具和依赖到 Go 缓存，需联网；不会安装/覆盖 PATH 中的 golangci-lint。工具以 `go run <module>@<固定版本>` 执行，依赖由各自的模块版本与校验机制约束。

## 本地入口

```sh
make verify          # 开发检查：会格式化文件
make release-check   # 发布检查：格式不正确时失败，不自动改源码
```

release-check 顺序执行：格式检查 → go mod verify → vet → race → 包含 Redis 标签的 lint → 独立 Redis 恢复测试 → 五个二进制构建 → 普通/Redis 标签源码漏洞扫描。每一步失败都停止，返回非零；不发布、不部署、不访问真实模型或商城遥测。

独立入口：`make check-fmt`、`make lint-redis`、`make test-redis`、`make security`。格式检查器使用当前模块工具链的 go/format，并通过 os.Root 限制文件读取范围；忽略 .git/vendor/data/tmp/bin/dist，不修改文件。原来的 make fmt 保留给开发者主动修正格式。

漏洞门槛使用 govulncheck 的普通输出与退出码，**不使用 JSON 模式退出 0 作为通过依据**。检查结果随数据库更新变化；一次扫描通过不是未来安全保证。工具依赖自身的安全治理仍需持续跟进，源码扫描目标是本应用，而非所有开发工具。

## GitHub Actions

仓库根[工作流](../../../.github/workflows/agent-release-check.yml)在 Agent 或其工作流变更时运行，也支持手动触发。工作目录为 incident-agent，Go 版本读取 go.mod；Actions 固定提交、contents:read、checkout 不保留凭据，30 分钟超时，同分支旧检查可取消。

CI 安装 Redis 测试可执行程序，但测试只连接自己创建的 Unix socket。没有模型密钥、发布令牌、部署步骤，也没有商城实验。增加 go mod tidy 后差异检查，避免模块文件漂移。

2026-11-01 首次远端运行通过：提交 `42c8f3f`，ubuntu-24.04，模块 tidy 差异检查和 `make release-check` 均成功，见[运行记录](https://github.com/HughTaiyaki/incident-agent/actions/workflows/agent-release-check.yml)。仓库随后重建，该运行记录已不存在；重建后首次运行在提交 `75c2f49` 上[通过](https://github.com/HughTaiyaki/incident-agent/actions/runs/41315442725)，范围相同。之后的提交以各自的运行结果为准；本地 macOS 通过不能替代远端 Linux runner 结果。

仓库[公开](https://github.com/HughTaiyaki/incident-agent)，提交前不得纳入本机绝对路径、个人邮箱、密钥或运行日志。根目录与项目 `.gitignore` 排除本地配置和 `data/`，提交者邮箱使用 GitHub noreply 地址。
