# 独立逐规则 tc 整形组件

本组件写集仅 `internal/shaping/` 和本文件。不会修改 dataplane、Agent 或平台，不提供 applied/healthy ACK。

## 设计和实施计划

采用**每规则上传＋下载合计带宽上限**，这是本组件的明确设计选择，用户仅确认过逐规则独立上限。一个规则的 TCP/UDP、原始/回复方向共享同一 HTB class。两方向必须经过同一个显式租用的专用 egress；不同接口上的 HTB class 无法共享一个 token bucket，本组件拒绝该拓扑，不自动安装 IFB、重定向或修改路由。

1. 先编译测试 red：严格接口/租约/配置校验、稳定 mark、碰撞、双向共享和暂停；实现 `Compile(platform.NodeConfig, Options) (Plan,error)`。
2. 再事务/ownership 测试 red：外部 root 拒绝、租约变更、写前意图、单调版本、串行、非原子失败恢复；实现 `Adapter.Apply/Recover` 和可注入 Backend/StateStore/Locker。
3. 实现 tc/IP JSON 检查和受限 runner 接口、私有持久状态与锁；加入明确 opt-in 的本地 Docker 验证；运行 focused race/vet/Linux 编译，分别报告实际执行和未验证内容。

mark 方案：显式预留非零 8 位 namespace，高 8 位 namespace＋中间 16 位稳定规则编号，mask `0xffffff00`，低 8 位保留。规则编号由 `SHA256(nodeID/tenantID/ruleID)` 前 16 位映射到 2–65534；同一快照或历史编号碰撞直接拒绝，不进行顺序依赖的探测分配。nft 每个业务包按规则设置 packet mark，不依赖 NAT 首包或只保存 conntrack mark。输出供主 Agent 集成，不执行 nft。

专用接口必须有显式租约：namespace 身份、device、ifindex、owner alias、mark namespace 和声明链路容量。初次接受没有 root 或内核 `noqueue`。真实物理网卡若存在不可删除的内核默认 `fq/0:`，仅在租约额外设置 `ReplaceDefaultQdisc=true`、绑定该次 `tc -j qdisc` 原始输出的 SHA-256、接口身份与 alias 完全匹配且没有任何 class/filter 时，才允许在持久化 Candidate 意图后用固定 `qdisc replace ... handle 5244: htb default ffff` 完成交接。默认 false；指纹变化、非零 handle、其他 qdisc 或任何 class/filter 仍拒绝。不能根据固定 handle 猜测已有 HTB 的归属。后续只允许状态中记录的同一专用 HTB 树；不执行 root delete，也不接管其他外部 root。

## 交付 API

```go
Compile(cfg platform.NodeConfig, opts Options) (Plan, error)
NewAdapter(opts Options, backend Backend, store Store, lock Locker) (*Adapter, error)
(*Adapter).Apply(ctx context.Context, cfg platform.NodeConfig) (Plan, error)
(*Adapter).Ensure(ctx context.Context, cfg platform.NodeConfig) (Plan, error)
(*Adapter).ApplyVerified(ctx context.Context, cfg platform.NodeConfig) (Plan, error)
(*Adapter).Verify(ctx context.Context, cfg platform.NodeConfig) error
(*Adapter).Restore(ctx context.Context, verifiedOld Plan) (Plan, error)
(*Adapter).RestoreConfig(ctx context.Context, old platform.NodeConfig) (Plan, error)
(*Adapter).PrepareEmpty(ctx context.Context) error
(*Adapter).Recover(ctx context.Context) error

NewLinuxBackend(absoluteTCPath, absoluteIPPath string) (*SystemBackend, error)
NewSystemBackend(runner Runner, namespaceIdentity func(context.Context) (string, error)) *SystemBackend
NewFileStore(absolutePrivateDirectory string) (*FileStore, error)
(*FileStore).Locker() Locker
```

`Backend` 提供 `Inspect(ctx, Lease) (Snapshot,error)`、`RunTC(ctx, Lease, []string) error`。`Runner.Run(ctx, Tool, []string)` 仅选择 tc/ip，无 shell。`Store.Load/Save` 持久化 State；`Locker.Lock(ctx)` 返回 unlock。Backend、Store、Locker 均可注入。`Compile` 不构造执行器、不修改网络；没有新 CLI 或自动启用入口。

`Options` 包含 `NodeID`、`Lease`、`UploadEgress`、`DownloadEgress`。Lease 必须声明：

