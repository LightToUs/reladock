# Agent 安全升级与自动回滚设计

## 意图

RelaDock 已能从固定 GitHub Release 首次安装 Agent，但已有节点没有受控升级流程。生产试点需要平台管理员逐节点请求升级、节点验证固定制品、保留旧版本并在新版本未健康启动时自动回滚，同时不能把控制面变成远程命令执行系统。

本阶段只完善 Agent/数据面二进制升级。支付模块保持现状，不接真实支付、不修改账务语义。控制服务升级仍由运维显式执行，不由节点升级器处理。

## 目标

- 平台管理员只能把一个节点升级到控制服务当前配置的固定 Release，不能提交 URL、命令、路径、摘要或任意版本。
- 同一时刻全平台最多一个进行中的升级，先完成香港入口试点的单节点灰度。
- 节点上的独立 root Updater 通过现有节点 mTLS 身份领取升级任务；普通 Agent 继续使用 `reladock` 非特权账号。
- Updater 固定从 `LightToUs/reladock` 的目标 tag 下载当前架构归档，验证平台返回的 64 位 SHA256、归档平面白名单、包内 `SHA256SUMS` 和二进制普通文件属性。
- 安装前保存当前 `reladock-agent` 与 `reladock-dataplane`，原子替换后重启当前实际启用的 validation 或 apply 单元。
- 新 Agent 必须在 120 秒内成功上报目标版本心跳并完成一次配置轮询；只有两项都成功后 Updater 才报告升级成功，否则恢复旧二进制并重启原单元。
- 控制面持久保存请求、领取、安装、成功、失败和回滚事实；后台显示目标版本、状态、时间和有界错误代码。
- 任务、状态与平台审计同事务；重复请求和重复报告幂等；进程或主机在任意阶段重启后可以从持久状态继续或保守回滚。

## 非目标与反目标

- 不自动升级控制服务、PostgreSQL、systemd 单元、环境文件、PKI、nftables/tc 配置或操作系统软件包。
- 不提供任意脚本、命令行、下载 URL、文件路径或用户自定义摘要。
- 不默认批量升级，不按 `latest` 自动跟随，不在没有管理员请求时升级。
- 不因 systemd `active`、进程存在或版本字符串变化就宣称转发健康。
- 不删除或重建 Agent journal、WAL、计量状态、PKI、shaping 租约或 applying 屏障。
- 不把回滚成功解释为业务流量已经恢复。

## 信任与权限边界

Updater 是新的 `reladock-updater` Go 二进制，由 root oneshot systemd 单元和 timer 运行。它只拥有以下能力：读取 root 管理的 Agent 环境文件与节点 PKI；通过固定 mTLS 控制 origin 请求升级任务；下载固定 GitHub 仓库 Release；写 `/opt/reladock` 的两个二进制与 `/var/lib/reladock-updater` 私有状态；调用 systemd 停止、启动和查询两个已知 Agent 单元。它不接受 shell 文本，不调用 shell，不修改网络规则。

任务来源必须同时满足：平台管理员会话授权、当前节点修订号、控制服务配置完整 Release 元数据、目标版本严格高于 Agent 当前语义版本、数据库不存在其他 active 升级。平台只返回自己启动时验证过的版本与对应架构摘要。节点请求不能选择其他版本。

Updater 不信任下载归档的路径或权限。归档必须只含现有 Release 白名单中的平面普通文件，不允许重复、链接、设备、目录或额外文件；解包大小有界。归档摘要和包内文件摘要均通过后才允许暂存。旧二进制备份和新二进制使用 root:root、0755，在同一文件系统通过临时文件、fsync 和 rename 发布。

## 控制面数据模型

迁移 v21 新增 `node_agent_upgrades`：

- `id` UUID 主键；`node_id` 外键；`target_version`；amd64/arm64 SHA256。
- 状态仅允许 `queued`、`claimed`、`installing`、`verifying`、`succeeded`、`failed`、`rolled-back`、`cancelled`。允许的边为 `queued→claimed`、`claimed→installing`、`installing→verifying`、`verifying→succeeded|rolled-back|failed`，以及 `queued|claimed→cancelled`、`claimed|installing→failed`；终态不可再次变化。
- `lease_token` 仅保存随机摘要，租约 5 分钟。只有尚未进入 `installing` 的过期 `claimed` 任务可以由同一节点重新领取并换发租约；进入 `installing` 后任务由节点持久状态恢复，原租约对该任务保持有效直到终态，每次有效报告更新活动时间，不允许第二执行者领取。
- `previous_version`、有界 `error_code`、requested/claimed/reported/completed 时间。
- 每节点最多一个 active 任务；全平台部分唯一索引保证最多一个 active 任务。
- 已完成记录不可重用为新任务；再次升级创建新 ID。

节点表不复制升级状态，以升级表为事实来源。管理员节点查询以左连接返回最新任务摘要。平台审计仅记录节点、任务、目标版本和状态，不记录证书、租约、归档内容、主机路径或原始错误。`cancelled` 只用于管理员取消尚未安装的 `queued`/`claimed` 任务；安装开始后不能远程取消或伪造完成。

## HTTP 与状态机

后台新增 `POST /api/admin/nodes/{id}/upgrade`，请求体只含 `revision`。服务端从已验证 `BootstrapConfig` 读取目标 Release 和摘要；未配置、当前版本未知、目标不高于当前版本、节点离线、节点维护中或已有其他 active 任务时拒绝。创建任务与节点 revision、平台审计同事务。相同节点、相同目标已有 active 任务时，重复请求返回原任务且不新增审计；其余新请求必须匹配当前 revision。另有只接受 `revision` 的取消接口，但仅允许取消 `queued`/`claimed`。

Updater 使用节点 mTLS：

