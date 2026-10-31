# 加密离机恢复验收

本清单验证 RelaDock 加密恢复包可以在隔离环境中读取。工具不负责上传对象存储，也不自动覆盖运行中的数据库、PKI、WAL、journal 或数据面状态。只有把包复制到故障域之外、独立保管解密密钥并完成替换主机演练后，才可以记录实际 RPO/RTO。

## 前置条件

- 在可信运维机生成 `age` 身份；私钥不放在控制机、节点、Git、工单或恢复包目录。控制机和节点只持有 `age1...` 公钥。
- 安装 `/usr/bin/age` 及与服务器 PostgreSQL 主版本兼容的 `/usr/bin/pg_dump`、`/usr/bin/pg_restore`。RelaDock 不下载这些工具。
- 准备现有的绝对备份目录，只允许备份操作者写入。生成后由外部同步工具复制到另一个账号/区域并配置保留策略；RelaDock 不保存云存储凭据。
- 记录演练开始时间、数据库事务时间、节点最后成功计量 ACK、包 SHA-256、复制完成时间和操作者。

## 控制面包

在控制机从权限为 0600 的环境文件加载 `DATABASE_URL`，不要把密码写到命令参数或聊天中：

```bash
/opt/reladock/reladock-recovery control-create \
  --destination /绝对路径/加密备份目录 \
  --recipient age1替换为离线身份对应的公钥
```

工具在同一个 PostgreSQL 导出快照内读取 schema、钱包/账本不变量并生成 custom-format dump，随后写入 0600、禁止覆盖的 `.age` 文件。记录路径、大小和 SHA-256，再使用独立工具复制离机。成功生成本地文件不等于已经离机。

在隔离验证主机使用专用 PostgreSQL 演练集群和具有 `CREATE DATABASE`/`DROP DATABASE` 权限的受保护 `DATABASE_URL`：

```bash
/opt/reladock/reladock-recovery control-verify \
  --bundle /绝对路径/reladock-control-时间-随机值.age \
  --identity /绝对路径/离线取出的age身份文件
```

校验会创建随机临时数据库、恢复、检查当前 RelaDock schema 版本及钱包/账本不变量，然后删除该数据库。不得把演练连接指向生产集群；工具虽不会恢复到源数据库，但演练权限和负载仍应隔离。

## 节点包

节点包必须在维护窗口创建。先确认没有未处理的配置应用、retirement、升级或 WAL 磁盘故障，然后执行：

```bash
systemctl stop reladock-updater.timer reladock-updater.service
systemctl stop reladock-agent.service reladock-agent-apply.service
/opt/reladock/reladock-recovery node-create \
  --destination /绝对路径/加密备份目录 \
  --recipient age1替换为离线身份对应的公钥
```

工具要求 Linux root、四个单元均为 inactive，并非阻塞取得 `/var/lib/reladock-locks/control.lock`。它只收集 `/etc/reladock`、`/var/lib/reladock-agent` 和可选 `/var/lib/reladock-updater`；软链接、硬链接、设备、socket、变化中的文件或锁竞争都会失败。创建完成后，按原先选择启动 validation 或 apply 单元及 updater timer，并确认心跳、配置、WAL 和实际业务健康。

节点包只能解到全新的非 `/etc`、非 `/var` 绝对暂存目录：

```bash
/opt/reladock/reladock-recovery node-verify \
  --bundle /绝对路径/reladock-node-时间-随机值.age \
  --identity /绝对路径/离线取出的age身份文件 \
  --staging /tmp/reladock-node-restore-本次演练
```

验证不启动服务、不修改系统目录，也不声称内核 nftables/tc/conntrack 状态已经恢复。暂存内容晋升到替换 VPS 仍是人工双阶段操作：保持所有 RelaDock 单元停止，核对主机、接口、namespace、SNAT、shaping 租约和证书身份，按原所有者/权限复制到空主机，先启动 validation，确认控制链路后再单独授权 apply。旧节点仍可启动时不得同时启用同一身份。

## 通过标准与证据

- 至少一份控制包和每个节点的一份包存在于另一个故障域，远端 SHA-256 与生成输出一致。
- 身份私钥在独立介质/密钥系统中可恢复，错误身份无法解密；恢复操作者和访问记录可审计。
- 控制包在隔离 PostgreSQL 完成恢复与不变量检查，临时数据库随后消失。
- 节点包只解到新暂存目录；替换主机先 validation 后 apply，逐规则计量 continuity 已人工对账。
- 记录从最近可用包到故障时点的数据损失作为 RPO，从宣布恢复到验证业务链路的时长作为 RTO。
- 记录未覆盖项目：对象存储耐久性、CA/age 密钥托管、真实 VPS 容量、普通 UDP 端到端健康和客户协议健康。

未执行真实离机复制和替换主机演练时，发布清单必须继续标记 `productionReady=false`，不得把本地成功校验描述为生产灾难恢复完成。
