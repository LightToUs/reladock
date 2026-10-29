# 可靠计量 WAL 与集成契约

2026-10-29：已通过 `dataplane.MeterHooks` 与应用 Agent 组合。替换/回滚前冻结平台链并最终采样；每次实际内核计数重建有独立 incarnation。隔离容器实测真实转发计数、周期归属、配置替换不刷新额度及内核配额/到期阻断。此证据不替代生产 offload、差异和吞吐验收。

`internal/metering` 是独立 Go 包，不导入 platform/dataplane/agent，不执行 nft、tc 或网络请求。它解析主代理提供的完整 named-counter 快照，把逐规则、逐周期、逐物理 epoch 的累计报告和 sequence 持久化，网络 ACK 后才移除报告。主代理负责真实内核 plan 的验证、数据面锁、freeze、配置替换、恢复和 mTLS 传输。

## 公共接口

```go
type Counter struct {
    Name, RuleID, SubscriptionID, Protocol, Direction string
}
type Reading struct {
    Counter
    Bytes, Packets int64
}
type Options struct {
    NodeID string
    MaxBytes int64
    MaxPending, MaxStreams int
}

func ParseCounters(raw []byte, expected []Counter) ([]Reading, error)
func Open(dir string, options Options) (*WAL, error)

func (*WAL) BindPeriod(ctx context.Context, ruleID, subscriptionID string,
    baselineUpload, baselineDownload int64) error
func (*WAL) CumulativeFor(ctx context.Context, ruleID, subscriptionID string) (
    uploadBytes, downloadBytes int64, err error)
func (*WAL) BeginEpoch(ctx context.Context, readings []Reading) (string, error)
func (*WAL) BeginEpochFor(ctx context.Context, incarnation string,
    readings []Reading) (string, error)
func (*WAL) Sample(ctx context.Context, readings []Reading) error
func (*WAL) Current() (epoch, incarnation string, err error)
func (*WAL) Next() (Record, bool, error)
func (*WAL) Ack(ctx context.Context, receipt Receipt) error
func (*WAL) Flush(ctx context.Context, sender Sender) error
func (*WAL) Close() error
```

`Report` JSON 字段与用量接收 API 一致：`ruleId`、可空的 `subscriptionId`、`epoch`、`sequence`、`uploadBytes`、`downloadBytes`。规则/周期标识使用小写 UUID；NodeID 是明确配置的节点身份。租户归属不来自此 payload，接收端仍从数据库核验。生产付费规则必须提供配置中的 SubscriptionID，不能把旧 WAL 改标为新周期。

`Record` 包含解析后的 `Report` 与独立复制的 `Payload []byte`。发送时使用 Payload 原始字节，不重新构造 JSON。`Receipt` 只要求接收端确认的 ruleId/subscriptionId/epoch/sequence；它不替代服务器对报告内容和累计计数的验证。

```go
type Sender func(context.Context, []byte) (Receipt, error)
```

Sender 只能在正确 mTLS 身份下获得成功 HTTP 响应、严格解析并确认持久化 ACK 后返回 Receipt。超时、非成功响应和不确定结果都返回 error。Flush 顺序发送队首，并匹配四个身份字段后调用 Ack；错误响应不会删除、跳过或重排报告。禁止把“请求已发出”视为 ACK。单独使用 Ack 时，调用方承担相同的网络确认前提。

## Plan 到 counter manifest 的映射

主代理遍历实际已验证内核 plan 的 `Plan.Counters`，复制 Name、RuleID、Protocol、Direction，并从对应 `ConfigRule.SubscriptionID` 补充周期 ID。不要使用准备下发但尚未生效的 candidate 映射读取旧内核计数。

解析器读取 `nft -j list counters` 的 JSON，只接受 `family=ip, table=reladock` 的 expected named counters。也可传入包含这些对象的完整 table JSON。其它表忽略；平台表中缺失、重复、额外的 counter，缺少 bytes/packets、负数、浮点数、重复 JSON key、整数溢出或单规则合计溢出均拒绝。每个 TCP/UDP 协议必须同时有 original 和 reply；两者分别累计 upload、download。`both` 由编译器展开为 TCP+UDP 后汇总到同一规则。

