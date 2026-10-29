# Agent 一次性安装接入 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 管理员登记专用入口 VPS 后复制一条命令，即可从 GitHub Releases 安装 Agent、申请证书、自动启动连接校验并查看检查结果。

**Architecture:** 保留 Go/PostgreSQL 控制面及现有 mTLS 配置通道。在正常 HTTPS 控制入口新增受限的一次性 CSR 注册端点；安装器从固定仓库的指定 Release 获取经过固定摘要校验的程序，由 VPS 本地生成私钥。安装完成只启动 validation 服务，实际网络变更仍须确认，不自动启用 apply。

**Tech Stack:** Go 标准库、PostgreSQL 事务、POSIX shell、systemd、现有静态后台、Node.js 构建测试脚本。

**Spec:** 本文“已确认范围”和“实施选择”将本次对话作为实施基线；同时遵守 AGENTS.md、docs/REQUIREMENTS.md、docs/ARCHITECTURE.md、docs/DEPLOYMENT.md。

## 已确认范围

- 用户已确认入口为专用中转 VPS。
- 用户已选择 GitHub Releases 作为安装包来源。
- 后台新建节点后提供专属安装命令；安装后自动配置、注册、开机启动和连接校验。
- 节点私钥不上传；安装凭证短期、一次性、绑定节点、可撤销。
- 实际转发与连接校验分离，网络检查及操作者确认前不修改防火墙或启动实际转发。
- 本轮仅实现和验证，不发布 Release、不接触生产服务器、不使用客户凭据。

## 实施选择（待本计划确认）

- 安装凭证 256 位随机值，有效期 15 分钟，仅保存摘要；创建新凭证撤销同节点旧的未使用凭证。命令只显示一次，响应禁止缓存，日志与审计不记录凭证。
- 固定仓库 LightToUs/reladock，支持 Linux amd64/arm64；管理员配置明确版本及各架构安装包 SHA256，不跟随 latest。未配置实际发布版本时禁用生成命令并解释原因，不提供不存在的下载地址。
- 引导脚本由 HTTPS 控制台提供，以便在下载 GitHub 包前取得可信固定摘要；不使用 curl 管道直接执行。生成命令先下载到临时文件，再调用 shell；敏感凭证经隐藏输入交给引导进程，不进入 HTTP URL 或 sudo/systemd 参数，不声称可以消除复制命令本身的剪贴板/终端历史风险。
- 配置专用在线节点签发中间 CA，根 CA 保持离线。签发功能默认关闭，必须同时配置签发证书、私钥、信任链及公开 Agent origin；不复用开发 CA 或服务器叶证书。
- 节点证书有效期暂定 30 天，界面展示到期时间；本轮不假称已支持长期自动轮换，生产长周期使用前必须补齐轮换或明确手动轮换操作。
- 一次性兑换把凭证绑定 CSR 摘要。同一 CSR 在 15 分钟窗口内可重取同一证书以恢复响应丢失，不产生第二个身份；不同 CSR 或撤销后的重放拒绝。已安装节点重跑默认拒绝，不覆盖原私钥、证书、WAL 或服务配置。
- 初始支持 systemd Linux；不自动修改包源、安装系统包、网卡 alias、qdisc、offload、路由或 IP forwarding。缺少依赖时报告缺项；cloud 安全组不在 Agent 可控范围。

## Global Constraints

- Go 1.24+、Node.js 20+；数据库验证只使用独立测试库随机 schema。
- 每条转发独立额度和带宽，安装流程不得更改套餐或计量语义。
- 公开注册接口只能完成绑定节点的 CSR 兑换，不能授予平台权限或绕过现有 mTLS。
- 无 applied ACK 和外部业务验证，不显示“转发正常”；联系时间不代表健康。
- 不清空系统防火墙，不删除现有私钥、Agent journal、WAL 或 applying 屏障。
- 不自动发布、升级运行节点或修改真实网络规则。

## Review Focus

- 兑换提交成功但响应丢失：相同 CSR 可恢复，其他私钥不可劫持凭证。
- 并发签发与证书撤销：同节点事务锁保证旧身份不能通过迟到响应重新授权。
- 已有安装或部分安装中断：保护原有密钥、账务状态及服务，明确失败阶段。
- GitHub 不可达、错误架构、错误摘要、恶意归档路径：安装前失败，不运行未验证文件。
- 多网卡、已有 tc root、缺少工具或云 NAT：检查显示未知/不满足，不擅自改网络或误报可转发。

