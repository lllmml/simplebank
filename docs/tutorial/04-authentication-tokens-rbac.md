# 第 4 阶段：认证、Token、Session 与 RBAC

> 面向后端与 Go 初学者。本章先从攻击者视角建立认证系统的完整知识模型，再逐行回到 simplebank 的代码和 Git 演进。读完后，你应该能解释“密码为什么要慢哈希”“Access Token 和 Refresh Token 为什么不能混用”“登录与权限不足为何是两类错误”，也能判断一个教学实现距离生产系统还缺什么。

## 0. 本章范围与事实基线

本章核对的是当前 `ft/RABC` 分支，以及这些关键提交：

| 提交 | 本阶段引入或改变的内容 |
|---|---|
| `4dd9f82` | bcrypt 密码哈希、创建用户 HTTP API |
| `b9210e6` | 第一版 JWT、PASETO、统一 `Maker`、角色常量、带 `token_type` 的 Payload |
| `b5758b2` | 为跟随课程版本改用旧 JWT 库，并删除 `token_type` 等设计 |
| `414d981` | Gin 登录接口，实际选择 PASETO Maker |
| `47561e0` | Gin Bearer 认证中间件和账户所有权检查 |
| `9ea22fb` | Refresh Token、`sessions` 表、续期接口 |
| `7e29361` | gRPC `UpdateUser` 的身份认证与“只能改自己”规则 |
| `1f4bdb0` | 需要认证的 gRPC API 单元测试 |
| `ce0b978` | 用户表增加角色，并把已有 `depositor`/`banker` 常量接入 Token 与 RBAC |

先明确三件容易读错的事实：

1. **当前运行时使用 PASETO，不使用 JWT。** `api.NewServer` 和 `gapi.NewServer` 都调用 `token.NewPasetoMaker`。JWT 代码与测试仍在仓库中，用于展示另一种实现，但没有被当前 Server 构造函数选中。
2. **当前 `main.go` 没有启动 Gin。** 它启动 gRPC、gRPC-Gateway 和异步 Worker；`runGinServer` 虽然存在，却没有被调用。因此 `api/middleware.go`、`api/token.go` 等是旧 Gin 路径的教学代码，不是当前可执行程序实际暴露的路由。
3. **这不是生产级银行认证系统。** 它已经展示密码哈希、Bearer Token、Refresh Session 和基础 RBAC，但缺少 Token 类型隔离、Refresh Token 轮换、会话封锁操作、MFA、密钥轮换、登录限速、完整审计等能力。

后文会一直区分三层：技术原理、simplebank 当前实现、工业界常见做法。不要把“知道概念”“代码里有字段”和“系统真正形成安全闭环”混为一谈。

---

## 1. 先从威胁模型出发

安全设计不是把 bcrypt、JWT、RBAC 这些名词拼起来，而是回答：保护什么、谁可能攻击、攻击从哪里发生、失败后损失有多大。

simplebank 至少有这些资产：用户密码、身份、账户数据、转账权限、Token、Session、角色、密钥和审计记录。攻击者可能是外部匿名者、拿到普通账户的人、窃取浏览器 Token 的脚本、读到数据库备份的人，也可能是误配置或越权的内部账号。

### 1.1 威胁建模表

| 威胁 | 攻击前提 | 可能后果 | 主要防线 | simplebank 当前状态 |
|---|---|---|---|---|
| 数据库泄漏后的离线猜密 | 攻击者拿到 `users.hashed_password` | 恢复弱密码，再撞库其他网站 | 每用户 salt、慢哈希、合适成本、弱密码阻止 | 使用 bcrypt 默认 cost 10，自动 salt；密码策略仍弱 |
| 在线暴力破解/撞库 | 可反复调用登录接口 | 账户接管 | 限速、失败退避、风险检测、MFA、泄漏密码库 | 未实现限速、锁定、MFA |
| 用户名枚举 | 登录响应区分“用户不存在”和“密码错误” | 收集有效用户名，提升撞库效率 | 统一外部错误与时序，内部保留原因 | Gin 返回 404/401；gRPC 消息也不同，可枚举 |
| Token 在传输中泄漏 | 没有 TLS、代理或日志泄漏 Header | Bearer Token 被直接重放 | TLS、日志脱敏、短有效期 | 代码不负责 TLS，部署必须保证；日志脱敏未形成规则 |
| Token 被 XSS 读取 | Token 放在 JS 可读存储且页面存在 XSS | 会话劫持 | CSP、输出编码、依赖治理、HttpOnly Cookie/BFF | 仓库未定义浏览器存储方案 |
| Cookie 被 CSRF 利用 | 浏览器自动带 Cookie，攻击者诱导跨站请求 | 以受害者身份执行操作 | SameSite、CSRF Token、Origin 检查 | 当前 API 示例使用 Header；没有 Cookie/CSRF 方案 |
| Refresh Token 重放 | 长期 Token 被复制 | 持续换取新 Access Token | Rotation、复用检测、Token family、撤销 | 当前 Refresh Token 重复使用到过期，无轮换 |
| Access/Refresh 混淆 | 两者格式和验证规则相同 | 长期 Refresh Token 被当作访问凭据 | `token_type`、不同 `aud`/密钥、验证入口分离 | 当前 Payload 无类型；受保护接口会接受有效 Refresh Token |
| 权限提升 | 可伪造角色或利用粗粒度规则 | 普通用户执行 banker 操作 | Token 完整性、服务端授权、最小权限、审计 | PASETO 防篡改；角色模型非常粗，只有两种字符串 |
| 角色变更后旧权限仍有效 | 角色已写入长寿命 Token | 被降权者继续使用旧角色 | 短 Access Token、会话撤销、权限版本/在线查询 | 角色会陈旧，Refresh 续期也不重查数据库 |
| 签名/加密密钥泄漏 | 配置、仓库、日志或主机泄漏密钥 | 攻击者可伪造全部 Token | Secret Manager、轮换、`kid`、最小密钥接触面 | `app.env` 已提交敏感配置，必须视为泄漏并轮换 |
| 密码修改后旧会话继续有效 | 攻击者已有 Token | 用户改密也赶不走攻击者 | 撤销所有 Session、比较 `issued_at` 与密码变更时间 | `password_changed_at` 会更新，但认证时不检查它 |