此映射只能来自已验证的 forward filter 全业务包计数器；NAT 链首包、链路总计或消费面板数字不能替代。解析器不证明内核表达式的正确性。读取命令的进程退出状态、输出上限、实际内核 plan 身份和完整性由主代理验证。旧 ExecRunner 若未允许只读 counter 命令，需在主代理的白名单中单独集成；本包不会绕过它运行命令。

## 周期基线与离线 quota

生产路径必须在首次采样/BeginEpoch **之前**执行：

```go
err := wal.BindPeriod(ctx, rule.ID, rule.SubscriptionID,
    rule.UsageUp, rule.UsageDown)
up, down, err := wal.CumulativeFor(ctx, rule.ID, rule.SubscriptionID)
```

第一份平台累计值固定为该 rule/subscription 的基线。后续有效快照重绑成功但忽略新基线：新快照可能已包含本机刚 ACK 的用量，重复相加会扣两次。记录同时保存固定基线与本机实际增量；两者不能跨周期或规则混用。

`CumulativeFor = 首次固定基线 + 所有本机物理 epoch 的采样增量`。ACK、旧 stream 清理、配置替换、进程重启都不清零这一值。主代理每次重建内核 quota 时从配置额度减去该有效总量，并检查上限/耗尽；不能把替换后的新 counter 零值当作整期使用量归零。上报 payload **不包含平台基线**，避免向接收端再次上报已计量流量。

直接 BeginEpoch 的新开发 WAL 会明确建立固定零基线，以保留原 API；之后 BindPeriod 也不会修改这个零值。生产不可依赖此兼容行为。未知且格式合法的 rule/subscription 查询返回 0；生产必须先绑定，不能据此推断历史使用量为零。

各周期总量最多 MaxStreams 条，保守保留已结束、已 ACK 周期；没有自动淘汰 API。满时 ErrFull 并停止推进，不能通过删除旧周期事实换取免费额度。baseline+本机增量、上下行合计均限制在非负 int64 范围。

## 物理 epoch、freeze 与恢复

1. 取得主代理跨进程数据面锁，核验实际内核 plan/实例与 WAL.Current。WAL 自身锁不替代数据面锁。
2. 在替换前对实际旧规则取得稳定计数。若主代理使用 freeze，应先持久化自身 Pending，再仅冻结平台的 exact-allocation 路径，然后读取计数。
3. ParseCounters 后 Sample；必须成功持久化后才能销毁旧 counters。原始各方向/协议 bytes 和 packets 任一回退都返回 ErrCounterReset；其它 counter 的增长不能掩盖回退。
4. 确认新内核计数器实例确实已建立后，读取新 counters，执行 BeginEpochFor。新 epoch 第一份值从物理零点累计，包含创建后至首次读取的流量。不得从首次读数减去“基线”而丢弃这部分用量。
5. 同一个 incarnation 重试只继续 Sample，不创建新 epoch。incarnation 必须标识实际的一次计数器重建（包括回滚重建），不能只使用回滚可能复用的配置 generation。允许字符为 `A-Za-z0-9._:/-`，1–128 字节；旧非当前 token 不得再次复用。
6. 回滚也会重置 counters。主代理须识别实际 kernel plan，先冻结并持久采样 candidate，再建立恢复实例的新 epoch。未确认实际 reset，不能调用 BeginEpoch 来掩盖未知计数回退。

新规则、周期、协议或 counter 名称集合变化时，Sample 返回 ErrEpochRequired；映射在一个 epoch 内固定。BeginEpoch 是无 incarnation 重试保护的显式 reset API，生产恢复优先使用 BeginEpochFor。

每个 rule/epoch 的 sequence 从 1 开始，只有累计字节变化才增加；同一读数不重复排队。仅 packets 变化时更新持久游标而不产生新财务报告。完整多规则采样要么全部提交，要么全部不变。