---

### Task 1: 一次性接入凭证与事务注册

**Files:** Create `internal/platform/bootstrap.go`, `internal/platform/bootstrap_test.go`, `internal/platform/migration18.sql`; modify `internal/platform/store.go`, `internal/platform/admin_nodes.go`.

**Interfaces:**
- Produce `CreateNodeBootstrap(ctx context.Context, who Identity, nodeID string, revision int64) (BootstrapGrant, error)`；Grant 含 token、expiresAt、nodeID、revision，只有创建返回原始 token。
- Produce `RevokeNodeBootstrap(ctx context.Context, who Identity, nodeID string, revision int64) error`。
- Produce `RedeemNodeBootstrap(ctx context.Context, token string, csrPEM []byte, signer BootstrapSigner) (BootstrapCertificate, error)`；签发和节点证书摘要绑定、配置入队、凭证兑换及审计在节点锁下原子完成。
- `BootstrapSigner` 只接收服务端确认的 nodeID 与经验证的 CSR，返回证书/链；不得接受客户端指定节点 URI、CA 权限或服务器用途。

- [ ] 添加过期、撤销、跨节点、非管理员、旧 revision、并发兑换、同 CSR 响应恢复、不同 CSR 重放、审计失败回滚及兑换/撤销竞争测试。
- [ ] 运行 `node scripts/start.mjs test ./internal/platform -run Bootstrap -count=1`，确认因未实现而失败。
- [ ] 实现 migration18 和接口，复用现有权限/节点锁及配置完整快照逻辑；人工证书轮换/撤销也使旧 bootstrap 恢复结果失效。
- [ ] 重跑上述命令，所有 Bootstrap 测试通过，并运行 `npm test`。
- [ ] 提交本任务代码及验证记录，不推送。

### Task 2: CSR 签发与受限 HTTPS API

**Files:** Create `internal/nodeauth/bootstrap.go`, `internal/nodeauth/bootstrap_test.go`, `internal/web/bootstrap.go`, `internal/web/bootstrap_test.go`; modify `internal/web/admin.go`, `internal/web/server.go`, `internal/runtimeconfig/config.go`, corresponding config tests, `cmd/reladock/main.go`, `deploy/control.env.example`.

**Interfaces:**
- Consume Task 1 store interfaces。
- Produce `nodeauth.LoadBootstrapSigner(certFile, keyFile, chainFile string) (platform.BootstrapSigner, error)`，校验 CA、私钥匹配、期限与用途，签发仅 ClientAuth 和 `spiffe://reladock/nodes/<id>`。
- Add admin `POST /api/admin/nodes/<id>/bootstrap`、`POST /api/admin/nodes/<id>/bootstrap-revoke`，沿用会话/同源/平台权限/revision。
- Add unauthenticated-but-token-guarded `POST /api/node-bootstrap/redeem`，Bearer token + 严格 JSON CSR；不放在需要客户端证书的 Agent listener 上。
- `BootstrapCertificate` JSON 包含节点编号、客户端证书、信任链、明确的 Agent HTTPS origin、证书到期时间；不含私钥。
- 新配置 `NODE_BOOTSTRAP_CA_CERT`, `NODE_BOOTSTRAP_CA_KEY`, `NODE_BOOTSTRAP_CA_CHAIN`, `AGENT_PUBLIC_ORIGIN`；部分配置或不安全 origin 拒绝启动，完全未配置则关闭。

- [ ] 编写无配置关闭、部分配置失败、CSR 签名错误、CA 提权/错误 URI、超长输入、错误凭证、限流、HTTPS 信任错误、响应 no-store、日志无凭证测试。
- [ ] 运行 `node scripts/start.mjs test ./internal/nodeauth ./internal/web ./internal/runtimeconfig -run Bootstrap -count=1`，确认失败。
- [ ] 实现签发器、端点、共享数据库限流及配置接线，不改浏览器认证或降低 Agent TLS 要求。
- [ ] 重跑测试至通过；验证证书可用于真实本地 mTLS 握手和一次配置校验。
- [ ] 提交本任务代码及验证记录，不推送。

### Task 3: 本地密钥注册命令及只读环境检查

