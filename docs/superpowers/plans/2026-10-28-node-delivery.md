# Node delivery Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans inline. 用户要求继续直接开发，沿用当前目录，不创建 Git 仓库、不等待方案确认。

**Goal:** 增加真实数据库节点管理、事务配置 Outbox、mTLS 校验 Agent 和页面交付状态。

**Architecture:** 全节点期望配置快照与规则变更同事务写库，按节点递增 generation。独立 HTTPS Agent 通道验证客户端 CA、节点 URI 与已登记证书摘要；租约领取及校验回报幂等。Agent 只做结构校验，不调用 nftables/tc，不回报已应用。

**Tech Stack:** Go、PostgreSQL、标准库 TLS/X509、现有 HTML/JS。

**Spec:** AGENTS.md、docs/REQUIREMENTS.md、docs/ARCHITECTURE.md、docs/ROADMAP.md。

## Global Constraints

- 仅回环监听；不得修改真实网络规则或采购/公开部署。
- Agent 校验通过不等于生效或健康，applied_version 保持 0。
- 节点登记/维护/证书绑定仅运维 CLI，企业 owner 不等于平台管理员。
- 不向企业接口返回跨租户配置快照、目标或证书信息。
- 沿用现有数据，版本迁移支持从 v1 升级，不清表。

## Review Focus

- 配置与规则/审计/端口全部回滚，不能留下未入队的规则。
- 并发节点变更避免锁顺序反转；每份快照 generation 唯一递增。
- 过期租约、重复 ACK、陈旧快照、失败重试不能篡改最新校验版本。
- 无证书、错误节点、未绑定/撤销证书不得领取配置。
- 维护节点不可新增规则；页面不将心跳或校验视为在线转发。

### Task 1: 数据库与节点配置事务

Files: internal/platform/migration2.sql、store.go、nodes.go、delivery.go、rules.go、delivery_test.go。

- [x] 测试先覆盖节点维护、规则变更入队、并发完整快照、升级回填、回滚。
- [x] 实现 v2 迁移、List/Add/Maintain/EnrollNode、配置快照及 checksum。
- [x] 实现租约/补领、失败退避、旧配置淘汰和幂等校验报告，真实数据库测试通过。

### Task 2: mTLS 通道与校验 Agent

Files: internal/nodeauth、internal/agent、internal/web/agent.go、cmd/reladock-agent、cmd/reladock-admin、cmd/reladock/main.go。

- [x] 测试先覆盖实际 TLS 身份边界、checksum/节点/重复端口等校验、正确校验不增加生效版本。
- [x] 实现证书工具与运维 CLI（独占文件创建，拒绝覆盖密钥）；可选独立 Agent HTTPS listener。
- [x] 实现 Go 校验 Agent，验证服务端证书，领取、验证、回报，不执行网络操作。

### Task 3: 页面、运维与验证

Files: public/*、internal/web/server.go、README.md、AGENTS.md、docs/*。

- [x] 节点选择和维护提示、规则恢复、手动刷新、节点期望/校验版本与最近联系，状态准确。
- [x] 全套 npm test、go race、vet；独立复查重要路径并修复。
- [x] 重启本机服务，浏览器验收，更新已实现与未实现清单。

## Execution record

- 范围决策：此轮交付配置分发与校验，不伪造 nftables/tc 应用、健康检查或真实计费。mTLS 可选监听 127.0.0.1:3002，未设置证书时不启动。
- 本机没有 Git 仓库，在原目录执行。数据库测试沿用独立随机 schema。

## 交付验证（2026-10-28）

- 22 个 Go 测试与 2 个前端测试通过，`npm test` 包含真实 PostgreSQL/TLS/HTTP/Agent 集成与竞态检测，`npm run vet` 通过。
- 实际 v1→v2 数据迁移测试验证既有账户、会话、规则保留且回填配置快照；本机服务升级也已完成。
- 浏览器以临时测试企业验证节点选择、创建入队、暂停/恢复重新入队，规则版本 1→2→3、节点 generation 1→2→3→4，生效与校验版本均保持 0。测试账户与规则已通过精确身份匹配清理，规则删除走事务 Outbox 逻辑，最新快照不含测试规则。
- 独立只读复查未发现 Important/Critical 问题。生产 Adapter、持久应用状态、健康检查、生产证书生命周期、滚动升级、历史清理和规模限制保留为后续任务，不声称已交付。
- 决策：此轮使用 validation-only Agent；没有实际执行网络规则，也没有生成/登记本机业务节点证书或启用新的 Agent 监听。mTLS 往返只在随机隔离数据库 schema 的集成测试中执行。
- 决策：维护只拒绝新分配，保留既有配置；平台运维管理使用本机 CLI，企业 owner 无全局写权限。
- 决策：领取最多 5 次，耗尽后显式显示需运维处理，由 retry 创建最新快照；不无限自动重试。
- 当前服务持续运行于 127.0.0.1:3001。原浏览器已填写的登录表单未被刷新或覆盖，验收在独立临时标签执行。