这张表体现一个核心方法：每个安全机制只解决特定威胁。bcrypt 保护数据库泄漏后的离线猜测，不会阻止在线撞库；加密的 PASETO 不会阻止已窃取 Token 的重放；RBAC 也不会替代资源所有权检查。

---

## 2. 密码存储：目标不是“无法解密”，而是“昂贵地猜”

### 2.1 为什么不能存明文，也通常不应可逆加密

如果数据库存明文，数据库备份、SQL 注入、内部误操作、日志或调试导出中的任一泄漏都会立刻暴露全部密码。可逆加密也不是理想答案：应用为了登录校验必须能解密，于是解密密钥一旦和应用一起失守，所有密码仍可批量恢复。

密码校验真正需要的能力只有：判断“用户这次输入”和“注册时输入”是否相同，并不需要找回原密码。因此应该使用**单向密码哈希/KDF**：

```text
注册：password + salt + cost  --password hash-->  encoded hash
登录：candidate + encoded hash --verify------->  相同 / 不同
```

这里不能用普通 SHA-256。SHA-256 的目标是快，而攻击者恰好希望每秒尝试尽可能多的候选密码。密码哈希需要有意地慢，现代方案还会消耗较多内存，让 GPU/ASIC 批量猜测变贵。

### 2.2 Salt、Pepper、Work Factor 分别是什么

**Salt（盐）**是每个密码独立生成的随机非秘密值。即使两个用户使用同一个密码，不同 salt 也会产生不同哈希。它阻止预计算彩虹表，也迫使攻击者逐个哈希猜测。Salt 不需要藏起来，通常直接编码在哈希字符串中。

**Pepper（胡椒）**是所有或一组密码额外使用的服务端秘密。它不能和哈希一起放在同一数据库，应存于 Secret Manager 或 HSM。Pepper 是纵深防御，不替代 salt，也会带来轮换困难；本项目没有 pepper。

**Work factor（成本参数）**控制一次哈希多昂贵。bcrypt 的 cost 每增加 1，工作量大致翻倍。成本过低抵挡不了硬件进步；过高则拖慢正常登录，甚至让攻击者用大量登录请求耗尽 CPU。因此工业界会在生产同型号机器上基准测试，选择用户可接受、服务容量可承受的成本，并随硬件发展升级。

### 2.3 bcrypt 的格式与边界

bcrypt 输出不是“纯哈希值”，而是自描述字符串，通常包含版本、cost、salt 和派生结果。因此校验时不需要单独查询 salt/cost。Go 的 `bcrypt.GenerateFromPassword` 自动生成随机 salt。

需要记住 bcrypt 的输入上限：常见实现只处理最多 72 字节。当前 simplebank 的 gRPC 校验允许密码长度 6～100（`len` 按字节计），Gin 也只设了最小长度；超过 bcrypt 可接受字节数的输入可能通过 API 参数校验，却在哈希阶段报错。这是业务校验与算法边界没有对齐。

### 2.4 回到 `util/password.go`

当前代码的核心只有两步：

```go
func HashPassword(password string) (string, error) {
    hashedPassword, err := bcrypt.GenerateFromPassword(
        []byte(password),
        bcrypt.DefaultCost,
    )
    // ...
}

func CheckPassword(hashedPassword, password string) error {
    return bcrypt.CompareHashAndPassword(
        []byte(hashedPassword),
        []byte(password),
    )
}
```

`bcrypt.DefaultCost` 在所用 Go 实现中是 10。`HashPassword` 返回包含 salt 与 cost 的编码字符串；`CheckPassword` 读取这些参数，重新派生并做比较。调用者只看 `error == nil`，不自己比较字符串。

`util/passsword_test.go` 核对了三个关键性质：正确密码可通过、错误密码返回不匹配错误、同一密码连续哈希两次结果不同。最后一点正是随机 salt 的可观察结果。

`4dd9f82` 在创建用户时先哈希，再将 `HashedPassword` 写入数据库；响应结构不包含哈希。后续测试中的自定义 gomock Matcher 也不是要求哈希字符串固定相等，而是用 `CheckPassword` 验证 Mock 收到的哈希确实来自测试密码。这是测试带随机 salt 的正确思路。

### 2.5 工业界密码策略

新系统通常优先评估 Argon2id；OWASP 当前把 bcrypt 更多定位为 Argon2id/scrypt 不可用时的遗留选择。不能机械地认为“bcrypt 就不安全”：已有 bcrypt 系统可在合适 cost 下继续运行，但新设计应结合合规、库成熟度和容量测试选择算法。

生产实践还包括：

- 允许长密码/密码短语，不依赖“必须大小写、数字、符号齐全”这种容易诱导固定模式的规则；对照常见与已泄漏密码阻止弱口令。
- 登录、重置密码和 MFA 验证都做基于账号、IP、设备与风险信号的限速；避免简单永久锁号被滥用于拒绝服务。
- 只有在用户请求或有泄漏证据时强制改密，而不是无理由定期轮换。
- 高风险操作使用 MFA 或 WebAuthn/passkey，并做最近登录确认（step-up authentication）。密码与短信验证码并不一定能提供足够的抗钓鱼能力。
- 保存算法和参数，让用户下次成功登录时按新参数重新哈希；迁移期可兼容旧哈希格式。
- 登录外部响应统一为“用户名或密码错误”，内部日志保留真实原因；同时尽量控制可观测时序差异。