**Files:** Create `internal/agent/bootstrap.go`, `internal/agent/bootstrap_test.go`, `internal/agent/preflight.go`, `internal/agent/preflight_test.go`; modify `cmd/reladock-agent/main.go`, `internal/web/agent.go`, `internal/platform/bootstrap.go`。

**Interfaces:**
- Produce `agent.Bootstrap(ctx context.Context, options BootstrapOptions) error`；Options 包含控制 HTTPS origin、凭证读取器、私有 PKI 目录，不接收已有私钥覆盖开关。
- Add `reladock-agent enroll --bootstrap-origin ... --pki ...`，凭证从 stdin 读取，不写日志/argv；复用已持久化 CSR 重试同一次注册，验证证书公钥/节点 URI/信任链后原子保存。
- Produce `agent.InspectHost(ctx context.Context) (HostReport, error)`，报告实际 IP/路由/架构、依赖、ip_forward、offload、qdisc 和检查时间，不修改网络。
- Add `POST /agent/preflight` 使用现有 mTLS 精确节点身份；节点只可更新自己的检查报告，数据库限制报告大小并保存时间。节点启动/周期连接时上报，权限不足的检查明确 unknown。

- [ ] 测试错误 CA、重定向、不匹配证书、响应丢失恢复、部分 PKI、目录权限、现有安装拒绝覆盖与无凭证泄漏。
- [ ] 测试环境检查仅执行固定只读命令；多网卡不自动选错接口、缺依赖和无权限显示 unknown、不满足条件不变为 ready。
- [ ] 运行 `node scripts/start.mjs test ./internal/agent ./internal/web ./internal/platform -run 'Bootstrap|Preflight' -count=1`，确认新增测试失败。
- [ ] 实现上述接口与子命令，默认仍是 validation，无 CAP_NET_ADMIN 的校验服务不得通过检测偷偷执行写操作。
- [ ] 重跑通过，并完成本地 HTTPS 注册 → mTLS 接入 → validated ACK 集成测试。
- [ ] 提交本任务代码及验证记录，不推送。

### Task 4: GitHub 安装包、安装脚本与后台命令

**Files:** Create `deploy/bootstrap-agent.sh`, `test/bootstrap-install.test.mjs`, `public/node-bootstrap.js`, `test/node-bootstrap.test.mjs`; modify `scripts/release.mjs`, `internal/web/bootstrap.go`, `internal/runtimeconfig/config.go`, `deploy/control.env.example`, `public/admin.html`, `public/admin.js`, `public/admin.css`。

**Interfaces:**
- Produce Release assets `reladock-v<VERSION>-linux-amd64.tar.gz` / `reladock-v<VERSION>-linux-arm64.tar.gz` 及独立归档 SHA256；保留原来的目录输出及包内 SHA256SUMS。
- 新配置 `NODE_RELEASE_VERSION`, `NODE_RELEASE_AMD64_SHA256`, `NODE_RELEASE_ARM64_SHA256`，严格版本/摘要校验；仅构造固定 GitHub 仓库下明确 tag 的 URL。
- Add `GET /install/agent.sh` 静态引导脚本和 `GET /api/node-bootstrap/release` 非敏感、禁止缓存的固定发布元数据；不接收任意下载 URL。
- Task 2 创建凭证响应包含可复制命令与 expiresAt；前端新增节点成功后打开安装面板，也支持现有节点重新生成/撤销。
- 引导脚本：root/Linux/systemd/依赖预检 → 私有临时目录 → 下载固定版本归档 → 比对控制台固定摘要 → 归档路径/类型检查 → 安装 → stdin 凭证兑换 → 启动并 enable validation 服务。任何步骤失败返回非零，不自动启动 apply。
- 已安装或不完整安装默认安全停止，保留状态并明确恢复操作；离线安装使用同版本本地验证包，网络兑换仍需控制台可达。

- [ ] 写安装沙箱测试：amd64/arm64 映射、错误摘要/下载失败/重定向策略、归档穿越和符号链接、已有配置保护、注册失败不启动服务、成功只启用 validation。
- [ ] 写界面测试：命令只在创建响应显示、过期/撤销/冲突反馈、缺发布配置禁用、转义与复制失败提示、不将 token 放入 localStorage/查询 URL。
- [ ] 运行 `node --test test/bootstrap-install.test.mjs test/node-bootstrap.test.mjs`，确认因功能缺失失败。
- [ ] 实现打包、脚本与安装面板；安装摘要从可信控制台取得而不是仅对比同下载源可替换的 SHA256SUMS。
- [ ] 重跑通过；两种架构均构建归档并验证清单。只测试本地模拟下载，不调用 GitHub 发布接口。
- [ ] 提交本任务代码及验证记录，不推送。

