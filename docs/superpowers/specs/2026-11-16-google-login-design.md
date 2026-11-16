# Google 授权登录设计

## 意图

为 RelaDock 现有企业成员增加可选的 Google OpenID Connect 登录方式，减少密码登录摩擦，同时保持现有企业、成员、角色、平台管理员授权、会话撤销和租户隔离边界不变。

首版只服务已经存在于 `members` 表中的账号。Google 首次返回的已验证邮箱只用于建立短期绑定挑战，不能单独证明现有 RelaDock 账号归属；用户必须再输入同邮箱现有账号的密码。密码验证与绑定事务完成后，后续登录只以 Google 稳定的 `sub` 标识查找成员。未知邮箱不会自动创建企业或成员，也不会从 Google 资料推断 company、role 或平台权限。

## 已确认范围

- 保留邮箱密码登录、注册、邀请和修改密码；Google 登录是附加入口。
- Google 登录默认关闭。只有完整配置 Client ID 与 Client Secret 时才启用；部分配置必须拒绝启动。
- 企业控制台和平台后台登录页都提供 Google 登录入口，分别只允许返回 `/` 与 `/admin`。
- 首次登录要求 Google ID Token 中存在规范邮箱且 `email_verified=true`；回调只创建 10 分钟绑定挑战，不创建会话。用户再提交同邮箱现有 RelaDock 账号密码，验证成功后才原子绑定 `provider=google` 与 `sub`。
- 已绑定账号以后只按 `(provider, subject)` 登录；Google 邮箱变化不会改写 RelaDock 成员邮箱，也不会把身份迁移给另一成员。
- 登录成功创建现有 24 小时 RelaDock 会话。当前数据库角色、企业状态和 `is_platform_admin` 每次重新读取，Google 身份不能授予或提升任何权限。
- 不保存 Google access token、refresh token、授权码、ID Token、头像或姓名。完成身份验证后立即丢弃提供方令牌。
- 不提供解绑、账号合并、仅 Google 注册、Google Workspace 域限制或 One Tap；这些需要单独需求。

## 协议与安全边界

使用服务端 OpenID Connect Authorization Code 流程，请求最小 scope：`openid email`。授权请求包含高熵 `state`、`nonce` 和 PKCE `S256` challenge；回调必须验证：

1. 回调中的 `state` 与短期 HttpOnly、Secure（HTTPS 时）、SameSite=Lax Cookie 完全一致；
2. 数据库中的 state 摘要存在、未过期、未消费，且只能原子消费一次；
3. token 交换使用相同 redirect URI 和 PKCE verifier；
4. ID Token 签名、issuer、audience、expiry 和 nonce 全部有效；
5. `sub` 非空、邮箱可规范化且 `email_verified=true`。

RelaDock 主会话 Cookie 继续使用 SameSite=Strict。只有短期 OIDC 尝试 Cookie 使用 Lax，以便 Google 顶层导航回调时携带；该 Cookie 只作用于回调路径，有效期 10 分钟，成功或失败后立即清除。未绑定 subject 的回调另签发 10 分钟 HttpOnly、SameSite=Strict、仅作用于 `/api/auth/google/link` 的随机绑定 Cookie；数据库只保存其 SHA256 摘要。

授权发起使用同源 `POST /api/auth/google/start`，继续要求 `X-Requested-With: RelaDock` 和现有 Origin/Host 检查。请求体只接受枚举返回目标 `/` 或 `/admin`。服务端返回 Google 授权 URL，浏览器再导航；不能提交任意返回 URL，避免开放重定向。回调为 `GET /api/auth/google/callback`，使用 Google 控制台登记的精确 URI `${PUBLIC_ORIGIN}/api/auth/google/callback`。

Google/OIDC 网络客户端必须有短超时、禁止环境代理承载 Client Secret、限制响应大小、拒绝非成功状态和重定向到非预期主机。提供方错误只向用户显示有界通用文案；令牌、code、state、verifier 和原始响应不进入日志、审计或 HTTP 错误。nonce 按协议包含在 Google 授权 URL 中，但不得进入应用错误或代理日志。前置代理必须对精确 callback 路径同时处理 access/error log，或做等价查询串脱敏。

