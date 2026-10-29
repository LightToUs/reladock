# 独立目标与合作 endpoint 探测

`internal/probe/` 已实现真实 TCP connect 与显式允许的 HTTP/S endpoint 请求。包只返回观测结果，不修改规则、不记录业务载荷、不启动服务或调度器；尚未接入 platform、web、Agent、数据库、ACK 或告警。不得把这个独立组件视为已完成规则端到端健康验收。

## 接线接口

```go
func New(config Config) (*Prober, error)
func (p *Prober) Run(ctx context.Context, request Request) (Result, error)

type Config struct {
    AllowedEndpoints []string
    RootCAs          *x509.CertPool
}

type Request struct {
    NodeID     string
    RuleID     string
    Generation int64
    Type       Type
    TargetHost string
    TargetPort int
    Endpoint   string
    Timeout    time.Duration
}
```

`New` 无网络操作，复制 endpoint 清单和显式信任根。构建后 `Prober` 可并发使用。调用方负责授权规则归属、实际目标、执行位置及合作 endpoint；不能直接把未经授权的用户 URL 或目标提交给本包。`nodeId` 必须符合节点 ID 格式，`ruleId` 必须为小写 UUID，`generation` 必须大于 0。这些元数据用于关联观测，不构成身份认证；主代理仍须用可信配置和上报通道绑定身份、拒绝陈旧 generation。

- TCP/UDP：设置 `TargetHost` 与 `TargetPort`，`Endpoint` 必须为空。目标可为合法 DNS 名或 IP，端口 1–65535；拒绝通配地址、多播、广播地址、IPv6 zone 和包含路径/凭据的主机名。企业私网和回环目标允许使用。
- HTTP/S：`Type` 为 `probe.HTTP`，设置 `Endpoint`；目标主机和端口字段必须为空。endpoint 必须与 `Config.AllowedEndpoints` 中的完整字符串精确相等；同一个 host 的其他路径、端口或 scheme 不自动获准。
- `Timeout=0` 使用 `DefaultTimeout`（3 秒）；非零值须大于 0 且不超过 `MaxTimeout`（10 秒）。调用方更短的 context deadline 优先。总预算覆盖 DNS、连接、TLS、响应头和响应体读取。

主代理应在实际探测位置执行并限制调度并发及频率。包内不提供自动周期、重试、跨节点任务分发或持久化。

## 结果与语义

```go
type Result struct {
    NodeID        string    `json:"nodeId"`
    RuleID        string    `json:"ruleId"`
    Generation    int64     `json:"generation"`
    ObservedAt    time.Time `json:"observedAt"`
    Type          Type      `json:"type"`
    Status        Status    `json:"status"`
    LatencyMillis *float64  `json:"latencyMillis"`
}
```

`observedAt` 是执行进程在完成观测时记录的 UTC 时间；消费端需评估时钟偏差与结果新鲜度。`latencyMillis` 是本次操作的实际总耗时（毫秒，保留小数），不是业务 RTT 或转发延迟。UDP 与调用方取消没有可用测量，值为 null。

| Type / status | 说明 |
| --- | --- |
| `tcp` / `targetReachable` | TCP 连接成功后立即关闭，不发送应用数据。只证明该执行位置能连接实际目标，不证明协议正常、转发规则已应用或端到端可用。 |
| `tcp` / `targetUnreachable` | 本次 TCP 连接失败或在探测自身预算内超时；不是对节点、路由或整个客户协议的最终诊断。 |
| `http` / `endpointResponsive` | 显式合作 endpoint 返回完整、未超限的 2xx 响应；HTTPS 还须通过证书与主机名校验。只代表此 endpoint 的响应。 |
| `http` / `endpointUnresponsive` | 连接/TLS/读取失败、探测超时、非 2xx、响应头/正文超限或不支持的编码。不能因此直接判断业务 UDP 或整条转发链路失败。 |
| `udp` / `unknown` | 普通 UDP 没有通用健康握手，本包既不 dial 也不发包。不会用 UDP socket 创建成功伪造健康。 |
| 任意 / `unknown`，同时返回 context error | 调用方取消或调用方 deadline 已到，属于未完成观测，不能记作健康失败。 |

网络故障以结果 status 表达，`error=nil`。输入/配置无效返回 `ErrInvalid` 且无 I/O；取消返回 `ctx.Err()`，status 为 `unknown`。调用方只应消费无输入错误的结果。结果中没有 URL、目标地址、HTTP 状态码、原始网络错误、响应头或正文，不应额外记录包含客户目标/凭据的错误信息。

## HTTP/S 的固定约束

允许清单最多 64 项，仅接受 `http` / `https` 绝对 URL。URL 最长 2048 字节，path 为 1–256 字节的显式绝对路径，只允许 ASCII 字母数字及 `/._~-`。拒绝 userinfo、query（包括空 `?`）、fragment、百分号转义、反斜线、重复斜线、`.` / `..` 路径段、非法主机及非法端口。因而不支持 URL token、签名参数或路径编码的变通认证。

请求固定为无正文的 GET，只使用库的固定 User-Agent，不接受任意 header、Cookie、Authorization、客户端证书或用户自定义 TLS 配置。不使用环境代理、不持有 cookie jar、不跟随任何重定向（即使目标也在允许清单中），不复用连接；没有应用层重试。

响应头最多 8192 字节，正文最多 4096 字节。正文只做有界丢弃，不保留或解释内容；超出上限返回 `endpointUnresponsive`。禁用自动解压，拒绝非空且非 `identity` 的 Content-Encoding，避免压缩正文扩张；慢响应同样受总超时限制。

HTTPS 至少使用 TLS 1.2，默认信任系统根证书；私有合作服务可显式提供 `RootCAs`。没有跳过证书/主机名校验的开关。HTTP 是明确允许的合作探测方式，不提供 TLS 身份保证。

允许清单约束的是调用目标配置，不是完整的网络隔离：DNS 名按操作系统解析，未实现 DNS/IP 固定、DNS rebinding 防护或地址级出站 ACL。企业私网/回环本来可能就是合法目标，不能在此包统一封禁。主代理若以后公开 endpoint 配置入口，须在授权层和执行网络中实施与租户、合作服务相符的出站限制；不能让租户自由扩大本包允许清单。

## 本地验证

```sh
GOPROXY=off GOSUMDB=off node scripts/start.mjs test -race ./internal/probe -count=1
```

测试仅创建 `127.0.0.1` 回环 TCP、UDP、HTTP、HTTPS 服务，无外部网络目标、真实客户目标或真实证书依赖。覆盖 TCP 成功/拒绝及不发送应用数据，UDP 无数据包且保持 unknown，endpoint 允许清单、重定向、凭据缺省、URL/path 验证、正文/响应头限制、HTTP 状态、慢头/慢正文超时、可信/不可信 TLS、调用取消与并发调用。HTTP/S 的成功不构成 UDP 协议探测或中转端到端测试。

按要求先加入测试并确认因缺少接口失败，再实现该包并验证通过。项目回归仍使用 `npm test`（真实 PostgreSQL 随机 schema、Go race 和 JS 测试）及 `npm run vet`；这两个命令的通过也不能替代外部链路或生产网络验收。