当前项目的最小密码长度是 6，未检查泄漏密码、未限速、未实现 MFA，不能直接照搬到真实银行系统。

---

## 3. Authentication 与 Authorization：先证明是谁，再判断能做什么

这两个词经常被混用，但它们回答不同问题：

- **Authentication（身份认证）**：你是谁？例如密码校验和 Token 完整性验证。
- **Authorization（权限授权）**：这个身份是否允许做当前动作？例如 depositor 是否只能修改自己，banker 是否能修改其他用户。

一个有效 Token 只能证明“服务端曾给这个身份签发过凭据，且凭据当前通过校验”，不能自动证明它有权操作任意账户。授权还要结合：动作、目标资源、资源所有者、角色、租户、金额、时间、风险等级等上下文。

状态码也应体现这个区别：

- HTTP `401 Unauthorized` 或 gRPC `Unauthenticated`：没有有效身份凭据。虽然 HTTP 名称叫 Unauthorized，语义上是“未认证”。
- HTTP `403 Forbidden` 或 gRPC `PermissionDenied`：身份已经有效，但权限不足。

simplebank 的 Gin 转账路径在 Bearer 验证后，还检查 `fromAccount.Owner == authPayload.Username`，这是资源级授权。gRPC `UpdateUser` 则先认证、检查允许角色，再做“banker 可改别人，depositor 只能改自己”的规则。

---

## 4. Bearer Token：拿到它的人就是持有人

Bearer 的字面意思是“持有者”。请求一般这样携带：

```http
Authorization: Bearer <access-token>
```

它不是“每次拿密码登录”，也不是 proof-of-possession：服务端只验证 Token 本身，调用者不需要额外证明自己掌握绑定私钥。因此攻击者只要复制 Token，就能在有效期内重放。

这带来几个直接要求：全程 TLS；不要放 URL 查询参数，因为 URL 容易进入日志、历史和 Referer；日志、错误追踪与 APM 对 Authorization Header 脱敏；Access Token 尽量短寿命；不要把 Token 输出到异常信息。

### 4.1 Gin 中间件如何工作

`api/middleware.go` 的流程是：

```text
读取 authorization Header
  -> strings.Fields 拆分
  -> 检查第一个字段是否为 bearer（忽略大小写）
  -> VerifyToken
  -> 将 Payload 放进 gin.Context
  -> ctx.Next() 进入后续 Handler
```

缺 Header、格式错误、不支持的类型、Token 无效或过期，都返回 HTTP 401。`api/middleware_test.go` 用表驱动测试覆盖了这些分支。

当前格式检查只判断 `len(fields) < 2`，不是严格要求恰好两个字段；`Bearer token extra` 会忽略多余字段。更严谨的实现应要求 `len(fields) == 2`，限制 Header 大小，并返回符合协议的错误结构。

### 4.2 gRPC Metadata 如何工作

gRPC 没有 Gin Context，但可通过 incoming metadata 携带相同的 `authorization` 值。`gapi/authorization.go` 依次检查 metadata、Header、格式、Bearer 类型、Token 和角色，然后返回 Payload。

注意，这段认证是 `UpdateUser` Handler 主动调用的，不是覆盖所有 RPC 的全局认证拦截器。新增敏感 RPC 时，如果开发者忘记调用 `authorizeUser`，就可能遗漏保护。工业界常把“哪些方法公开、哪些方法需何种权限”放进统一 interceptor/policy 层，再在 Handler 内做资源级判断。

---

## 5. JWT：签名不等于加密，灵活也意味着必须严格验证

### 5.1 JWT 的三段结构

常见的签名 JWT（JWS）长这样：

```text
base64url(header).base64url(payload).base64url(signature)
```

- Header 描述类型和算法，例如 `alg: HS256`。
- Payload 是 Claims，例如主体、签发时间、过期时间、受众。
- Signature 让接收方发现 Header/Payload 被篡改。

前两段只是 Base64URL 编码，任何拿到 Token 的人都可以解码阅读；签名 JWT 默认**不保密**。不要把密码、银行卡号等敏感数据放进 Payload。JWT 也有加密形式 JWE，但不能把 JWS 和 JWE 混为一谈。

### 5.2 算法校验为什么危险

JWT 的算法写在攻击者可修改的 Header 中。安全实现不能“来什么 alg 就按什么验”，而要由服务端配置固定允许算法，并让密钥类型与算法严格匹配。历史上出现过 `alg=none`、HMAC/非对称算法混淆、错误共享密钥等问题。

simplebank 的 `JWTMaker.CreateToken` 固定使用 HS256；`VerifyToken` 的 `keyFunc` 拒绝非 HMAC 算法，因此 `TestInvalidJWTTokenAlgNone` 验证了 `none` 会被拒绝。这一点是正确的。

但当前校验只断言 `token.Method` 属于 `SigningMethodHMAC` 家族，没有精确断言就是 HS256；HS384/HS512 也属于该家族。工业实现应显式 pin 到唯一允许算法，使用维护中的库，并校验完整 Claims，而不是仅依赖“签名通过”。仓库还在使用已经归档的 `github.com/dgrijalva/jwt-go`，但当前运行时并不走 JWTMaker。

### 5.3 Claims 不只是 `exp`

当前 `token.Payload` 包含：

| 字段 | 含义 | 类似标准 Claim |
|---|---|---|
| `ID` | 每个 Token 的 UUID | `jti` |
| `Username` | 当前用户 | `sub` 的项目化表达 |
| `Role` | Token 签发时的角色快照 | 自定义 claim |
| `IssuedAt` | 签发时间 | `iat` |
| `ExpiredAt` | 过期时间 | `exp` |

`Payload.Valid` 只检查过期时间。生产系统通常还验证：