- `POST /agent/upgrade/claim`：提交规范化架构和当前编译版本，领取本节点任务；响应包含任务 ID、目标版本、所选架构摘要和固定 GitHub 仓库标识，不返回任意 URL。
- `POST /agent/upgrade/report`：提交任务 ID、租约、状态和允许列表错误码。状态只能沿显式状态边推进；重复相同报告成功，不同结果冲突。
- 普通 Agent 心跳包含纯编译版本。目标版本心跳只更新节点事实，不单独完成任务。Updater 观察到由新 Agent 在成功心跳和成功配置轮询后写出的本地健康证明，才报告 `succeeded`；控制面在同一事务中再次确认节点最近心跳版本等于目标版本后进入终态。

任务错误码限制为：`download-unavailable`、`archive-invalid`、`checksum-mismatch`、`install-failed`、`service-failed`、`health-timeout`、`rollback-failed`、`state-invalid`。原始系统错误只进本机 journal，不上传数据库或通知。

## 节点升级流程

1. timer 启动 Updater；没有任务时立即退出。
2. 通过 mTLS 领取任务，把任务和租约原子写入 `/var/lib/reladock-updater/state.json`。
3. 下载固定归档到私有临时目录，验证外层摘要、归档白名单和包内摘要。
4. 将新 Agent/Dataplane 暂存到 updater 私有目录；向平台报告 `installing`。
5. 检测当前 active 单元，只允许 `reladock-agent.service` 或 `reladock-agent-apply.service`，拒绝两者同时 active；停止该单元后取得现有 control journal 锁并确认没有待恢复的 delivery journal。配置应用正在进行或存在持久应用屏障时，不替换二进制，重启原单元并报告 `failed/state-invalid`。
6. 复制当前两个二进制到本任务回滚目录，记录各自摘要并 fsync。
7. 原子替换两个二进制，在 root 私有状态中写入带随机 challenge 的 `awaiting-health.json`，把只读请求投影到 `/run/reladock-updater/health-request.json`（`root:reladock`、`0640`），释放 control journal 锁，启动原单元并报告 `verifying`。
8. 新 Agent 读取请求；在同一任务 ID、challenge 和目标版本下分别成功完成心跳和一次配置轮询后，原子写入 `/run/reladock-agent/upgrade-health.json`。该标记是进程存活证明，不是制品完整性证明；制品完整性只由 Updater 的固定 Release 摘要校验建立。
9. Updater 最多等待 120 秒。匹配健康标记、服务保持 active 且控制面保存的最近心跳版本等于目标版本时报告 `succeeded`，收到成功响应后才清理回滚副本。
10. 超时、启动失败或状态不一致时恢复两个旧二进制、重启原单元并报告 `rolled-back`；恢复也失败则报告 `rollback-failed` 并保留全部状态供人工处理。

下载、验证和尚未替换时崩溃可安全重做。替换后崩溃时，下一次 Updater 根据 `state.json`、当前/备份摘要和健康标记继续验证或回滚，并在 `/run` 被重建后重新投影健康请求，不猜测成功。Updater 使用本机进程锁避免同节点重复执行。任何未知组合都失败关闭并保留文件。

## 编译版本与兼容性

新增 `internal/buildinfo`，默认版本为 `dev`；Release 构建使用 Go ldflags 注入 `v<package.json version>`。Agent 心跳、请求头和 Updater 都使用同一值，移除硬编码版本。正式升级只接受严格 `vMAJOR.MINOR.PATCH`，首期只允许同 major 且目标大于当前版本；`dev` 不能发起或确认生产升级。

自动升级只替换两个二进制，因此新 Release 必须继续兼容已安装 systemd 参数与环境文件，并保证升级验证期间写入的 journal、WAL、计量、PKI 和数据面状态仍可由同一主版本的旧二进制读取。Updater 不快照或回滚这些业务状态。需要不可逆状态迁移、修改单元、环境或前置条件的版本必须在 Release 元数据中标记为手工升级；本阶段不自动执行。

## 首次部署 Updater

现有节点不会凭空获得 root Updater。Release 包、`install-linux.sh` 和首次节点安装包增加 Updater 二进制、oneshot 单元和 timer；新节点首次安装时一并启用 timer。已存在节点必须由运维使用可信固定版本安装包执行一次人工升级来安装这些文件，完成后后续 Agent/Dataplane 版本才走自动升级。本阶段实现和测试该安装路径，但不自动登录或修改现有生产 VPS。

## 后台呈现

节点列表增加“Agent 版本 / 升级”事实：当前编译版本、目标版本、升级状态、最近更新时间和有界错误码。仅平台管理员看到“升级到当前固定版本”按钮；进行中禁用，失败/回滚后允许重新创建新任务。文案明确：升级成功只表示新 Agent 完成控制心跳和配置轮询，不表示数据面或客户协议健康。

## 测试与验收

- PostgreSQL：全平台单 active、节点锁/修订冲突、权限、幂等领取/报告、租约过期、状态单向、审计失败回滚、心跳完成任务。
- HTTP/mTLS：错误节点、错误证书、架构、版本、任务/租约、正文和重放边界。
- Updater：外层摘要、归档穿越/链接/重复/额外文件、包内摘要、部分写、fsync/rename、服务选择、启动失败、健康超时、回滚失败、每个崩溃点恢复。
- Linux 隔离测试：fake systemctl 下成功切换与自动回滚；真实 VPS/systemd、GitHub 可达性和 apply 模式升级仍作为生产环境验收，不能由容器测试替代。

## 后续阶段

完成本阶段后继续实现：Prometheus/运维指标与告警、控制数据库和节点状态离机恢复演练、节点/端口安全退役、真实 TCP/UDP/协议链路与容量验收。支付保持最后处理。
