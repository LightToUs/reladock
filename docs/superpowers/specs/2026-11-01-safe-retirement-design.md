# RelaDock 安全退役设计

## 意图

RelaDock 当前会永久保留曾下发规则的端口隔离，避免旧 NAT/conntrack 状态把新客户流量送往旧目标。下一阶段要把“永久占用”收敛为可审计、可恢复的安全退役：只有 Agent 在连接跟踪前的持续精确隔离下清理并验证指定 allocation 的 conntrack 状态后，平台才能释放端口。释放后本地保护性隔离可以继续存在，直到未来一次原子配置应用安全接管该 tuple；节点退役撤销控制身份，但不声称删除 VPS 或清空这些保护性内核规则。

本设计服务企业和网络服务商的 TCP/UDP 透传，不终结或理解 VLESS、WireGuard 等客户协议。

## 核心决定

### 两层状态机

每个 endpoint allocation 新增不可变 `allocation_id` UUID，规则快照按 TCP/UDP 携带对应 ID。规则删除在同一事务为每个 `(allocation_id, node, listen address, protocol, listen port)` 建立 retirement item，并生成完整配置快照；该快照原子移除 NAT、计数与整形，同时在 raw prerouting 优先级建立带 allocation ID 标识的精确 pre-conntrack quarantine。状态机为 `waiting-removal -> queued -> claimed -> purging -> released`。只有尚未开始副作用的任务可进入带有界错误码的 `failed`；未知执行结果保持 `purging` 并显示需处理，不能超时猜测成功。平台不保留不可观察的 `verified` 中间状态，最终 ACK 直接在一个事务内保存 proof 并 `purging -> released`。

同一节点最多有一个 `claimed/purging` retirement；数据库部分唯一索引是最终约束。节点存在任意未 released retirement 时不允许新建或领取 Agent upgrade，已有 active upgrade 时 retirement 只能停留在 waiting/queued，不能 claim/begin。删除规则和生成 quarantine 快照本身仍允许发生，避免升级阻止客户停止业务。

节点保留现有 `unprovisioned/ready/maintenance` 状态，并新增 `draining/retired`。`maintenance` 是可恢复的停止分配，管理员从这里显式进入不可逆的 `draining`；draining 禁止新端口分配、恢复规则、编辑网络身份、重新登记和创建升级任务。只有不存在未删除规则、endpoint allocation、活动 retirement、在途配置、Agent 升级和未处理应用屏障时才允许完成 retired。完成退役在同一事务撤销节点证书、轮换结果和所有 bootstrap grant，增加 revision 并写平台审计。retired 节点不可重新启用；重新使用同一 VPS 必须登记新节点身份。

### 清理边界

Agent 只接受平台数据库中已存在、且当前 applied Good 同时持有相同 allocation ID 的 pre-conntrack quarantine 和历史 forward guard 的精确 allocation；retirement claim 自身通过已登记的 mTLS 身份和不可猜测租约绑定，不依赖调用方提供的 allocation。执行顺序固定为：持久化本地 retirement journal；调用幂等 `begin-purge` 并等待明确成功；取得与 apply/upgrade 相同的外部控制锁；验证当前两个 nft guard；使用固定绝对路径 `/usr/sbin/conntrack` 按 IPv4、协议、原始目标地址和原始目标端口删除；使用同样过滤条件重复只读 XML 查询直到零匹配；重新验证 pre-conntrack quarantine；通过现有 Pending/Recovery 耐久转换原子重建 nft，只移除该 allocation ID 的 forward guard、继续保留 pre-conntrack quarantine；验证新 Good 与内核一致并进入本地 `PurgedPendingACK`；持久化 proof；通过 mTLS ACK。任何步骤失败都不 ACK，平台继续保留 allocation，本地 journal 保留可恢复事实。

pre-conntrack quarantine 位于 RelaDock 自有 `ip reladock` 表的独立 base chain，类型为 filter、hook 为 prerouting、优先级必须早于 conntrack（固定 raw 优先级），按具体 `ip daddr + meta l4proto + th dport` drop。删除快照在同一个 nft transaction 中以该 quarantine 替换旧 DNAT，避免无保护窗口；Verifier 必须核对 chain 属性、精确表达式、allocation ID comment 和表所有权。现有 forward guard 只负责阻止已建立/NAT 状态继续转发，不能作为“阻止新 conntrack”的清理门禁。