| 字段 | 要求 |
| --- | --- |
| `Device` | Linux 字面接口名，1–15 ASCII 字母数字/下划线/横线；拒绝 lo |
| `IfIndex` | 实际正整数 ifindex，接口重新创建后不会自动接管 |
| `NamespaceID` | 实际 `/proc/self/ns/net` 身份；重建 namespace 后要求重新授权 |
| `OwnerID` | 字面 ID，运行时 alias 必须等于 `reladock-shaping-v1:<OwnerID>` |
| `Dedicated` | 必须明确为 true；独占使用为运维承诺，不通过猜测接口名判断 |
| `ReplaceDefaultQdisc` | 默认 false；只有运维核对物理接口上的空内核默认 `fq/0:` 后才可设 true，不授权其他 root 替换 |
| `DefaultQdiscFingerprint` | `ReplaceDefaultQdisc=true` 时必填；同一节点上 `tc -j qdisc show dev <Device> \| sha256sum` 的 64 位小写摘要，任何输出变化都会拒绝交接 |
| `MarkNamespace` | 明确分配的非零 8 位 mark namespace，须由主 Agent 保证全路径不冲突 |
| `LinkMbps` | 1–100000，接口声明的物理调度预算，不是企业共享带宽套餐 |

`UploadEgress == DownloadEgress == Lease.Device`，不满足直接拒绝。外部路由与真实双向路径必须由主 Agent/部署验收证明；字符串相等不能证明真实流量路径。物理网卡专用与网络 namespace 独占、alias/handle 都是协作约定，不能防御另一个有 NET_ADMIN 的进程伪造或并发改写。

## 稳定分类与整形语义

规则编号精确计算为 `slot = bigEndian16(SHA256(nodeID+"/"+tenantID+"/"+ruleID)[:2]) % 65533 + 2`。Mark 为 `(uint32(namespace)<<24) | (slot<<8)`；class ID 为 `5244:<slot的十六进制>`。根 handle 固定 `5244:`，物理父类 `5244:1`，未分类默认类 `5244:ffff`。历史 class/mark 永不自动释放；历史碰撞也拒绝，避免旧 packet mark 被分配给另一条规则。16 位空间采用拒绝碰撞策略，实际容量受生日碰撞约束，不能按 65533 条无碰撞容量宣传。

`Plan.Classes` 输出规则/企业 ID、稳定 ClassID、Mark、Mask、CeilMbps、Active；`Plan.Markings` 输出每协议的同一 Mark/ClassID 以及 nft 字面选择和赋值语句。两个方向的 `ct direction { original, reply }` 共用同一 class；`both` 的 TCP/UDP 也共享它。Paused 规则保留受限类及编号，但不输出 nft 标记。删除/失败候选类同样保留，防止遗留 mark 落入无限制路径。暂停/删除的丢包职责仍由 dataplane 负责，本组件不宣称暂停能停止流量。

nft 标记需在验证本平台规则后、终止 accept 前，对**每个业务包**执行，并要求正确 `oifname`。表达式是 `meta mark set (meta mark & 0x000000ff) | <ruleMark>`。不保存/覆写 ct mark，不依赖 NAT 首包，不改低 8 位。主 Agent 还须确认这些高 24 位不与策略路由、其他 nft/iptables/tc 使用冲突。包不应用 NFT，只提供集成信息；不走该 egress 的方向不受其约束。

每规则 HTB leaf 使用极小的 `rate 8bit` 调度基数和 `ceil <BandwidthMbps>mbit`，均可从物理父类借用，规则之间不共享业务套餐。所有 leaf 基数总额小于最小物理预算。`quantum 1514`，burst/cburst 为 ceil 的 10ms 字节量且至少 1600 字节。上限是 token bucket 的时间平均约束，包含 burst、排队、丢包和实际链路开销；不是保证带宽，也不是每一毫秒严格无突发。MTU、大包、GSO/TSO/offload、吞吐与延迟仍需实际网络验收。

fw 精确过滤器使用 `mark/0xffffff00`、priority 10；保留 namespace 的未知 mark 由 priority 20 的 fw drop 拦截。非本 namespace 的未分类流量进入物理默认类，不受到某业务规则的限制；这不是允许业务包漏标，主 Agent 必须先冻结流量并验证所有业务包分类覆盖。

## 应用、幂等与跨阶段恢复

tc 命令是顺序执行的，**不存在 nft 式原子事务或 `tc -c`**。主 Agent 必须在 nft freeze 和计量最终采样之后调用 shaping，再替换 nft；本包不提供跨 tc/nft 的原子性，也不负责解除冻结。每条命令前都复核租约和已知树，拒绝外部 root、未知类/过滤器、改动的速率/ceil 和非预期 action。初次空 root 使用 `qdisc add`，竞争中出现外部 root 时内核报错。唯一 replace 例外是租约指纹精确绑定的空内核默认 `fq/0:`，并且必须在 Candidate 意图持久化之后替换为固定 HTB；不执行 root delete。仅允许平台已知树内 class/filter replace。

