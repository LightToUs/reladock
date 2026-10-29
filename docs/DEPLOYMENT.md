# Linux 部署与验收

这些文件提供可重复构建和进程托管基础，不能替代下方验收。默认校验服务不会启用流量；独立 apply 服务会实际修改平台专用 nft/tc，必须在明确目标节点经过人工审核后显式启动。仓库和发布清单不宣称生产已验收。

## 构建

本机运行 `npm run build:release`，或设置 `RELEASE_ARCH=arm64`。输出到 `.cache/release/linux-amd64/` 或 `linux-arm64/`，包含 Go 静态二进制、SHA256SUMS、服务模板和配置示例。目标 Linux 主机运行这些程序不需要 Node.js 或 Go。构建目录不包含本机 `.env`、数据库内容或开发私钥。

## 控制平台安装

发布包内有 `install-linux.sh`：在目标机器核对后执行 `sh /path/install-linux.sh control /absolute/release-directory`；节点使用 `agent`。脚本检查 Linux/root/systemd 和二进制 SHA256，安装系统账号、程序及服务模板，保留已有环境文件。它不会自动启动或启用服务，也不会修改网络规则。脚本完成后填写实际配置和证书，再显式启动服务。

控制平台可用 `reladock --check-config` 校验当前进程环境和 TLS 文件，不连接数据库或监听网络；它不自动读取 env 文件，由 systemd EnvironmentFile 或受控启动器注入。

选择支持 systemd 的 Linux。准备独立 PostgreSQL（当前开发环境使用 PostgreSQL 17），限制数据库网络访问，为应用建立独立数据库及账号。生产数据库不要作为测试数据库。先完成备份再启动新版本；程序启动会自动事务迁移，版本降级不自动回滚数据库。

人工建立非登录系统用户 `reladock`，将二进制放入 `/opt/reladock`，配置放入 `/etc/reladock`。程序应由 root 管理且运行用户不可写。环境文件设 0600；证书及私钥仅允许服务账号读取。将 `deploy/reladock.service` 安装到 `/etc/systemd/system`，配置 `control.env.example` 中的实际值，再使用 systemd 启动。模板默认非特权 8443 和 Agent 9443；使用 443 需由运维明确配置端口映射或服务能力。

`PUBLIC_ORIGIN` 必须是浏览器实际访问的完整 HTTPS origin（包含非默认端口，无路径和末尾 `/`）。控制台直接终结 TLS 1.3；不信任 X-Forwarded-* 或 Forwarded 头。错误 Host 与跨 origin 写操作会拒绝。TLS 私钥和公钥证书必须成对配置；证书轮换后重启服务。生产 API 使用 PostgreSQL 共享限流；支付 webhook 另有单进程预算，多副本/高并发前仍须验收容量及上游入口限流。

独立 Agent 通道需要 `ca.crt`、`server.crt`、`server.key`，由与节点身份相匹配的 CA 签发。节点证书含 `spiffe://reladock/nodes/<id>` 和客户端用途，先在平台登记节点并绑定其公钥证书。服务器证书 SAN 必须匹配 Agent 连接域名。CA 签名私钥不得复制到节点或随发布包分发；本机 dev-enroll 证书不得当作生产证书。

## 节点安装

复制校验 Agent 二进制及本节点独立的 `ca.crt`、`agent.crt`、`agent.key`。配置 `agent.env.example` 并安装 `reladock-agent.service`。校验服务使用非特权账号，只有出站 mTLS 联系能力。实际转发命令与此服务分离；`docs/DATAPLANE.md` 描述 Adapter 的限制和验收，不能用校验通过代替规则已生效。

### 显式应用模式

先核对实际 IPv4 ingress 与 SNAT；控制平台的 `listenAddress` 必须为本节点实际地址，不可保留 `0.0.0.0`。客户目标须是可路由的非回环 IPv4；DNS 在受控应用时解析并固定，不承诺即时切换。平台不自动配置网卡、路由、ip_forward、卸载功能或云安全组。