每个 waiting/queued/claimed retirement 的快照中同时生成两条 allocation-ID-bound 保护：raw-priority quarantine 阻止新 conntrack，forward guard 阻止清理前已存在的 DNAT 流继续转发。两条规则 comment 均包含固定版本前缀和 allocation ID 摘要，Verifier 将 comment 与精确表达式共同校验。历史 migration generation 也必须用新规则替换旧的无 ID forward guard；旧 guard 只能作为保守保护，不能满足 purge 门禁。

进入 purging 后平台阻止配置 claim/begin，清理流程因此可以在“查询为零”后把本地 Good 转为 `PurgedPendingACK`：该状态只保留 pre-conntrack quarantine，不再保留已无连接可保护的 forward guard。转换失败或提交不明时沿用 dataplane recovery 恢复“两层保护”并且不 ACK；转换成功后即使 ACK 丢失也持续 quarantine 并重放相同 proof。平台 ACK 成功后，本地仅把该项从 `PurgedPendingACK` 归档为 `ReusableQuarantines`，不再修改内核。proof/Good digest记录 forward guard 的删除前 identity 与删除后不存在事实；最终 drain attestation 只核算仍应存在的 pre-conntrack quarantine。

conntrack 参数由代码根据结构化 allocation 固定生成，tuple 是 Agent 所在主机默认 network namespace、默认 conntrack zone 0 中，进入 DNAT 前的具体 ingress IPv4、L4 协议和原始目标端口；源地址/源端口有意不限制，从而清理该入口的全部客户连接。删除形态等价于 `-D -f ipv4 -p <tcp|udp> --orig-dst <address> --dport <port>`，查询形态等价于 `-L -f ipv4 -p <tcp|udp> --orig-dst <address> --dport <port> -o xml`。v1 不支持 setns、非零 zone 或 IPv6。只接受具体合法全局单播 IPv4、`tcp/udp` 和平台端口范围；不解析人类可读摘要作为成功依据。删除返回“零匹配”和查询得到零条都视为幂等零状态，但命令语法错误、权限错误、截断/非 XML 输出和超时均失败关闭。

`0.0.0.0` 只允许未部署节点做计划占位；现有 dataplane 同样不允许它进入 applied 快照。自动清理要求 allocation 冻结的是具体 ingress IPv4，不能在 Agent 侧把 wildcard 临时展开成网卡地址。

历史 wildcard allocation 在 v1 永久保持 waiting-removal/占用，不提供网页重绑、SQL 绕过或 Agent 自动推断；含此类 allocation 的节点不能完成自动退役，必须等待未来单独设计的受控迁移。仅当节点从未 applied、从未登记证书、无规则/allocations/历史业务事实时，平台可提供无需 Agent attestation 的“撤销未部署登记”，它与已部署节点退役是不同动作。

禁止 `conntrack -F`、无过滤删除、shell 命令、调用方提供参数向量、自动修改 sysctl/flowtable 或清理其他节点/地址。TCP 与 UDP 是不同 retirement item；`both` 必须两项分别成功才释放相同数字端口。通用 UDP 端到端健康仍未知。

### 平台释放条件

平台在节点事务锁下验证：规则已 deleted；其 quarantine generation 已被节点 applied generation 覆盖；对应 generation 的 outbox 确实有 applied 事实；Agent 证书身份匹配；item token、allocation ID/tuple 和 proof 匹配；同 allocation ID 没有新规则引用。proof 是 schema-versioned、有界 canonical JSON，字段固定为任务 ID、allocation ID、节点、默认 namespace/zone、tuple、quarantine generation、前后 guard identity/digest、conntrack 版本、固定过滤器版本、删除/零匹配事实、kernel boot ID、本地 journal sequence 和 Good 状态摘要。字符串采用规范化 IPv4/小写协议，JSON 使用结构体稳定字段顺序，平台重算 SHA-256 后保存。真实性来自 mTLS Agent，这是经认证的节点断言，不表述成独立内核证明。随后将 item 标记 released 并删除对应 endpoint allocation，二者与平台审计同事务。重复 ACK 幂等；冲突 ACK 不改变状态；原始命令输出不进入网页或审计。

