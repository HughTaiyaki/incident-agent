# 阶段 0006 增量：Go Redis 客户端

后续安全进展：Go 工具链升级和漏洞复验结果见[任务 0007 报告](0007-production-baseline.md)；本文保留当时的验收记录。

后续进展：本文件保留该增量的历史状态，当前 checkpoint/Worker 接线结果见[Worker 报告](0006-worker-recovery.md)。

日期：2026-11-01。用户已明确授权新增 go-redis/v9。当前完成客户端与真实 Redis 测试；任务 0006 的 Harness checkpoint、Worker 调度与进程崩溃恢复仍待完成。

## 本轮改动

- go.mod/go.sum 固定 go-redis/v9 v9.20.1；间接依赖 xxhash/v2 v2.3.0、atomic v1.11.0。该版本声明 Go 1.24，兼容项目 Go 1.25；查询到最新版 v9.23.0 要求 Go 1.26，未隐式升级工具链。
- [client.go](../../internal/adapter/outbound/redisstate/client.go)：显式连接/命名空间配置、TLS 校验、类型化 Command/Reply/State、固定脚本与 Key、上下文超时、禁用自动重试、错误正文脱敏。
- [单元测试](../../internal/adapter/outbound/redisstate/client_test.go)：非法地址/命名空间/动作、明文远程拒绝、不安全 TLS 拒绝、调用前取消、重试和 timeout 配置。
- [客户端集成测试](../../internal/adapter/outbound/redisstate/client_integration_test.go)：真实独立 Redis 的创建/调用/提交/接管/终结，命名空间隔离，超时后的状态读回，重建客户端并读取 AOF 保存的报告。
- 更新任务、AGENTS、README、架构状态、渐进式索引及[客户端使用契约](../development/redis-state.md#5-go-客户端2026-11-01)。

## 验证结果

通过：`make test-redis`（race）、`make verify`、带 redisintegration tag 的定向 lint、`make build`、`go mod verify`。本机 Redis 8.0.2，独立临时 Unix socket、port=0；未连接商城 Redis，未修改商城代码，未调用真实模型。

**漏洞扫描未通过。** 使用当前 Go 1.25.3 和 `govulncheck v1.1.4` 扫描，报告 29 项可达标准库漏洞；另有导入包/模块级条目，不能以功能测试通过替代安全验收。完整机器可读结果保存在本机 `data/redis-client-20261101/govulncheck.json`（运行产物，git 忽略）。该结果不是在线攻击验证，发布前应升级兼容且受支持的工具链并重新扫描，不宣称生产安全检查通过。

复现：

```sh
GOTOOLCHAIN=local go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...
```

## 后续范围

客户端尚未被调查 CLI 调用。下一步将完整执行快照和 Begin/Commit 接入 Harness，再增加 Worker 发现/续租/收尾与真实进程故障测试。工具链安全更新是生产发布前置项。原有[脚本阶段报告](0006-redis-protocol.md)保留历史验证范围；用户授权已经生效，无需再次确认此依赖。