`Apply` 要求 generation 严格大于持久 `HighWater`。执行前原子持久化 `Pending`、完整 Candidate、历史编号和 `PriorGood = Good`；逐条安装后回读全部 root/classes/fw filters，成功才保存新 Good 与已验证计划摘要。编译、归属检查失败不执行 tc；首次意图写盘失败也不执行 tc。任何部分应用或最终提交失败都尝试运行时恢复，即使重新写恢复意图失败也不会提前跳过内核恢复。恢复使用独立 30s context，不被原请求取消抑制。

自动恢复保留 Good＋Candidate 所有历史类/mark；同规则采用两者较严格的 ceil。首次失败没有 Good 时仍保留受限候选类，不删除 root。只有完整回读和状态保存成功才清 Pending。任何未解决运行时/存储错误返回 `ErrRecoveryRequired`；全盘写失败不能保证持久 Pending，调用方必须保持 nft 冻结并人工处理。`ErrStoreUncertain` 标识 rename 后目录 fsync 等失败。无成功或健康假象。

`Ensure` / `ApplyVerified` 是同一接口语义：新 generation 正常应用；与持久 Good 完全相同的 generation/config 会重新回读，完整时不修改，缺少已知对象时先写恢复意图再重建并验证。相同 generation 不同内容拒绝；不允许借幂等入口修改旧配置或跳过验证。

`Verify(ctx,cfg) error` 是 nft 新流量开启后的**只读**验证入口：要求 config/generation 完整匹配持久 Good，检查 root、所有 class 的 rate/ceil 和 fw 分类，缺失/改动直接报错。不会调用 RunTC、不会 Save、不消耗 generation，不会像 Ensure 一样修复。新增测试覆盖 missing root/filter、修改 ceil、不同 config、Pending 和没有 Good，并断言命令与状态写入计数均不增加；真实 Docker 测试也调用此 Verify 回读现有树。不要在已开启流量后的检查阶段调用可能重建的 Ensure/ApplyVerified。

`Restore(ctx, oldPlan)` 仅接受 canonical 且摘要在持久已验证历史中的 Plan；保留当前所有编号，恢复旧业务分类与较严格限额，**不降低 HighWater**。受控恢复本身也先持久化 Restoring/Candidate；进程中断后 Recover 继续完成这个请求，不会反向恢复成较新的业务配置。它返回实际安装的安全 Plan，其 ceil 可能低于传入旧 Plan。

`PriorGood *Plan` 在新 Apply 执行前与意图一起保存，成功提交仍保留，支持进程重启后仅凭旧 NodeConfig 恢复。`RestoreConfig(ctx, cfg)` 规范化排序后只匹配持久 Good/PriorGood 的完整 config，并再次验证其历史摘要，随后走同一受控 Restore；不接受任意旧 cfg。更久历史只保存摘要，必须提供原完整已验证 Plan 调用 Restore，不能根据摘要反推历史配置。不存在自动历史回收接口。

`PrepareEmpty(ctx)` 为首次 nft apply 失败、dataplane 仍为空/冻结时使用：只检查接口与持久 Good 的完整运行树，不修改 tc/文件、不消耗 generation、不输出可启用转发的新分类。没有 Good 时也检查接口归属及外部 root。树不完整则报错，不会用 verify-only 接口偷偷重建。保留的 tc 类在无业务流量时是安全残留；未来更高 generation 可正常 Ensure。

建议编排：冻结平台流量 → 最终采样 → shaping.Ensure → nft replace → 业务验证；跨阶段失败时保持冻结，使用 shaping.RestoreConfig(oldNodeConfig)；原先没有 dataplane 配置时保持 empty dataplane 并调用 PrepareEmpty。只有整体 nft/tc/路径验证完成才由主 Agent 决定 ACK，本包不发送 ACK。

## Linux Backend 与持久状态

`NewLinuxBackend` 只允许 Linux 和绝对 tc/ip 路径，直接参数数组执行，不经过 shell，不构造 nft 或路由操作；默认不会被 Compile 创建。命令允许 show 和本组件需要的 root add、class/filter replace；Backend 是可信底层原语，调用方不可将任意用户命令直接交给它绕过 Adapter。

`SystemBackend` 实际读取 `ip -j -d link`、`tc -j qdisc/class/filter show`。校验 namespace、alias、ifindex、root HTB/default、每个 class 的 parent/rate/ceil、fw mark/mask/priority/classid/drop。兼容 HTB class 的 `class`/`kind`、顶层或嵌套参数、parent-scoped fw 回读省略 parent 的格式；部分 iproute2 在没有 class/filter 时输出空字节而非 `[]`，这两个明确的空清单视为空，qdisc 与其他 JSON 仍必须完整。tc 输出 rate/ceil 是 byte/s，转换到 bit/s 后验证。写执行器只允许固定 root add、显式授权路径使用的固定 root replace，以及 class/filter replace；delete、其他 root kind/handle 和非法接口名均拒绝。未知模式、offload 或未知 action 不视为成功；不根据 plan 数据伪造 readback。