规则删除事务为每个仍保留的 endpoint allocation 建立一条 retirement item，并记录包含 quarantine 的新 generation；`both` 因此固定产生 TCP、UDP 两条。创建规则继续以 endpoint allocation tuple 主键作为最终互斥，同时每次插入产生新 allocation ID；retirement 未 released 时永远不可复用。claim 只返回数据库中冻结的 allocation ID、tuple 和 generation，不能由 Agent 改写。最终 ACK 才在一个事务内保存 proof、released 和删除 allocation。

释放后 Agent 仍在 Good 的 `ReusableQuarantines` 中保留旧 allocation ID 及 pre-conntrack quarantine。未来同 tuple 的新规则必须拥有不同 allocation ID；平台创建新 allocation 时记录唯一 `predecessor_retirement_id`，快照同时携带旧/新 ID 的接管关系。普通 apply 只有看到“旧 ID 已本地清理”“旧 retirement 与 predecessor 匹配”和“新 ID 为当前快照 active allocation”时，才在一次 nft transaction 中移除旧 quarantine 并建立新 DNAT。失败回滚恢复旧 quarantine。新 generation 的 applied ACK 在节点事务内把 predecessor retirement 标记为 `quarantine_consumed_at` 并记录 `consumed_by_allocation_id`；在 applied 前绝不认为旧隔离已消费。旧 journal、proof 或任务因 allocation ID/guard comment 不匹配，不能清理或 ACK 新 incarnation。若端口一直不复用，保护性 quarantine 可以永久保留。

### 崩溃与恢复

本地 journal 保存任务、租约 token、allocation ID/tuple、begin-purge 确认、删除前 guard 摘要、清理结果、本地状态转换 intent、验证后的 Good 摘要和最终报告。Agent 必须先耐久写 journal，再调用 begin-purge；只有明确收到平台已进入 purging 才可执行删除。begin 响应不明时只幂等重试/查询，绝不执行副作用。删除命令可重放，零匹配视为幂等成功，但只有 quarantine 在删除前后均验证、且本地状态转换经过现有 pending/recovery 语义后才可 ACK。平台响应丢失会重发同一 proof；若平台已幂等接受，Agent 收到成功后才能清理 journal。启动时先恢复 retirement journal，再允许领取或应用可能复用端口的新配置。不能删除 journal、dataplane Tombstone 或数据库 retirement 行来解除隔离。

平台 claim 租约为 2 分钟，queued/failed 使用有界指数退避且最多自动领取 5 次；租约只保护执行者，不代表任务成功。进入 `purging` 后不因租约超时自动重置或由另一执行者并发领取。Agent 重启后凭同一持久 journal、任务 ID 和 token 继续；当前有效的同一 node identity 证书即使经过正常轮换也可继续，手工证书撤销和重新登记在 purging 时被服务端禁止。如果本地 journal 丢失，任务保持需人工处理而不是重新猜测。运维可对尚未开始副作用的 `queued/failed` 重试，对过期 `claimed` 重新领取；已进入 `purging` 的未知结果必须由同一节点本地恢复流程处理。没有“强制释放”网页按钮。

应用与退役共享一个根级外部控制锁，且 supervisor 在单个循环中优先恢复未完成 retirement，再执行普通 apply。平台有 `applying` 配置时不允许 claim/begin retirement；有 `purging` retirement 时不允许 claim/begin 配置。queued/waiting retirement 不阻塞无关快照，但所有新快照必须携带这些尚未开始 purge 的两层保护；apply/rollback 还必须叠加本地 PurgedPendingACK/ReusableQuarantines 的 pre-conntrack quarantine。普通 apply 发现目标 allocation ID 存在未完成本地 retirement intent 时失败关闭。规则 deleted 不可恢复，因此 rollback 也不能重新激活已释放规则。退役对 dataplane 状态的修改沿用 Candidate/Pending/Recovery 的耐久写入和内核回滚约束，不能另写一个跳过 WAL 的简化状态文件。

后台 reconciler 只推进可由数据库证明的 `waiting-removal -> queued`：quarantine generation 已 applied、对应 outbox 为 applied、allocation/规则关系仍一致。它绝不推断 conntrack 或本地 Agent 成功。错误码固定为 `tool-missing/tool-version/permission-denied/guard-missing/guard-mismatch/state-mismatch/output-invalid/query-nonzero/io-failed/internal-error`，浏览器仅显示映射后的安全说明。

## 管理界面与运维

