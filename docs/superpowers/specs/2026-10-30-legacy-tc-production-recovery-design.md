# 旧版 tc 兼容与生产恢复设计

## 目的

让已授权的入口 Agent 在 iproute2 5.15 环境中安全回读 HTB class，停止重复累积计量冻结规则，并沿用现有持久化恢复流程解除生产节点的应用屏障。恢复后，只有具备有效逐规则套餐周期的规则才允许转发。

## 已确认现场

- `node-01` 使用 iproute2 5.15；`tc -j qdisc` 输出 JSON，但 `tc -j class` 输出 `class htb ...` 文本。
- Agent 将 class 文本交给 JSON 解码，因首字符 `c` 失败；generation 5 保持 `applying`，generation 7 排队。
- 每次失败后的 fail-closed 冻结都会再插入一条 `reladock-meter-freeze`，形成重复 drop。
- 当前业务规则是 `103.30.78.63:20001/UDP -> 149.33.27.162:443/UDP`，但没有套餐价格版本或规则订阅，因此最新快照按设计下发为 `paused`。
- 入口 IPv4 转发、SNAT、路由和目标 ICMP 可达性正常；UDP 服务健康只能由真实协议握手确认。

## 设计

### HTB class 兼容

保留 JSON 为首选格式。仅当 `tc -j class show` 的非空输出不是 JSON 数组时，进入严格的旧版 HTB 文本解析：

- 每个非空行必须以 `class htb` 开头。
- 只接受唯一 class ID、`root` 或 `parent <id>`、`rate <值>`、`ceil <值>`，并拒绝缺失、重复、未知 class 类型和无法解释的速率。
- 支持 tc 可能输出的十进制 `bit/Kbit/Mbit/Gbit/Tbit`，转换为 bits/s，并检查溢出。
- 文本解析结果继续经过现有 lease、class、rate、ceil、重复对象和外部对象校验；兼容层不放宽所有权或覆盖规则。
- 空输出仍表示没有 class；看似 JSON 但格式错误时不回退文本，避免把损坏 JSON 误当成合法状态。

### 幂等冻结

在插入 `reladock-meter-freeze` 前，读取已验证为平台所有的 table：

- relay 链已经存在至少一条合法的带该 comment 的 drop 时，视为已冻结，不再插入。
- 同 comment 但不是 drop、位于错误链或结构异常时拒绝，不能凭 comment 猜测安全状态。
- 没有冻结时仍以单条 nft 事务插入，然后才读取计数。
- 既有重复冻结不在失败路径中逐条删除；下一次成功的原子表替换会清理它们，避免恢复前破坏计数与物理状态。

### 生产恢复

发布新固定版本和摘要，先升级控制机发布元数据，再升级入口 Agent。重启后的 Agent 使用现有 journal：先恢复 generation 5 的本地 shaping/dataplane，再对同一 token/checksum 做精确重试并回报；不得手工清除 `applying` 或删除状态文件。

恢复到最新暂停快照后，创建明确的测试套餐版本并通过现有钱包/购买事务给当前规则建立有效周期。测试资金若使用本地调整，必须有审计记录，不伪装成真实支付。只有新 generation applied、nft/tc 回读正确后，才交付 VPN 客户端入口。

## 非目标

- 不终结或解密 VPN 协议。
- 不绕过套餐、额度、到期或应用屏障。
- 不把 ICMP、Agent 在线或 applied ACK 表示为 UDP/VPN 端到端健康。
- 不自动升级系统 iproute2，不清空宿主防火墙，不手工重置计量/WAL。

## 验收

- iproute2 5.15 的真实 class 文本 fixture 可解析，格式偏差和溢出 fail closed。
- JSON class 路径保持兼容。
- 重复 Observe/恢复失败最多保留一条新冻结，不继续增长；已有重复冻结仍被识别为已冻结。
- 全套 PostgreSQL、Go race、Node、vet、Linux 隔离数据面测试通过。
- 生产 generation 5 最终终结，最新 generation applied；入口出现 UDP 20001 的 DNAT/SNAT、mark、tc class/filter，重复冻结被成功替换清理。
- 真实 VPN 客户端握手另行作为端到端验收，UDP 健康在此之前保持未知。
