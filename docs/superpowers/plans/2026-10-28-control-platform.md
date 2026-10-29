# RelaDock 控制平台第一步 Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans to implement this plan task-by-task. 用户已要求直接开发，在当前目录执行，不再等待方案审批。

**Goal:** 将本地 JSON 原型替换为实际运行的 Go + PostgreSQL 企业账户及规则控制台。

**Architecture:** PostgreSQL 保存企业、成员、会话、规则、端口占用、幂等请求与审计。Go HTTP 服务处理认证、授权与事务；保留并接入现有静态控制台。没有 Agent 的规则始终待部署。

**Tech Stack:** Go 1.24+、pgx v5、PostgreSQL 17、本地 Docker Compose、现有 HTML/JS。

**Spec:** AGENTS.md、docs/REQUIREMENTS.md、docs/ARCHITECTURE.md、docs/ROADMAP.md。

## Global Constraints

- 平台只中转 TCP/UDP，不托管客户 VPN 账号和订阅。
- 仅回环监听；不得操作实际网络规则、接真实支付或公开部署。
- 5 TB 月双向合计、最高 1 Gbps、USD；无支付与采集时不显示虚构余额或用量。
- 端口唯一性依赖数据库约束，TCP/UDP 分开；双协议原子占用。
- JSON 不再作为新控制平台数据源；不自动导入没有企业归属的旧规则。

## Review Focus

- 跨企业读取、暂停、删除规则必须拒绝，数据库以 tenant_id 限定操作。
- 并发分配和双协议冲突不得产生部分占用或孤立规则。
- 幂等键重放须返回相同规则，修改请求体须返回冲突。
- 退出后旧会话失效，跨站请求及不合法输入不得写库。
- 数据库不可用时停止启动；不退回内存或 JSON 假装成功。

### Task 1: 数据库、账户与会话

**Files:** go.mod、internal/platform/{schema.sql,store.go,auth.go,store_test.go}、compose.yaml、scripts/{setup.mjs,start.mjs}。
**Interfaces:** Open(ctx, url) 返回 Store；Register/Login/Session/Logout 返回企业隔离的身份。

- [x] 编写真实 PostgreSQL 集成测试：重复邮箱、密码错误、会话撤销和数据库重新连接后持久化。
- [x] 运行测试，确认缺失功能失败。
- [x] 实现版本迁移、事务注册、PBKDF2-SHA256 密码、随机且仅存摘要的会话。
- [x] 运行集成测试并确认通过。

### Task 2: 租户规则、端口与审计

**Files:** internal/platform/{rules.go,rules_test.go}。
**Interfaces:** CreateRule(ctx, identity, idempotencyKey, input)、ListRules、PauseRule、DeleteRule。

- [x] 编写测试：并发同协议端口唯一、TCP/UDP 共端口、both 原子分配、跨租户拒绝、幂等重放与冲突、暂停/删除版本及审计。
- [x] 运行测试，确认失败后实现租户授权、数据库锁和唯一约束、幂等与审计事务。
- [x] 验证回滚不会保留部分写入；删除释放端口。

### Task 3: HTTP 与浏览器操作流程

**Files:** cmd/reladock/main.go、internal/web/{server.go,server_test.go}、public/index.html、package.json。
**Interfaces:** /api/auth/{register,login,logout,me}、/api/overview、/api/rules、/api/rules/{id}/{pause,delete}。

- [x] HTTP 测试覆盖无登录、Cookie、跨站写、正文限制、非法输入、限流、规则操作及幂等。
- [x] 运行失败测试，再接入真实 Store、同源校验、严格 JSON、有限请求频率、HTTP 超时及回环监听。
- [x] 接入注册/登录/退出和规则暂停/删除，状态显示待部署/已暂停，不伪造监控。
- [x] 运行全套测试及浏览器流程，替换 3001 端口上旧原型。

### Task 4: 文档与验证

**Files:** README.md、AGENTS.md、docs/{REQUIREMENTS,ARCHITECTURE,ROADMAP}.md。

- [x] 更新实际实现、运行方式、数据库备份/重置说明、未接入能力。
- [x] go test -race ./...、go vet ./...、npm test；重启服务验证持久化。
- [x] 独立复查认证、并发、幂等、回滚和公开监听边界，修复重要问题。

## Execution record

- Baseline: npm test 2/2 passed。目录不是 Git 仓库，不初始化仓库、不创建 worktree、不提交；在用户指定目录直接修改。
- 按用户“直接开发”执行，无方案审批暂停。数据库仅新建项目独立本地容器，不触及已有其他项目数据库。

## 验证与交付记录（2026-10-28）

- 数据库、认证、规则、HTTP 的功能测试先出现缺失功能失败，再实现通过。12 个 Go 集成测试和 2 个前端重试测试通过。
- `npm test` 完整通过；另外运行 `node scripts/start.mjs test -race -count=1 ./...` 强制绕过缓存；`npm run vet` 通过。
- 浏览器实际完成企业注册、规则预留、暂停（期望版本由 1 变 2、生效版本保持 0）、退出和重新登录。重启 Go 服务后会话、规则与审计保留。验收专用账户与规则已按精确 ID 清理，页面留在登录入口。
- 独立代码复查发现两处 P2：列表刷新失败导致误导重复创建、跨站拒绝请求消耗登录限额；已增加回归测试并修复。
- Ruling: 当前目录没有 Git，不初始化仓库，不做 worktree/提交；代码直接保存在用户指定目录。
- Ruling: 先完成路线图首项的账户与规则纵向功能；UI 延用现有静态页面，React、成员邀请、API Key、Agent、计量、支付及账务继续列为未接入。
- Ruling: 未交付规则允许软删除并释放端口；`applied_version!=0` 时禁止释放，后续必须实现删除 ACK。
- 实际运行：127.0.0.1:3001；数据库：独立 reladock Compose 项目，127.0.0.1:55434。未公开服务，未修改实际转发/防火墙，未连接收款或客户凭据。