样本持久化不等于替换操作原子化。本包不承诺采样到替换之间无丢包/无计量差额；freeze 的正确性、所有 writer 的锁协作、在途包、offload 和失败恢复均需主代理/实际 Linux 路径验证。外部未协作 reset 后若计数已增长超过旧读数，单靠数字无法识别。不能宣称命令成功即代表业务交付、实际停流或端到端健康。

## 有界存储与故障处理

| 设置 | 默认值 | 硬范围 |
|---|---:|---:|
| MaxBytes | 4 MiB | 4096 字节–64 MiB |
| MaxPending | 1024 | 1–100000 |
| MaxStreams | 4096 | 1–100000 |
| counter JSON | 8 MiB | 固定上限 |
| counter 数 | 16384 | 固定上限 |

active/backlogged stream 数和永久保留的周期总量数分别不得超过 MaxStreams；最终编码后的文件还受 MaxBytes 限制。报文和游标元数据都计入限制。上限存入 WAL，重开时必须保持一致，不隐式迁移容量设置。

目录必须为绝对路径、私有目录（新建 0700）且父路径可信。仅创建最后一级目录。文件为私有普通文件（新建 0600），拒绝软链接/硬链接与不安全权限。Linux/macOS 持有非阻塞 OS flock，第二个 owner 返回 ErrLocked；同一进程的 hooks 和 sender 共用一个 WAL 实例。网络发送在 WAL mutex 外，不需要持有数据面锁。

存储使用有界快照式 WAL：只有 `wal.json`、固定 scratch `wal.next` 和 `wal.lock`。每次提交先完整写 scratch → 文件 fsync → 原子 rename → 目录 fsync，成功后才推进内存状态。两个镜像文件的逻辑总大小最多约 2×MaxBytes，不产生无限增长的随机临时文件。checksum 和结构约束共同验证读回状态。

重启只信任提交镜像，绝不按时间戳提升 scratch。丢失/损坏提交文件、有游标却缺少基线、schema 不支持等情况 fail closed，不自动清空重建。当前 WAL 格式 **v3**；v1/v2 不能推导已 ACK/prune 的基线，必须人工对账，不能猜零迁移。整个目录被外部删除无法仅由库检测；主代理不得用一个空新 WAL 自动接管仍运行的旧数据面。

任何存储错误返回 ErrUncertain 并拒绝当前实例后续操作，必须 Close/Open 重新核验。rename 可见但目录同步失败时，新旧完整提交都可能成为恢复结果；重放保持幂等，不能假设旧状态仍在。context 取消或网络结果不确定时保留报告。

ErrFull 不淘汰事件或推进 sequence/累计值。网络长时间离线时容量有界必然产生背压；主代理应报警、停止配置切换或保持冻结/恢复状态，待 ACK 腾出空间。不能忽略 Sample/BeginEpoch/BindPeriod 的错误继续删除原 counters。AfterReplace 初始化失败尤其不能再次销毁尚未持久采样的新实例；需要可恢复地保持/冻结它并重试，不回报 applied。有限 WAL 不承诺无限离线运行。

ACK 后只移除已确认队首，可清理没有待发报告的旧 stream；当前游标、sequence、高水位 epoch 编号、固定基线和周期总量均保留。重复、错序或跨周期 ACK 返回 ErrAck，不能删除其它报告。

## 验证范围

包内测试使用私有临时目录和构造的 nft JSON；覆盖全协议方向、缺失/负数/溢出/歧义输入、重放同 payload、reset、周期隔离、fsync 提交边界故障、容量背压、ACK 失联/取消、并发原子采样、进程退出、锁和文件安全、固定平台基线、离线累计跨 ACK/prune/重启保留，以及旧格式拒绝加载。

```sh
node scripts/start.mjs test -race ./internal/metering -count=1
node scripts/start.mjs vet ./internal/metering
```

本包测试不使用 PostgreSQL、不操作宿主网络、不执行 nft/tc。Linux 交叉编译仅证明编译兼容，不替代真实 Linux fsync/断电/内核/全链路验收。主代理的 Docker 集成结果应独立记录；不能从这些单元测试推导生产计费完整性或替换中无丢包。