### Task 5: 全流程验证、部署说明与交付

**Files:** Modify `docs/DEPLOYMENT.md`, `docs/ARCHITECTURE.md`, `docs/ROADMAP.md`, `README.md`, `AGENTS.md`; add integration tests to Tasks 2–4 test files as appropriate。

- [ ] 先添加全流程失败测试：注册成功但本地落盘/启动失败后恢复、令牌过期或已撤销、GitHub 不可达、没有配置快照时的真实 Agent 联系上报。
- [ ] 实现最小恢复修正，不能删除 WAL、重新签发不同私钥身份或通过假状态掩盖失败。
- [ ] 在临时 Linux 容器/systemd 测试环境完成可信模拟 Release 下载 → 安装 → CSR 注册 → mTLS validated → 环境检查上报；不得改变主机网络规则。若测试环境不支持 systemd，明确区分真实接入验证与模拟服务调用。
- [ ] 运行 `npm test`、`npm run vet`、amd64/arm64 release 构建与归档校验；记录真实数据库测试结果，缺数据库不得宣称全套通过。
- [ ] 更新文档：管理员准备受信签发链/固定 Release 元数据、节点安装操作、凭证泄漏/过期/证书轮换、失败恢复和不自动启用网络的边界。
- [ ] 独立审查安全边界、并发兑换、安装供应链和误报状态；重要问题补失败测试后修复并重跑全套。
- [ ] 报告已实现/未验收，不发布 Release、不推送、不连接真实 VPS；上线发布及真实 VPS 验收另行授权。

## 自审记录

- 已覆盖对话确认的安装、自动配置、注册、自动启动校验与后台结果展示；网络修改仍是后续确认动作。
- Task 1 凭证事务是 Task 2 API 的依赖，Task 2 协议是 Task 3/4 的依赖，Task 4 的安装/前端共用固定发布元数据。
- 五项 Review Focus 均在所属任务列出故障测试。
- 对长期证书轮换、生产网络准备和真实 GitHub 下载仍须明确验收，不纳入“全部生产就绪”承诺。

## 执行与验收记录（2026-10-29）

五项功能任务已落地于 `feat/agent-bootstrap`；用户选择当前目录功能分支，未创建工作区，未推送或发布。为避免保存接口尚未接通的中间状态，本轮在整体验证后统一本地提交，而非逐任务提交。

- 最终 `RELADOCK_INSTALLER_DOCKER_TESTS=1 npm test` 成功：真实 PostgreSQL 随机 schema、Go 竞态检测、9 项 Node 测试全部通过，0 跳过。`npm run vet` 与 `git diff --check` 成功。
- Linux amd64 与 arm64 发布包已构建，归档及单独 SHA256 文件位于 `.cache/release/`；未上传到 GitHub。macOS AppleDouble 附加条目不进入归档。
- 真实本地 HTTPS CSR 注册、mTLS 配置校验、环境上报、响应丢失/部分落盘恢复、审计失败回滚和历史数据迁移测试通过。
- 无网络 Linux 安装器测试覆盖精确归档清单、异常符号链接/嵌套路径拒绝、已有凭据保护和只启动校验服务。服务管理器及安装器中的 Agent 注册结果使用模拟器，真实证书通信另由 Go 集成测试验证；不是实际 systemd/VPS 验收。
- 独立审查发现两项 Important：迟到界面响应恢复凭证/混淆节点、截断证书阻塞恢复。均先重现 RED，再修复为请求代次/节点绑定以及完整文件同步后原子无覆盖发布，回归与全套 GREEN。没有未处理的审查次要项。

实施裁定与边界：签发回调保留在无循环依赖的层次；平面归档必须由可信控制台固定摘要授权；bootstrap 地址仅支持 DNS/IPv4 HTTPS，IPv6 字面量需改用 DNS。前两项若失误会造成接口返工或安装拒绝，不降低信任校验。实际 GitHub 下载、生产 PKI、systemd、VPS 容量和端到端流量仍是上线门禁；证书自动续签未纳入本轮，30 天前须人工轮换。不能因本轮验收通过而标记生产就绪。
