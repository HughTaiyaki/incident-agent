# 任务 0007：生产工程基线

日期：2026-11-01。状态：本机工程验收完成，远端 CI 待运行。范围对应[路线 P0](../docs/development/production-roadmap.md)。

## 目标与实施

1. 将 Agent 模块从 Go 1.25 升级到官方仍支持的修复版本；只使用项目工具链选择，不修改系统默认 Go 或商城。
2. 固定 golangci-lint/govulncheck 版本；迁移旧 lint 配置，保留原有检查意图。
3. 提供统一发布检查：格式检查（不自动改文件）、vet/race/lint、独立 Redis 恢复测试、构建、模块校验与漏洞扫描；失败必须返回非零。
4. 添加仓库根 GitHub Actions，固定 Action 提交与最小权限；不发布、不部署、不调用真实模型/商城。
5. 记录本机验收及远端 CI 尚未运行的事实，同步 AGENTS/README/索引。

## 验收门槛

- 支持中的工具链完成五个二进制构建、既有单元/race 和 Worker 恢复回归。
- govulncheck 的源码可达检查通过，含持久化/Worker 路径；不得仅凭 JSON 输出进程退出 0 判为通过。
- 发布命令不依赖 PATH 中旧版 lint，也不隐式升级到 latest。
- 文档能重现版本、环境、命令、结果与局限。远端 CI 仅配置不计作实测。

## 验收结果与交接

2026-11-01 本机 `make release-check` 退出 0：模块/格式/vet/race/lint、独立 Redis 恢复、五个命令构建和普通/Redis 标签漏洞扫描全部通过。格式检查负例及越界符号链接拒绝验证通过，CI 配置 actionlint 通过。没有推送或运行远端 CI。

版本、文件变化与限制见[改动报告](../docs/reports/0007-production-baseline.md)。下一阶段是[任务 0008](0008-capacity-and-results.md)，本轮未实现容量控制或终态投影。