- `iss`：是否由预期签发方签发；
- `aud`：是否发给当前 API，防止 A 服务的 Token 被 B 服务接受；
- `token_type`/purpose：只能把 Access Token 用于资源 API，把 Refresh Token 用于续期；
- `nbf`/`iat`：时间窗口是否合理，并设置有限时钟偏差；
- `jti`：在需要时用于撤销、追踪和重放检测；
- 关键流程所需的认证强度，例如是否完成 MFA。

RFC 8725 还强调不同种类 JWT 应使用互斥的验证规则，防止一种 Token 被另一种入口接受。

---

## 6. PASETO v2.local：项目当前实际使用的 Token

PASETO 试图通过固定版本与 purpose 对算法选择做约束，减少 JWT 过度灵活带来的误用。仓库使用 `github.com/o1egl/paseto` 的 V2：

```go
paseto.NewV2()
maker.paseto.Encrypt(maker.symmetricKey, payload, nil)
maker.paseto.Decrypt(token, maker.symmetricKey, payload, nil)
```

这生成的是 **v2.local** Token。v2.local 使用对称密钥和认证加密（XChaCha20-Poly1305）：没有密钥的人既不能读取 Payload，也不能在不被发现的情况下修改它。构造器严格要求 32 字节对称密钥。

要区分三个性质：

- **机密性**：外部观察者不能读取 Payload；v2.local 提供。
- **完整性/真实性**：修改会被发现，持有正确密钥的一方生成的 Token 才能通过；v2.local 提供。
- **防盗用/防重放**：偷到完整 Token 后不能使用；普通 Bearer v2.local **不提供**。

所以 PASETO 加密不意味着可以把 Token 放进日志，也不意味着长有效期没关系。所有可解密 v2.local 的服务都持有同一对称密钥，原则上也都能签发 Token；微服务边界扩大时，需要评估独立密钥、非对称 public purpose、集中签发或 Token introspection。

### 6.1 `Maker` 接口的 Go 设计价值

`token.Maker` 把业务依赖收窄成两个方法：

```go
type Maker interface {
    CreateToken(username, role string, duration time.Duration) (string, *Payload, error)
    VerifyToken(token string) (*Payload, error)
}
```

JWTMaker 与 PasetoMaker 都实现它。Go 是隐式实现接口：不需要写 `implements`。Server 只依赖接口，测试辅助函数也能使用同一协议。这展示了“依赖抽象”的价值。

但接口本身也固化了一个当前缺陷：`CreateToken`/`VerifyToken` 没有 Token 类型、issuer 或 audience 参数。抽象不只是为了可替换，也必须表达不能被忽略的安全语义。

---

## 7. Access Token、Refresh Token 与服务端 Session

### 7.1 为什么拆成两个 Token

若只有一个长寿命 Token，被盗后攻击窗口很长；若只有一个 15 分钟 Token，用户每 15 分钟就要重新输密码。常见折中是：

- **Access Token**：短寿命，频繁发送给资源 API；泄漏窗口较短。
- **Refresh Token**：长寿命，只发给授权/续期端点；用于获得新 Access Token，必须保护得更严。

当前 `app.env` 配置 Access Token 为 15 分钟、Refresh Token 为 24 小时。时长没有普适答案，应按风险、客户端类型和撤销能力决定。

### 7.2 为什么有 Token 还需要 Session 表

完全自包含 Token 的优点是资源服务只验密码学和时间，不必每次查数据库；缺点是签发后难以立即撤销。simplebank 为 Refresh Token 建立 `sessions` 表：

| 字段 | 用途 |
|---|---|
| `id` | 使用 Refresh Payload 的 UUID，连接 Token 与 Session |
| `username` | 会话属于谁 |
| `refresh_token` | 当前 Refresh Token 原文 |
| `user_agent`、`client_ip` | 登录环境信息，可用于展示、审计或风险判断 |
| `is_blocked` | 预留的封锁标志 |
| `expires_at` | 服务端会话过期时间 |
| `created_at` | 建立时间 |

登录时创建 Access、Refresh，再以 Refresh Token ID 创建 Session。续期时，`api/token.go` 先验证 Token，再按 ID 取 Session，并检查：未封锁、用户名一致、数据库 Token 与请求 Token 一致、Session 未过期。通过后签发新 Access Token。

这比“只校验 Refresh Token”多了一层服务端控制。但“表里有 `is_blocked`”不等于已经支持撤销：当前 `db/query/session.sql` 只有 `CreateSession` 和 `GetSession`，没有把 Session 标记 blocked、删除 Session、注销单设备或注销全部设备的操作。续期代码会读取 `is_blocked`，却没有业务路径能改变它。

### 7.3 当前续期路径实际上没有运行

`api/token.go` 属于 Gin Router，`main.go` 未调用 `runGinServer`；当前 Proto/gRPC 服务也没有 renew-access RPC。因此项目有续期实现与单元代码，但当前可执行程序并未暴露这个入口。读仓库时必须区分“函数存在”和“运行时可达”。

该 Handler 还声明了专用的 `renewAccessTokenResponse`，实际却构造并返回 `loginUserResponse`，只填 Access Token 两个字段。若 Gin 路径被启动，JSON 中还会出现 Session、Refresh Token、User 等字段的零值；这不会直接签发额外凭据，但会让 API 契约混乱。Session Token 不一致时返回的错误文字也是 `blocked session`，没有准确表达原因。生产代码应使用专用响应类型和稳定、可观测但不泄密的错误码。

### 7.4 当前最关键的 Token 类型混淆

`b9210e6` 的第一版 Payload 原本有 `TokenTypeAccessToken`/`TokenTypeRefreshToken`，创建与验证都要求传入类型。`b5758b2` 随课程版本删除了这个字段；后来的 Refresh Token 只是“有效期更长、ID 对应 Session”的同形 PASETO。

后果是：

- 续期入口收到 Access Token 时，通常因为找不到对应 Session 而失败，但这是数据库状态造成的偶然失败，不是类型校验。
- 更严重的是，Gin/gRPC 的受保护资源入口只调用 `VerifyToken`，不查询 Session、不检查类型。因此一个有效 Refresh Token 可以放进 `Authorization: Bearer ...`，被当成 Access Token 接受。