节点须有 nft、iproute2（tc/ip）与 ethtool。运维先明确启用 IPv4 forwarding；完成专用接口与 mark namespace 授权，关闭 GRO/GSO/TSO/LRO，并确保无 flowtable/offload 绕过。运行时只检查这些条件，拒绝不匹配，不自动更改系统前置设置。严禁与另一 NET_ADMIN 程序共同管理该接口/namespace；软件归属标记不是对恶意特权进程的隔离。

创建 `/etc/reladock/shaping.json`，权限 0600，所有者服务账号可读。以下是格式说明，实际值需从目标节点核对，不可原样复制：

```json
{"Device":"eth0","IfIndex":2,"NamespaceID":"net:[REPLACE_ACTUAL_INODE]","OwnerID":"hk-01","Dedicated":true,"MarkNamespace":82,"LinkMbps":1000}
```

接口 alias 必须由运维明确设置为 `reladock-shaping-v1:<OwnerID>`。双方业务方向必须从同一租约接口发出，规则将通过 packet mark 共用一个 HTB 上限；不支持双 egress 共享一个桶，不自动安装 IFB 或修改路由。节点重建/网卡重建后 namespace/ifindex 不匹配会拒绝接管，须重新核对租约。

填写 `agent.env` 的实际 SNAT、接口、租约、二进制绝对路径和私有状态目录。默认单位永远只校验；应用选择独立的 `reladock-agent-apply.service`，由非 root 账号以最小 CAP_NET_ADMIN 运行，两单位互斥。安装器只复制模板，不启动它，不修改网卡。选择启动应用单位就是实际网络修改，必须在获授权的目标节点进行；本地 macOS 不支持 apply。

主 Agent 每周期保存真实 counter 并通过 mTLS 补传。WAL 满、磁盘不确定或 rollback 未完成时冻结平台业务或保留 applying 屏障；排障后由运维生成新快照，不能删 WAL/state 或直接清数据库状态。重启保留 `.control` journal 与 dataplane/shaping/WAL（实际控制子目录名 `control`）；恢复使用原 job/token，证书更换后仍须保留原状态。状态丢失的自动重建尚未实现，必须人工对账。35s 保守到期余量可能提前停止临近到期规则，不适用于秒级套餐。

删除已经下发的规则会等待移除确认，并继续保留端口/conntrack 隔离。当前不自动回收这类端口，不应以手工删除 allocation 来强行复用。参阅 [tc 契约](SHAPING.md)、[计量契约](METERING.md)、[探测边界](PROBES.md)。

## 备份与恢复

本地 Docker 开发数据库可运行 `npm run backup` 生成权限 0600 的 `.local/backups/*.dump`；再用 `npm run backup:verify -- /absolute/path/to/backup.dump` 恢复到独立随机数据库，校验版本与钱包/账本一致性后清理该临时数据库。原业务库和备份文件保留。这验证本地恢复流程，不代表生产离机备份、加密保存和 RPO/RTO 已验收。

数据库备份不包含各节点私有交付 journal、dataplane/shaping 状态和计量 WAL。生产须在节点进程停止并完成状态一致性核对后加密备份这些目录；PKI 恢复材料分开保存。不能只恢复控制数据库、丢弃 WAL 后将原周期当作全新计数，不能把旧快照随意应用到新 namespace。

使用 `pg_dump --format=custom` 对独立业务数据库做加密离机备份，数据库凭据通过受保护的 pgpass 提供，不写进命令历史。备份至少包含 schema、账本、订单及节点配置；TLS/CA 恢复材料分别管理。恢复先在独立临时数据库用 `pg_restore --exit-on-error` 验证，再核对企业/规则/账本行数、约束、钱包与不可变账本余额一致性，演练后记录实际 RPO/RTO。不要在业务数据库上直接尝试恢复或执行测试套件。

## 上线门禁

- HTTPS、管理员授权撤销、租户隔离、邀请与 API Key 实测。
- Linux 实际 TCP/UDP 转发、暂停/删除、节点重启、配置竞争及失败回滚。
- 每规则限速实测、独立上下行计量、计数重置、补传去重、配额耗尽与续期。
- 所选支付渠道回调验签、重复回调、金额/币种/订单校验、退款冲正和到账对账。
- 外部健康探测、UDP 未知状态、失联告警及备份恢复演练。

以上均需实测证据。Linux 构建通过不代表运行环境或链路已验收。