规则列表显示“等待移除应用 / 等待连接清理 / 清理中 / 已安全释放 / 需处理”，并显示有界错误原因和最近更新时间。节点列表先提供现有“进入维护”，维护节点才显示“开始排空”；draining 不可取消，只允许删除剩余规则、配置重试、正常证书续签/轮换和取消尚未开始的升级，不允许恢复规则、编辑节点、重新登记/撤销证书或创建升级。“完成退役”仅由服务端计算的资格字段启用，确认文案明确会永久撤销 Agent 身份、保留保护性入口隔离但不会删除 VPS。网页只提交节点 ID、revision 和动作，不提交 eligibility 或 allocation。

运维文档提供精确查询命令用于人工核对，但不提供绕过状态机的 SQL 或 flush 命令。真实 Linux 验收必须覆盖长连接 TCP、UDP 状态、重复 purge、Agent 在删除后/ACK 前崩溃、平台响应丢失、同端口并发创建和节点退役门禁。

## 非目标

- 不迁移现有规则到另一节点；排空要求客户先删除或另建规则。
- 不承诺已有客户连接无感迁移。
- 不自动删除 VPS、DNS、云安全组、路由、主机账号或第三方防火墙。
- 不清理不属于 RelaDock 精确 allocation 的 conntrack。
- 不把 retirement ACK 表示为端到端业务健康。
- 不改变支付或套餐实现。

## 安全与验收门禁

默认 validation-only Agent 不执行 conntrack，也不领取 retirement；只有显式 apply unit 且通过现有专用网络租约门禁时可领取 purge。安装器为 apply 主机安装/校验 conntrack-tools，但不自动启用 apply。缺少固定 conntrack 二进制、`CAP_NET_ADMIN`、权限、guard、持久状态或可验证输出时失败关闭。生产就绪仍要求真实 VPS/内核版本上的隔离测试和离机恢复演练；单元测试、mock runner 或 Docker network namespace 不能替代该门禁。

节点完成 retired 的服务端门禁固定要求：状态为 draining；无非 deleted 规则；无 endpoint allocation；无非 released retirement；`desired_generation = applied_generation` 且最新 generation 的 outbox 为 applied；无 leased/applying 配置；无活动 Agent 升级；无待激活证书轮换；并且有当前 certificate identity 在当前 generation 上提交的最终 drain attestation。attestation 固定证明：无 Pending/Candidate/Recovery、无未完成 retirement journal、无活动业务规则、用量 WAL 已完成最终采样且全部 ACK、updater 无恢复任务、kernel readback 与 Good digest 一致；并列出仍保留的 ReusableQuarantines。

平台根据 `released AND quarantine_consumed_at IS NULL` 的 retirement 行计算期望 ReusableQuarantines，attestation 的规范化 `(retirement_id, allocation_id, tuple, guard_digest)` 集合必须精确相等，多一项或少一项都拒绝。attestation 绑定 node ID、desired/applied generation、当前 certificate fingerprint、kernel boot ID、Agent started-at/version、journal sequence、Good digest、期望 quarantine set digest 和 observed-at。服务端只接受 observed-at 在接收时间前 2 分钟内且不晚于服务端 30 秒的报告；仅用于一次 finalize 事务。generation/revision 改变、证书轮换、后续 heartbeat 报告不同 boot ID/started-at、更新的 attestation/journal sequence 或任何新本地恢复状态报告都会使旧 attestation 失效。

满足门禁后同事务撤销证书和所有 bootstrap、删除未激活轮换结果、将状态置为 retired、增加 revision 并写平台审计。历史规则、用量、账本、审计、proof、已 released retirement 和最后 attestation 保留，不物理删除节点行。retired 的含义是平台资源已释放、业务配置为空且控制身份已撤销；保护性 ReusableQuarantines 留在 VPS，直到操作员删除 VPS/网络或另行执行未来设计的受控主机清理。

## 迁移策略

迁移为现有 endpoint allocation 增加 allocation UUID，并写入新快照 schema；对已 deleted 且仍保留 allocation 的历史规则创建 `waiting-removal` item，但绝不假设现有 forward guard 可清理。迁移为相关节点生成新的 quarantine generation，该 generation 同时安装 allocation-ID-bound pre-conntrack quarantine 和 forward guard；只有 Agent 应用并验证这两层保护且回报 applied 后，reconciler 才将其 queued。当前 Agent 无 matching Good/allocation ID、节点使用 wildcard 地址或无法应用新 schema 时，任务保持 waiting-removal 并显示需升级/修复，不自动释放。