正确修复不是依赖“前端不会这么做”，而是让服务端强制区分 purpose。可以组合使用：独立 `token_type`、不同 `aud`、不同密钥、不同解析器，并让受保护 API 明确只接受 Access Token，续期 API 明确只接受 Refresh Token。

### 7.5 Refresh Token Rotation 与复用检测

当前 renew 只签发新 Access Token，旧 Refresh Token 可一直重复使用到 24 小时过期。工业界对公共客户端常采用 Refresh Token rotation：

```text
登录 -> R1
R1 换取 -> A2 + R2，并立即使 R1 失效
R2 换取 -> A3 + R3，并立即使 R2 失效
若 R1 再次出现 -> 判定泄漏，撤销整个 token family
```

服务端会存当前 Token 的哈希、family ID、父子关系、使用时间和撤销原因，并用事务或原子更新保证并发请求只有一个成功。存哈希而不是 Refresh Token 明文，可降低 Session 表泄漏后的直接重放风险。

OAuth 2.0 Security BCP（RFC 9700）要求公共客户端的 Refresh Token 使用 sender-constrained 机制或 rotation 来检测重放。simplebank 不是完整 OAuth 授权服务器，但这个威胁和对策同样值得借鉴。

### 7.6 密码修改、角色变更与撤销

更新密码时，`gapi/rpc_update_user.go` 会重新 bcrypt 哈希，并更新 `password_changed_at`。但 Token 校验从不读取这个字段，也没有撤销 Session，所以已经签发的 Token 在到期前仍有效。

角色也嵌入 Payload，是签发时快照。若数据库中把 banker 降为 depositor：

1. 旧 Access Token 仍携带 banker，直到自身过期；
2. 旧 Refresh Token 也携带 banker；
3. 当前续期用 `refreshPayload.Role` 生成新 Access Token，不重新查询用户；
4. 又因为没有 Session 封锁操作，旧权限理论上可延续到 Refresh Token 过期。

常见解决思路包括：Access Token 短寿命；角色高风险变化时撤销会话；在 Token 中放 `authz_version` 并与服务端版本比较；关键操作在线查询权限；续期时重读用户状态；或者使用集中授权服务。选择取决于性能、权限变更时效和系统规模。

---

## 8. 浏览器中的 Header、Cookie、XSS 与 CSRF

“Token 放 Header 就安全”“Cookie 一定更安全”都过于简单。真正要看浏览器是否自动发送、JavaScript 是否能读取，以及应用架构。

### 8.1 Authorization Header 路线

SPA 常把 Access Token 放在内存，再由 JavaScript设置 Authorization Header。它不会像 Cookie 那样被浏览器对目标站点自动附带，因此传统 CSRF 风险较低。但只要 XSS 能在页面执行，恶意脚本就可能读到内存、`localStorage` 或 `sessionStorage` 中的 Token，并传到攻击者服务器。`localStorage` 还会长期保留。

### 8.2 HttpOnly Cookie 路线

`HttpOnly` Cookie 不能被 JavaScript 直接读取，`Secure` 限制到 HTTPS，`SameSite` 可限制部分跨站发送。但浏览器会自动带 Cookie，因此必须考虑 CSRF：使用合适 SameSite、CSRF Token、Origin/Referer 验证，并禁止用 GET 改状态。

HttpOnly 也不是“免疫 XSS”：脚本虽读不到 Cookie，仍可能从受害者页面发同源操作请求，或读取页面中的敏感响应。根本防线仍包括输出编码、框架安全 API、严格 CSP、依赖治理和避免危险 DOM 操作。

### 8.3 常见折中

一种常见 BFF（Backend for Frontend）方案是浏览器只持有 HttpOnly Session Cookie，BFF 在服务端保管下游 Token。也有方案把短寿命 Access Token 放内存、Refresh Token 放受限 HttpOnly Cookie，并为 Refresh 端点做 CSRF 防护。移动端则通常使用操作系统安全存储。

没有脱离威胁模型的唯一答案。simplebank 只规定了 `Authorization: Bearer` 的服务端解析，没有定义 Token 在浏览器如何存储，也没有 Cookie 属性、CORS 与 CSRF 防护；前端接入时不能自行脑补这些能力已存在。

---

## 9. RBAC、最小权限与资源级授权

### 9.1 RBAC 原理

RBAC（Role-Based Access Control）把权限赋给角色，再把角色赋给用户：

```text
用户 -> 角色 -> 权限 -> 资源上的动作
```

例如 `depositor` 可能拥有“查看自己的账户、从自己的账户转账”，`banker` 可能拥有“审核客户资料”。但角色不应只是职位名称；应先列出具体权限，再组合成角色。

**最小权限原则**要求每个身份只获得完成任务必需的权限，并尽量缩短授权时间。它还要求职责分离：能发起高风险动作的人不一定能单独审批。

### 9.2 simplebank 当前 RBAC

角色常量最早由 `b9210e6` 写入 `util/role.go`；`b5758b2` 删除早期 Payload 的 Role/TokenType 后，常量文件仍保留。`ce0b978` 才把角色真正接到数据库用户、Token 和授权路径中，具体变化是：

- `users` 增加 `role varchar NOT NULL DEFAULT 'depositor'`；
- 使用 `util/role.go` 已有的 `DepositorRole`、`BankerRole` 常量；
- Token Payload 增加 Role；
- 登录时从数据库用户读取 Role 写入 Access/Refresh Token；
- `authorizeUser` 接受允许角色列表；
- `UpdateUser` 允许两种角色进入，但非 banker 只能修改自己的用户名对应记录。

当前规则可写成：