FileStore 使用最终目录 0700、文件 0600、私有临时文件、文件 fsync、原子 rename、目录 fsync 和严格 JSON（8 MiB 上限）。锁使用同一 inode 的 flock，支持取消。状态含 Good、PriorGood、完整 Candidate、Restoring、HighWater、已验证摘要。所有控制同一租约的进程必须使用同一目录和锁；父目录须可信，禁止删除/替换锁 inode。最终 symlink、宽权限、非普通状态文件拒绝。不支持原 namespace/接口身份变化后的无授权自动接管。磁盘故障与断电的保证受文件系统影响。

## 验证方法与边界

```sh
RELADOCK_SHAPING_DOCKER=0 go test -race -count=1 ./internal/shaping
go vet ./internal/shaping
```

默认测试全部 mock ip/tc，覆盖注入/租约/外部 root、mark 稳定与碰撞、双向/双协议合并、暂停、历史编号、部分失败、持续磁盘失败、串行、同 generation verify/修复、受控 Restore、PriorGood 重启恢复、PrepareEmpty 不写与真实 iproute2 JSON 格式。持久状态/锁测试使用私有 tempdir；没有宿主网络修改。

显式 Docker 测试只使用**已存在本地镜像**，`--pull=never --network none --cap-drop=ALL --cap-add=NET_ADMIN`；临时容器 `reladock-test-shaping-<random>` 内创建 dummy 接口、nft 输出标记、真实 HTB/fw 树。无 host 网络/挂载/privileged、不建 Docker 网络、不下载镜像。结束删除精确命名的容器，镜像保留。中断时检查并清理该测试记录的容器名。

```sh
RELADOCK_SHAPING_DOCKER=1 \
RELADOCK_SHAPING_IMAGE=reladock-dataplane-test:local \
go test -count=1 -timeout=5m -v ./internal/shaping \
  -run '^TestDockerHTBReadbackAndCombinedRuleCeilings$'
```

真实测试核对 Linux readback，两个合成方向共用规则 A 的 2 Mbit/s class，另一规则 B 独立 4 Mbit/s；通过 tc 字节统计实际速率检验，另测同 generation verify、旧 Plan restore 和外部 HTB root 不被接管。**dummy 合成流量不是实际转发的双向路由验收**，不能据此声称不同出口可共享、真实业务标记覆盖或生产 ready。自动采样、计费、Agent 接线、tc/nft 整体恢复、真实转发吞吐均不是本包的交付。

参考原始接口：[iproute2 HTB](https://man7.org/linux/man-pages/man8/tc-htb.8.html)、[fw classifier](https://man7.org/linux/man-pages/man8/tc-fw.8.html)、[tc_class JSON](https://raw.githubusercontent.com/iproute2/iproute2/main/tc/tc_class.c)。

## 本次验证证据（2026-10-29）

- 单元测试先 red 后实现，包含编译器、应用、Linux JSON 格式、持久 Store/锁、Ensure、Restore、PriorGood/RestoreConfig、只读 PrepareEmpty。
- focused `go test -race -count=1 ./internal/shaping`、`go vet ./internal/shaping`、gofmt 检查通过；Linux amd64 测试二进制交叉编译通过。
- 已执行真实 `TestDockerHTBReadbackAndCombinedRuleCeilings`，使用既有 `reladock-dataplane-test:local`（镜像 ID `858aaa15b59d5914cdb4fc232f5ae19a1e000681ed2790af50ac41e681c52018`）、network-none 容器，仅 NET_ADMIN。测试 PASS，约 10.24s；规则 A 两个合成方向共用 2 Mbit/s class，tc 实测 **1.99 Mbit/s**；规则 B 独立 4 Mbit/s class，实测 **3.97 Mbit/s**。同 generation verify、旧 Plan Restore、外部 HTB root 保留也通过。cleanup 删除精确命名的临时容器，不删除镜像。
- 初始真实回读测试因 iproute2 顶层 HTB 字段／`class` 字段等格式差异失败，已补对应 red→green 单测并修复后重跑通过。负载生成器改用每流独立 socket，避免测试端共享发送缓冲造成假性规则饥饿。
- 未运行全平台 PostgreSQL 测试；未改 dataplane、Agent、platform 或部署文件。未接入主 Agent，未验证实际业务双向路由、整体 tc/nft 故障恢复、计量及 offload；不能宣称 production ready。最终检查为作者自审，主代理仍需集成复核。
