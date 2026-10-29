# 节点成本与现金口径差额（v16）

本模块保存管理员明确录入的节点成本，并提供 `cash-basis` 运营报表。收入取实际套餐购买的钱包扣款账本，成本取报告期间录入的节点费用；不使用充值额、套餐目录售价或流量推算收入/成本。报表不是会计应计毛利，也不宣称已经核验供应商付款、银行现金流或完整经营利润。

## 主代理接线

迁移 16 已登记；后台“成本与经营差额”已接入真实 API、录入与分页查询。模块不提供任何默认价格或成本。

Store 接口：

```go
AdminRecordNodeCost(ctx context.Context, who Identity, key string, input NodeCostInput) (NodeCostEntry, error)
AdminNodeCosts(ctx context.Context, who Identity, from, to time.Time, page int) (Page[NodeCostEntry], error)
AdminCashMargin(ctx context.Context, who Identity, from, to time.Time, page int) (CashMarginReport, error)
```

调用 `serveCosts(w, r, who) bool` 时，应先执行现有 Host、同源、请求头、限流和会话鉴权，并在通用 `/api/admin/*` 分发之前调用它。读取使用 `adminReadTx`，不获取全局管理写锁；新增成本使用 `adminTx`。所有接口在数据库复核当前平台权限及企业状态，拒绝 API key 上下文。企业 owner/admin 不因企业角色获得访问权；权限撤销后旧 Identity 不能继续读取或录入。

## 不可变成本录入

`POST /api/admin/costs`，要求 `Idempotency-Key` 请求头：

```json
{
  "nodeId": "hk-01",
  "amountCents": 12500,
  "serviceFrom": "2026-10-01T00:00:00Z",
  "serviceTo": "2026-11-01T00:00:00Z",
  "invoiceRef": "supplier-invoice-123/hk-01"
}
```

金额必须是正整数 USD 美分，范围 1–9223372036854775807。不接受小数、零、负数或其他币种；响应固定 `currency: "USD"`。节点必须已存在。serviceFrom/serviceTo 均必须明确提供且后者晚于前者，表示服务期间 `[serviceFrom, serviceTo)`。时间统一为 UTC、截断到数据库微秒精度，支持 1970–9999 年。服务期间可以是历史或未来，只作凭据说明，不参与报表期间筛选和摊销。

invoiceRef 去除首尾空白后为 1–160 字符，拒绝控制字符，大小写敏感。同一个 `(nodeId, invoiceRef)` 只允许一笔成本。一个供应商发票明确覆盖多个节点时，可分别录入有明确依据的节点金额；系统不自动拆分、均摊或根据流量猜测分配。

幂等键格式与项目现有规则一致：8–128 位字母数字及 `._:-`，在平台成本表中全局唯一。相同键和规范化后完全相同的内容返回原记录（包括原 actor 和 recordedAt），不再写审计；相同键改内容返回 409。同节点相同 invoiceRef 换键也返回 409，不重复计费。不同节点可使用同一 invoiceRef，但仍需不同幂等键和明确的节点费用依据。

记录包含服务器从当前数据库身份取得的 actorId/actorEmail，不能由输入指定。成本、实际 actor 和 `platform_audit_logs` 中的 `node-cost.record` 事件同事务提交；审计失败时成本完全回滚。`recordedAt` 由数据库时钟生成，调用方不能覆盖或回填。

数据库触发器禁止 UPDATE、DELETE 和 TRUNCATE。没有编辑、删除、自动导入、负数冲正或伪造付款确认接口。当前只实现正数不可变基础，录错后的冲正流程尚未实现。

## 显式报告期间

- `GET /api/admin/costs?from=...&to=...&page=1`：不可变成本列表。
- `GET /api/admin/costs/margin?from=...&to=...&page=1`：平台汇总与节点分页。

from/to 都是必填 RFC3339 时间（可含时区/小数秒），归一为 UTC 微秒精度。采用半开区间 `[from,to)`：包含起点，不包含终点。必须 `from < to`，单次窗口最长 366 天。页码为 1–100000，每页最多 100 条；报告的平台合计始终涵盖全部节点，不能把当前页小计算作平台合计。

筛选基准明确为 `timeBasis: "recorded-at"`：

| 项目 | 时间列 / 金额来源 |
| --- | --- |
| 套餐购买收入 | `billing_ledger.created_at`，取实际 `subscription-debit` 的负数金额的相反数，并连接真实 `rule_subscriptions.ledger_id` 与 rule/tenant/price version 绑定。 |
| 节点成本 | `node_cost_entries.recorded_at`，取期间内录入记录的完整金额，不按服务期间重叠比例摊销。 |

因此，补录旧服务期间的费用会进入现在的录入期间，不能假装它是旧期现金支出。报表的 `basis: "cash-basis"` 是这里明确定义的内部套餐扣款/成本录入口径，不是应计收入确认，也不是充值到账现金流报表。

收入通过 `billing_ledger.rule_id -> relay_rules.node_id` 汇总；查询不排除软删除规则，因此规则删除不会抹掉历史购买收入。该关联依赖规则保留且节点归属不被重写；未来若增加规则迁移，需要先保存可靠的历史节点归属。当前不会按企业、用户数或流量分摊节点成本。钱包充值、支付退款、人工调整等非套餐购买账本不计入本模块收入；当前没有套餐退款净额重分类逻辑。

## 缺失成本与金额边界

响应包含 `totals` 和 `nodes: {items,total,page,pageSize}`。每个节点列出 `revenueCents`、`costCents`、`marginCents`、`hasCost` 和 `costCoverageComplete`：

- 节点期间内没有录入成本时，hasCost=false，costCents=null，marginCents=null；不把未知费用当成已填写的零成本。
- 节点有成本时，costCents 是录入成本之和，marginCents 是实际购买收入减该成本。
- 平台 hasCost 表示至少一个节点有成本；costCents 为已知成本总和，无任何成本时为 null。
- 平台 costCoverageComplete 仅在所有注册节点在该期间均有成本记录时为 true；否则平台 marginCents=null。一个无收入节点也可能有成本，不能自动按零处理。

coverageComplete 只表示每个节点至少有一条成本记录，不保证发票已齐全或金额经过财务审核。UI 应始终标注“现金口径”和“成本录入覆盖”，不得将已知费用小计直接标成完整毛利；不得把 JSON null 转成 0。

SQL 在取反和 SUM 前使用 `numeric`。节点及平台的收入、成本分别校验必须落在非负 int64 范围，超限返回输入/范围错误，不回绕、截断或给出部分成功报表。只有两个非负金额都通过界限校验后才相减。合计与节点页由一个 SQL 语句读取，保证同一 MVCC 视图，不会拼接不同时刻的金融数字。

HTTP 金额以 JSON 整数传输。前端处理超过 JavaScript 安全整数的金额时须使用无损整数解析/表示，不得转为浮点后四舍五入；UI 精度由主代理接线时处理。

## 验证

`costs_test.go` 使用真实 PostgreSQL 的独立随机 schema，覆盖企业/Key 越权、权限撤销、并发精确重放、幂等冲突与发票去重、实际 actor 来源、不可变触发器、审计失败回滚、充值排除、软删除规则历史收入、未录入/部分录入成本、recordedAt 半开边界和 numeric 聚合溢出。先写测试并确认缺少接口时失败，再实现并验证。

运行 `node scripts/start.mjs test -race ./internal/platform -run TestCosts -count=1`、`npm test` 和 `npm run vet`。本模块无外部网络、服务器采购、真实发票支付或客户凭据调用。