| 动作 | depositor | banker |
|---|---|---|
| 调用 `UpdateUser` | 可以 | 可以 |
| 修改自己 | 可以 | 可以 |
| 修改别人 | 不可以 | 可以 |

但它仍是很粗的示例：

- 数据库 role 列没有 CHECK/枚举/外键，不能从数据库层阻止任意字符串；
- 没有角色管理 API，banker 如何被可信地授予并未形成流程；
- 只有 `UpdateUser` 使用这套 gRPC RBAC，没有全局权限矩阵；
- banker 修改别人时，也能走可选 password 字段重置其密码，这可能比“修改客户资料”权限更大；
- 没有 maker-checker、金额阈值、租户边界、临时授权、审批与细粒度审计；
- 角色作为 Token 快照会陈旧。

工业系统常把 `user:update_profile`、`user:reset_password`、`role:assign` 等权限拆开，而不是全部压进 banker。小系统可用代码内权限矩阵；系统变大后可使用集中策略服务或成熟授权引擎，但必须保证默认拒绝、策略版本化、可测试和可审计。RBAC 解决“角色拥有什么权限”，资源级检查仍要解决“这条记录是否属于你”“是否在同一租户”。

### 9.3 当前错误码边界

`UpdateUser` 的外层代码是：

```go
authPayload, err := server.authorizeUser(ctx, allowedRoles)
if err != nil {
    return nil, unauthenticatedError(err)
}
```

而 `authorizeUser` 同时负责 Token 认证和角色判断；角色不允许时返回普通 `permission denied` error。外层随后把所有 error 统一包装成 gRPC `Unauthenticated`。所以**如果角色不在允许列表，真实返回码也是 `Unauthenticated`，而不是 `PermissionDenied`**。

只有通过允许角色检查后，depositor 尝试修改别人时，Handler 才明确返回 `codes.PermissionDenied`。`1f4bdb0` 的测试覆盖了后者、过期 Token 和无 Authorization，但没有覆盖“不在 accessibleRoles 中的未知角色”。

更清晰的设计是让认证与授权返回可区分的类型/status：无效或缺失凭据 -> `Unauthenticated`；身份有效但角色/资源权限不足 -> `PermissionDenied`。这不仅符合语义，也利于告警和审计。

---

## 10. 把项目中的完整流程串起来

### 10.1 注册/创建用户

旧 Gin 路径：

```text
POST /users
  -> 参数绑定（密码最少 6）
  -> bcrypt.GenerateFromPassword(DefaultCost)
  -> 只把 hashed_password 写入 users
  -> 返回不含密码哈希的 userResponse
```

当前 gRPC `CreateUser` 走 `CreateUserTx` 并带异步邮件任务，但密码核心仍由 `util.HashPassword` 完成。创建参数不允许客户端自选 banker，数据库默认 depositor，这一点符合默认最小权限思路；不过 banker 的授予流程缺失。

### 10.2 登录

当前 gRPC `LoginUser`：

```text
校验 username/password 格式
  -> GetUser(username)
  -> bcrypt.CompareHashAndPassword
  -> 创建短期 Access PASETO（含 username/role/ID/时间）
  -> 创建长期 Refresh PASETO（结构相同，只是 duration 不同）
  -> 提取 User-Agent 和 Client IP
  -> CreateSession(refresh token ID, token 原文, 环境, 过期时间)
  -> 返回用户、Session ID、两个 Token 与各自过期时间
```

这里对不存在用户返回 `codes.NotFound: user not found`，密码不正确返回 `codes.NotFound: password error`。虽然 code 相同，message 不同；Gin 更是 404 与 401 不同。攻击者可以据此枚举有效用户名。生产上应给外部统一提示，内部记录结构化原因，并加限速与审计。

Session 创建发生在两个 Token 生成之后；若数据库写 Session 失败，本次登录返回 Internal，已经生成的字符串没有交给调用者，通常不会成为有效会话，但系统仍应记录失败指标。响应与日志必须避免打印 Token。

### 10.3 调用需要认证的 `UpdateUser`

```text
客户端把 PASETO 放进 gRPC metadata authorization
  -> authorizeUser 解析 Bearer
  -> PASETO Decrypt + Payload.Valid(exp)
  -> 检查 Role 是否为 banker/depositor
  -> 参数校验
  -> banker 或 username 与目标一致？
  -> 更新可选字段
  -> 若改密码，重新 bcrypt 并更新 password_changed_at
```

顺序上先认证再校验请求参数，可避免匿名调用者通过错误细节探测更多业务信息。真正访问数据库前完成授权，测试用 `Times(0)` 验证不应执行的 Store 调用没有发生。

### 10.4 续期 Access Token（代码存在但运行时未暴露）

```text
POST /tokens/renew_access，Body 携带 refresh_token
  -> VerifyToken（没有 token_type 检查）
  -> 用 Payload.ID 查 sessions
  -> 检查 blocked、username、Token 原文、expires_at
  -> 用 Refresh Payload 中的 username/role 创建新 Access Token
  -> 不更换 Refresh Token，不更新 Session
```

这个流程展示了服务端 Session 的价值，也同时暴露了未轮换、角色陈旧、Token 类型混淆和未运行等边界。

---

## 11. 测试应该证明什么

### 11.1 当前已有的有效测试

- `util/passsword_test.go`：bcrypt 正确/错误密码与随机 salt。
- `token/jwt_maker_test.go`：有效、过期、`alg=none` 被拒绝，并校验 username/role/时间。
- `token/paseto_maker_test.go`：有效与过期 PASETO，并校验 Payload。
- `api/middleware_test.go`：Bearer 成功、缺 Header、错误类型、错误格式、过期。
- `api/user_test.go`：创建用户使用自定义密码 Matcher；登录覆盖用户不存在和密码错误。
- `gapi/rpc_update_user_test.go`：本人更新成功、其他 depositor 被拒绝、无 Token、过期 Token、无效参数及 Store 调用次数。