## 数据与事务

新增 schema v41：

- `member_external_identities`：`provider`、`subject`、`member_id`、创建时间；`(provider, subject)` 唯一，且首版同一成员最多绑定一个 Google subject。
- `oauth_login_attempts`：state 摘要、PKCE verifier、nonce、固定返回目标、过期/消费时间和创建时间；不保存明文 state。过期尝试可由登录路径顺带有界清理。
- `google_link_challenges`：随机绑定令牌摘要、Google subject、规范邮箱、固定返回目标、过期/消费时间和创建时间；不保存明文绑定令牌。过期挑战可由回调路径顺带有界清理。

回调验证成功但 subject 未绑定时，只保存挑战摘要并返回固定目标的 `googleAuth=link` 状态，不查询现有成员或创建会话。`POST /api/auth/google/link` 必须携带挑战 Cookie、同邮箱和现有密码。首次绑定与会话创建在一个数据库事务内完成：锁定平台授权顺序与挑战，重新核验密码、企业 active、邮箱、当前角色与平台标志，插入外部身份，消费挑战，创建会话，并写 `account.google_link` 与 `account.login` 审计。任一步失败全部回滚；错误密码不会消费挑战，可在认证限流内重试。并发首次登录只能有一个绑定写入，另一个请求在密码验证后读取同一绑定并安全完成登录；同一 subject 已属于其他成员或同一成员已有其他 Google subject 时统一拒绝。

已绑定登录同样在事务内重新读取当前成员和 active 企业、清理过期会话、创建新会话并写 `account.login`。企业停用、成员角色变化、平台权限变化和现有会话撤销逻辑不修改外部身份记录；未来 Google 登录仍以当时数据库权限为准。

## 配置与运行边界

新增环境变量 `GOOGLE_OIDC_CLIENT_ID` 与 `GOOGLE_OIDC_CLIENT_SECRET`。两者全空表示关闭；只配置一项或 Client ID 格式明显无效时 `--check-config` 和正常启动都失败。redirect URI 从已验证的 `PUBLIC_ORIGIN` 派生，浏览器不能覆盖。

认证实现通过小型 provider 接口隔离 OIDC 库与 HTTP 路由，使自动测试使用本地替身，不访问 Google。生产实现使用成熟的 Go OAuth2/OIDC 验证库完成授权 URL、token 交换、远程签名键缓存与 ID Token 校验，不自行实现 JWT 加密算法。

## 界面与失败行为

登录/注册卡片增加分隔线和“使用 Google 登录”按钮；平台后台登录卡片增加同一按钮。页面加载公开状态接口决定是否显示，关闭时不展示不可用入口。

已绑定 subject 的成功回调设置 RelaDock 会话并以 303 返回固定目标。未绑定 subject 的成功回调只设置绑定挑战并以 303 返回固定目标的 `googleAuth=link`；页面要求输入现有邮箱和密码，提示位于可隐藏的 Google 按钮容器之外。拒绝授权、state/nonce 不匹配、尝试过期、提供方不可用、邮箱未验证、密码错误、未知成员或绑定冲突均不创建会话、不绑定账号，并返回有界通用错误；页面从地址栏清除枚举参数。未知邮箱不得泄露更细的企业或成员信息。

## 验证与交付边界

测试必须先失败再实现，并覆盖：配置全空/部分配置、授权 URL 与 PKCE/state/nonce、Cookie 属性、回调重放、过期尝试、错误 state、无效 token 声明、未验证邮箱、未知邮箱只产生挑战、正确/错误密码、挑战过期与重放、并发绑定、subject 冲突、已绑定后邮箱变化、停用企业、平台管理员既有权限、审计失败回滚、令牌不落库/不回显、回调查询串不写 Nginx access log、企业与平台返回目标以及静态页面入口。

完整验证运行 `npm test`、`npm run vet` 和 `git diff --check`。本功能只修改控制面与登录界面，不连接真实 Google 项目、不写入真实 Client Secret、不部署生产、不改变 Agent、数据面、支付或客户流量。真实 Google Cloud OAuth consent screen、授权 Client、redirect URI 和生产浏览器回调需要用户另行提供配置并授权部署后验收。