### 11.2 仍应增加的安全回归测试

1. 将 Refresh Token 放进受保护 API，期望被拒绝；当前实现会暴露失败测试。
2. 未知角色访问只返回 `PermissionDenied`，不是 `Unauthenticated`。
3. Header 必须恰好两个字段。
4. JWT 只允许 HS256，HS384/HS512 输入必须被拒绝。
5. 修改密码后旧 Session/Token 按产品策略失效。
6. banker 降权后不能用旧 Refresh Token 签发 banker Access Token。
7. 并发使用同一个旧 Refresh Token 时，rotation 只有一次成功，复用触发 family 撤销。
8. 登录对不存在用户和错误密码返回一致外部信息，并有速率限制测试。
9. Refresh Token 数据库存哈希，数据库泄漏后的原值不能直接重放。
10. Token 的 `iss`、`aud`、`type`、时间偏差和最大寿命均有边界测试。

安全测试不仅是“正确凭据能成功”，更重要的是证明错误凭据、错类型、错受众、旧角色和重放都失败，而且失败发生在任何数据变更之前。

---

## 12. 面向生产的改造路线

下面按优先级给出一条合理路线；它不是要求初学者一次完成所有内容。

### P0：立即处理的秘密与基本边界

- 当前仓库已提交的 `app.env` 含数据库凭据、Token 对称密钥和邮件发送凭据。不要复述、复制或继续使用这些值；应立即轮换，把真实秘密移出版本控制，并检查 Git 历史。仅在最新提交删除文件不能撤回历史泄漏。
- 所有入口强制 TLS，代理链正确配置；Authorization、Cookie、密码、邮件验证码在日志/APM 中脱敏。
- 登录统一外部错误，增加限速、监控和异常登录告警。
- 明确 Access/Refresh Token 类型和 audience；受保护 API 只接收 Access。

### P1：会话生命周期闭环

- 增加注销当前会话、注销全部设备、封锁/撤销 Session 的 SQL 与 API。
- 实现 Refresh Token rotation、family 与复用检测；数据库只存 Token 哈希。
- 密码重置、账号禁用、角色降权时撤销相关会话。
- 续期时重新加载用户有效状态和权限，或验证权限版本。
- 为 Access 与 Refresh 使用清晰的不同校验策略，必要时使用不同密钥。

### P2：密钥和 Token 治理

- 密钥放 Secret Manager/KMS/HSM，限制可读取身份；制定生成、启用、回滚、吊销和销毁流程。
- 使用 key ID/version 支持新旧密钥短暂并存；先让验证端接受新密钥，再切签发，最后淘汰旧密钥。
- 校验固定算法、issuer、audience、purpose、最大 TTL、`iat/nbf/exp` 与时钟偏差。
- 选择仍被维护的库，并跟踪安全公告。不要自行实现密码学原语。

### P3：认证强度与集中授权

- 为高风险用户和操作启用 MFA，优先评估抗钓鱼的 WebAuthn/passkey；设计恢复码和设备丢失流程。
- 将“修改资料”“重置密码”“授予角色”“转账审批”等权限拆开，遵守最小权限与职责分离。
- 多服务系统可集中策略决策，但资源服务仍需执行结果；策略默认拒绝、版本化、自动测试。
- 记录登录成功/失败、Token 刷新/复用、Session 撤销、密码和角色变更、授权拒绝等审计事件。日志包含 actor、action、target、result、reason、request/session ID 与可信时间，但绝不含密码和 Token 原文。
- 对敏感操作加入幂等、防重放 nonce、交易签名或 step-up authentication；Bearer Token 本身不具备请求级防重放能力。

---

## 13. 动手练习

### 练习 1：画出认证边界

不改代码，画出 gRPC Login、UpdateUser、Gin renew 三条调用链，并标注每一步属于：参数校验、身份认证、角色授权、资源授权、数据库状态检查。特别标记 Gin 路径当前不可达。

### 练习 2：增加 Token Purpose

给 Payload 增加 `TokenType`，让 Maker 的创建/验证必须显式选择 Access 或 Refresh。增加两条测试：Refresh 调受保护 API 失败；Access 调续期 API 失败。思考为什么只看有效期长短不等同于类型。

### 练习 3：严格算法与 Claims

若保留 JWT 教学实现，将校验固定到 HS256，增加 issuer、audience、type，并分别构造错算法、错 audience、错 type Token 验证失败。不要直接信任 Header 的算法。

### 练习 4：补全 Session 撤销

为 `sessions` 增加 `BlockSession`/`DeleteSession` 查询，设计 logout-current 和 logout-all。定义密码修改后是撤销所有会话，还是保留当前会话；将产品选择写进测试。

### 练习 5：实现 Rotation 的状态机

为 Session 增加 token hash、family ID、used/revoked 时间和原因。用事务实现 R1 -> R2，写并发测试证明同一 R1 只能成功一次；第二次使用撤销整个 family。

### 练习 6：修复错误语义

拆开 `authenticate` 与 `authorize`：缺失/无效 Token 返回 `Unauthenticated`；有效身份但角色或资源不允许返回 `PermissionDenied`。增加未知角色测试，并保持数据库 `Times(0)`。

### 练习 7：设计权限矩阵

把 `banker` 当前能修改他人密码的问题拆成 `user:update_profile` 与 `user:reset_password`。再为角色授予定义“谁能授予谁、是否需二人审批、如何审计”。

### 练习 8：做一次密码成本基准

使用 Go benchmark 测不同 bcrypt cost 的吞吐和 P95 延迟，结合实例 CPU 与峰值登录 QPS 估算容量。说明为什么不能只选“数字最大”的 cost；再调查 Argon2id 的内存、迭代和并行参数。

---

## 14. 自测题与答案

### 问题

1. Salt 是秘密吗？两个用户相同密码为何哈希不同？
2. 为什么 SHA-256 不能直接存密码？
3. bcrypt cost 越大是否永远越好？
4. JWT 签名是否会隐藏 Payload？
5. PASETO v2.local 加密后，Token 被偷还能否使用？
6. 401/Unauthenticated 与 403/PermissionDenied 的区别是什么？
7. 为什么 Access Token 短、Refresh Token 长？
8. simplebank 有 `is_blocked` 字段，为何仍不能说已经支持会话撤销？
9. 当前项目能否阻止 Refresh Token 被当作 Access Token？
10. 用户从 banker 被降为 depositor 后，旧权限为何不会立即消失？
11. HttpOnly Cookie 是否彻底解决 XSS？Authorization Header 是否彻底解决 CSRF？
12. RBAC 已判断 banker 后，为什么仍可能需要资源级授权？

### 答案

1. Salt 通常不是秘密，和哈希一起存储；每个密码使用独立随机 salt，所以相同密码输出不同，也不能用一次预计算同时匹配所有用户。
2. SHA-256 设计得太快，数据库泄漏后攻击者可以高速离线猜测。密码需要专用、可调成本，最好还抗 GPU 的 KDF。
3. 不是。成本过高会伤害正常登录容量并放大 CPU 拒绝服务；应在目标硬件基准测试后选择，并配合限速。
4. 不会。普通签名 JWT 的 Header/Payload 仅 Base64URL 编码，签名提供防篡改而非保密。
5. 能。它仍是 Bearer Token，攻击者可原样重放；加密只隐藏内容并防止篡改。
6. 前者表示没有有效身份，后者表示身份有效但动作不允许。当前 simplebank 的角色列表拒绝被错误统一包装为 Unauthenticated，是应修边界。
7. Access 经常发送，短寿命缩小泄漏窗口；Refresh 少量使用、严格保护，用于改善用户体验并配合服务端会话撤销。
8. 因为 SQL 只有 Create/Get，没有业务操作把它改为 true，也没有 logout/rotation API；字段只是数据模型的一部分。
9. 不能。当前 Payload 没有 token type，受保护接口只做通用 `VerifyToken`；有效 Refresh Token 可被当作 Bearer 访问凭据。
10. 角色是 Token 签发时快照；校验和续期不重查数据库，Session 又无法主动封锁，所以会陈旧到 Token 过期。
11. 都不能。HttpOnly 防脚本直接读取 Cookie，但 XSS 仍可发同源请求；Header 通常降低自动附带导致的 CSRF，却可能因 JS 可读存储而更怕 Token 被 XSS 窃取。
12. 角色只能说明一类主体的一般权限，还需判断目标是否属于该用户/租户、金额是否超限、状态是否允许等上下文。

---

## 15. 掌握清单

完成本章后，请确认自己能不看答案解释：

- [ ] 明文、可逆加密、普通哈希、密码 KDF 的区别。
- [ ] salt、pepper、work factor 分别解决什么，不能解决什么。
- [ ] bcrypt 的自动 salt、cost、自描述格式与 72 字节边界。
- [ ] Authentication、Authorization、角色授权、资源授权的层次。
- [ ] Bearer Token 为什么必须防泄漏、使用 TLS、避免进入 URL/日志。
- [ ] JWT 三段结构、签名不保密、算法 pinning 与 Claims 校验。
- [ ] PASETO v2.local 的机密性/完整性，以及它不防 Token 重放。
- [ ] Access/Refresh Token 的职责与类型隔离的重要性。
- [ ] 服务端 Session 如何提供状态，以及字段存在不等于撤销闭环存在。
- [ ] Refresh Token rotation、family 与复用检测的完整流程。
- [ ] Cookie/Header 在 XSS、CSRF 维度上的取舍。
- [ ] RBAC、最小权限、职责分离与资源级授权的关系。
- [ ] 角色写入 Token 后为何会陈旧，密码修改为何要处理旧会话。
- [ ] 当前运行时使用 PASETO；Gin API 没有启动；renew 入口当前不可达。
- [ ] 当前 gRPC 权限错误包装、登录枚举、秘密提交等真实风险边界。
- [ ] 工业系统中的 MFA、密钥轮换、审计、集中授权和防重放分别位于哪一层。

如果你能清楚回答下面这句话，本阶段就算真正入门了：

> Token 通过密码学验证，只能说明凭据真实且尚未过期；系统仍必须验证它的用途、签发方、受众、会话状态、当前权限和目标资源，而且要为凭据泄漏与权限变化设计撤销路径。

---

## 16. 核对资料

以下资料用于核对本章的协议与工业实践；项目事实则以仓库当前源码和上述 Git 提交为准：

- [OWASP Password Storage Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)：密码哈希、salt、pepper、cost、Argon2id/bcrypt 建议。
- [NIST SP 800-63B](https://pages.nist.gov/800-63-4/sp800-63b.html)：密码、认证器、限速和身份认证生命周期。
- [RFC 6750: Bearer Token Usage](https://datatracker.ietf.org/doc/html/rfc6750)：Bearer Header、TLS、401/403 和 Token 泄漏风险。
- [RFC 7519: JSON Web Token](https://datatracker.ietf.org/doc/html/rfc7519)：JWT 结构与标准 Claims。
- [RFC 8725: JWT Best Current Practices](https://datatracker.ietf.org/doc/html/rfc8725)：算法固定、验证规则和不同 JWT 类型互斥。
- [RFC 9700: OAuth 2.0 Security Best Current Practice](https://datatracker.ietf.org/doc/html/rfc9700)：Refresh Token rotation、重放检测等当前 BCP。
- [PASETO v2 specification](https://paseto.io/rfc/)：v2.local 的 XChaCha20-Poly1305 与 purpose。
- [OWASP Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)：Session 生命周期、Cookie 与日志保护。
- [OWASP CSRF Prevention Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html)：SameSite、CSRF Token 与 Origin 校验。
