# 第 3 阶段：从 HTTP 原理到 SimpleBank 的 Gin API

> 适合读者：第一次系统学习 HTTP、REST、Gin 和 API 测试的 Go/后端初学者。
>
> 本文核对基线：仓库当前 `ft/RABC` 分支、`api/` 目录、`main.go`、`proto/service_simple_bank.proto`，以及 Git 提交 `e2d19a3`、`ee825a2`、`ebf8c36`、`502b17c`、`414d981`、`47561e0`、`3a77ae5`。

## 0. 先说最重要的事实：当前程序没有启动 Gin API

仓库里确实有一套 `api/` 目录下的 Gin HTTP API，包含用户、登录、续期、账户和转账。但是，当前 `main.go` 的 `main()` 实际调用的是：

```text
runTaskProcessor(...)
runGatewayServer(...)
runGrpcServer(...)
```

`runGinServer(...)` 虽然还保留在 `main.go` 末尾，却没有被调用。因此：

- 当前正常运行程序时，`api/` 是一条遗留、未启用的代码路径；
- HTTP 端口上运行的是 gRPC-Gateway，不是 Gin；
- 当前 Proto 服务只定义了创建用户、更新用户、登录和验证邮箱；
- 当前 gRPC-Gateway **没有账户与转账 RPC**；
- 不能看到 `api/account.go` 就推断 `/accounts` 当前可以访问。

本文仍然详细讲 Gin，是因为这一阶段的 Git 历史完整展示了 HTTP API 从无到有、加入测试和认证的过程，非常适合学习。本文中的 `curl` 只能用于你**有意识地单独启动 Gin Server** 的实验环境，不能直接照抄后假定当前 `main()` 已暴露这些接口。

## 1. 学习地图：一个 HTTP 请求如何穿过后端

先建立全局图景：

```text
客户端
  │
  │ HTTP 请求：方法、URI、Header、Body
  ▼
Gin Router（路由匹配）
  │
  ▼
Middleware（日志、恢复、认证等）
  │
  ▼
Handler（绑定参数、校验、业务编排）
  │
  ▼
Store 接口（数据库能力的抽象）
  │
  ▼
PostgreSQL / 事务
  │
  ▼
Handler 将结果映射为状态码、Header、JSON Body
```

这条链上每一层都应该职责清晰：

- HTTP 层理解协议、输入和输出，不应该自己重写数据库事务；
- Middleware 处理多个路由共有的横切逻辑，不应该偷偷执行具体转账；
- Handler 负责编排，不应该把密码哈希返回给客户端；
- Store 隐藏持久化实现，让 Handler 可替换、可测试；
- 数据库必须守住最终一致性，不能只依赖 API 校验。

后端工程的难点通常不是“能返回一个 JSON”，而是让所有边界在失败、并发、恶意输入、重试和升级时仍然正确。

---

## 2. HTTP 到底是什么

HTTP 是应用层请求—响应协议。一次典型交互是：客户端发送一个请求，服务端返回一个响应。以 HTTP/1.1 的可读形式表示：

```http
POST /accounts HTTP/1.1
Host: api.example.com
Authorization: Bearer <access-token>
Content-Type: application/json
Accept: application/json

{"currency":"USD"}
```

响应可能是：

```http
HTTP/1.1 201 Created
Content-Type: application/json
Location: /accounts/42

{"id":42,"owner":"alice","balance":0,"currency":"USD"}
```

请求由以下部分组成：

1. 方法，例如 `GET`、`POST`；
2. 请求目标，也就是常说的路径和查询参数；
3. Header，携带内容类型、认证信息、追踪信息等元数据；
4. 可选 Body，携带 JSON 等实体内容。

响应由以下部分组成：

1. 状态码，例如 `200`、`404`；
2. Header，例如 `Content-Type`、`Location`；
3. 可选 Body，例如资源表示或统一错误体。

HTTP/1.1、HTTP/2、HTTP/3 的线上编码和传输方式不同，但方法、URI、Header、状态码等核心语义仍由 HTTP 语义标准定义。写业务 API 时，先掌握语义；连接复用、多路复用、QUIC 等传输细节可以后学。

### 2.1 无状态是什么意思

通常说 HTTP 是“无状态”的，意思不是服务端不能保存任何状态，而是每个请求需要携带足够的信息供服务端理解，协议不会自动替你保留“上一次业务步骤”。

银行账户、Session 都可以保存在数据库；Access Token 也可以放在 Header。无状态协议和有状态业务并不矛盾。

---

## 3. URI、Path、Query、Header、Body 分别放什么

看这个地址：

```text
https://api.example.com/v1/accounts/42?include=entries&page_size=20
```

- `https`：scheme，说明使用 HTTPS；
- `api.example.com`：authority/host；
- `/v1/accounts/42`：path；
- `include=entries&page_size=20`：query；
- `#fragment` 若存在，通常只由客户端处理，不会作为 HTTP 请求目标的一部分发送给服务端。

### 3.1 Path 参数

Path 通常标识“哪个资源”：

```text
GET /accounts/42
```

Gin 路由写作：

```go
router.GET("/accounts/:id", server.getAccount)
```

`api/account.go` 使用 `ShouldBindUri` 把 `:id` 绑定到：

```go
type getAccountRequest struct {
    ID int64 `uri:"id" binding:"required,min=1"`
}
```

### 3.2 Query 参数

Query 适合过滤、排序、分页和可选展示方式：

```text
GET /accounts?page_id=2&page_size=10
```

项目用 `ShouldBindQuery` 和 `form` tag：

```go
type listAccountsRequest struct {
    PageID   int32 `form:"page_id" binding:"required,min=1"`
    PageSize int32 `form:"page_size" binding:"required,min=5,max=10"`
}
```

不要把密码或 Token 放进 Query。URL 常被浏览器历史、代理、访问日志和监控记录，敏感值更容易泄露。邮箱验证链接中的短期一次性验证码是常见例外，但也必须短期有效、一次性使用，并避免第三方页面通过 Referer 等途径得到它。

### 3.3 Header

Header 是协议元数据。常见字段：

- `Content-Type`：请求 Body 实际是什么格式；
- `Accept`：客户端希望收到什么格式；
- `Authorization`：认证凭证；
- `User-Agent`：客户端信息；
- `Idempotency-Key`：某些 API 自定义或约定的幂等键；
- `X-Request-ID` 或标准 Trace Context：请求关联与链路追踪。

HTTP Header 字段名大小写不敏感。项目常量写成小写 `authorization`，`ctx.GetHeader` 仍可以读到常见的 `Authorization`。

### 3.4 Body

Body 适合结构化输入，例如创建账户或发起转账：

```json
{
  "from_account_id": 1,
  "to_account_id": 2,
  "amount": 100,
  "currency": "USD"
}
```

Body 不是无限大。工业服务会在反向代理或应用层限制请求体大小，否则攻击者可以用超大 JSON 消耗内存、CPU 和网络。

---

## 4. HTTP 方法：语义、安全性与幂等性

### 4.1 常用方法

| 方法 | 常见语义 | 是否安全 | 是否幂等 |
|---|---|---:|---:|
| `GET` | 读取资源 | 是 | 是 |
| `HEAD` | 只读取响应元数据，不返回 GET 的内容 | 是 | 是 |
| `POST` | 创建资源或执行命令 | 否 | 标准不保证 |
| `PUT` | 用完整表示创建/替换目标资源 | 否 | 是 |
| `PATCH` | 部分修改资源 | 否 | 不天然保证 |
| `DELETE` | 删除目标资源 | 否 | 是 |
| `OPTIONS` | 查询通信选项，浏览器 CORS 预检也会用 | 是 | 是 |

“安全”表示客户端不请求改变服务端状态；日志计数等附带效果不改变该方法的主要语义。“幂等”表示重复发送同一请求，服务端预期效果和发送一次相同。幂等不代表响应每次字节完全一致，也不代表请求不会写日志。

### 4.2 为什么转账不能随便重试

`POST /transfers` 默认不是幂等的。如果客户端已提交成功，但在收到响应前网络断开，它不知道转账是否成功。盲目重试可能再次扣款。

工业界常为支付、转账、下单等操作设计幂等能力：

1. 客户端为一次业务操作生成唯一幂等键；
2. 服务端把“调用方 + 幂等键”设置唯一约束；
3. 第一次执行时保存请求摘要和最终响应；
4. 相同键、相同参数重试时返回原结果；
5. 相同键、不同参数时返回冲突错误；
6. 幂等记录与业务结果尽量在同一数据库事务中提交。

注意：`Idempotency-Key` 在很多支付 API 和网关中很常见，但不要把它误写成“所有 HTTP 服务天然支持的万能标准 Header”。它必须成为你的 API 契约，并在服务端真正实现。

当前 SimpleBank 的 Gin 转账接口没有幂等键。`TransferTx` 保证单次数据库事务原子性，但“事务原子”与“重复 HTTP 请求只转一次”是两个问题。

---

## 5. JSON 与 Go Struct

JSON 只有对象、数组、字符串、数字、布尔值和 `null` 等基本类型。Go 用 struct 描述期望的请求形状：

```go
type transferRequest struct {
    FromAccountID int64  `json:"from_account_id" binding:"required,min=1"`
    ToAccountID   int64  `json:"to_account_id" binding:"required,min=1"`
    Amount        int64  `json:"amount" binding:"required,gt=0"`
    Currency      string `json:"currency" binding:"required,currency"`
}
```

这里有两套 tag：

- `json:"amount"` 告诉 JSON 编解码器字段名；
- `binding:"required,gt=0"` 告诉 Gin 的验证器该字段必填且大于零。

### 5.1 缺失、零值与 null

初学者最容易忽略“字段没传”和“字段传了零值”的区别。对于普通 `int64`：

- 没传字段，解码后是 `0`；
- 传 `0`，解码后也是 `0`。

创建转账时二者都非法，所以普通值加 `required,gt=0` 足够。部分更新时则必须区分“没传”和“明确清空”，工业界常使用指针、nullable wrapper 或 Proto `optional`。

### 5.2 不要直接返回数据库模型中的秘密

`api/user.go` 专门定义 `userResponse`，不包含 `HashedPassword`。即使密码是哈希，也不能暴露。这个 DTO（数据传输对象）把 API 契约和数据库模型隔离开。

项目的 `requireBodyMatchUser` 测试也明确断言响应中的 `HashedPassword` 为空，这不是多余测试，而是在锁定安全边界。

工业项目还会避免直接暴露内部自增 ID、数据库列名、审计字段和内部枚举，除非它们真的是对外契约的一部分。

---

## 6. 状态码：让客户端不用猜

状态码是协议的一部分，不是随便挑一个“看起来差不多”的数字。

| 状态码 | 适用场景 |
|---|---|
| `200 OK` | 成功读取、更新或执行，并返回结果 |
| `201 Created` | 成功创建新资源；可配合 `Location` |
| `204 No Content` | 成功但不返回响应 Body |
| `400 Bad Request` | JSON 语法、类型或参数格式错误 |
| `401 Unauthorized` | 未提供、无效或过期的认证凭证；其语义实际是“未认证” |
| `403 Forbidden` | 已认证，但没有执行该操作的权限 |
| `404 Not Found` | 目标资源不存在，或出于安全考虑不暴露其存在 |
| `409 Conflict` | 唯一约束冲突、幂等键冲突、状态冲突等 |
| `422 Unprocessable Content` | 请求格式可解析，但语义校验失败；是否与 400 区分要在团队内统一 |
| `429 Too Many Requests` | 触发限流 |
| `500 Internal Server Error` | 未预期的服务端错误 |
| `503 Service Unavailable` | 临时不可用，例如关键下游不可用或服务过载 |

### 6.1 当前代码的状态码问题

当前 `api/` 能工作，但部分映射不够符合常见工业语义：

- 创建用户和创建账户成功返回 `200`，更清晰的资源创建语义通常是 `201`；
- 用户名/邮箱重复、账户唯一约束或外键冲突返回 `403`，重复资源通常更适合 `409`；
- 已认证用户访问别人的账户返回 `401`，更准确通常是 `403`，或者为防止资源枚举统一返回 `404`；
- 登录时用户不存在返回 `404`、密码错误返回 `401`，这会让攻击者更容易枚举用户名；面向公网的认证接口常统一返回通用的 `401` 信息；
- Session 不存在时续期返回 `404`，对客户端通常仍可统一视为无效凭证并返回 `401`；
- 所有事务业务失败都映射为 `500`，余额不足、币种冲突、重复请求等应有稳定的领域错误和 4xx 映射。

这些是“可改进点”，不是说所有团队必须一字不差采用同一个状态码。关键是：语义清楚、文档一致、测试锁定，并且不泄露敏感状态。

---

## 7. REST：围绕资源设计，而不是把函数名塞进 URL

REST 不是“返回 JSON”的同义词。对业务 API 来说，最实用的设计习惯是：先识别资源，再用 HTTP 方法表达操作。

较自然的账户设计：

```text
POST   /v1/accounts        创建账户
GET    /v1/accounts/42     获取账户 42
GET    /v1/accounts        列出当前用户的账户
POST   /v1/transfers       创建一笔转账
```

不太资源化的设计：

```text
POST /createAccount
POST /getAccount
POST /doTransfer
```

但不要把 REST 当宗教。登录、续期这类动作有时用动作式 endpoint 更直观。重要的是契约稳定、语义可理解、认证与错误一致。

### 7.1 当前 Gin 路由

`api/server.go` 当前注册：

| 方法与路径 | 是否认证 | Handler |
|---|---:|---|
| `POST /users` | 否 | `createUser` |
| `POST /users/login` | 否 | `loginUser` |
| `POST /tokens/renew_access` | 否，但必须提交有效 Refresh Token | `renewAccessToken` |
| `POST /accounts` | 是 | `createAccount` |
| `GET /accounts/:id` | 是 | `getAccount` |
| `GET /accounts` | 是 | `listAccount` |
| `POST /transfers` | 是 | `createTransfer` |

这些只是 `api/` 中注册的 Gin 路由；再次强调，当前 `main()` 没启动它们。

### 7.2 API 版本化

工业界常见：

- Path 版本：`/v1/accounts`，最直观；
- Header 或媒体类型版本：URL 更干净，但调用和排障更复杂；
- 不为每次小改动都升大版本；尽量做向后兼容增加；
- 删除字段、改变含义、改变必填条件等破坏性变更才需要明确迁移计划；
- 给旧版本设置弃用公告、监控使用量和终止日期。

当前 Gin 路由没有 `/v1` 前缀。当前 gRPC-Gateway 的用户接口有 `/v1/...`，但它是另一条运行路径，不应混为一谈。

---

## 8. Gin 中的 Router、Handler 与 Middleware

### 8.1 Router

Router 根据 HTTP 方法和路径找到 Handler。相同路径的 `GET` 与 `POST` 可以指向不同 Handler。

当前代码：

```go
router := gin.Default()
router.POST("/users", server.createUser)
router.GET("/accounts/:id", server.getAccount)
```

`gin.Default()` 默认带 Logger 和 Recovery Middleware：前者记录基本访问日志，后者在 Handler panic 时恢复并返回 500，避免整个进程因为单个请求直接退出。Recovery 不是错误处理的替代品；可预期错误仍应显式返回。

### 8.2 Handler

Handler 接收 `*gin.Context`，完成一次请求的 HTTP 层编排：

```go
func (server *Server) getAccount(ctx *gin.Context) {
    // 1. 绑定并验证输入
    // 2. 读取认证身份
    // 3. 调用 Store
    // 4. 检查资源所有权
    // 5. 映射错误或返回 JSON
}
```

Handler 应尽量薄。复杂业务规则放进 service/use-case 层或领域层，数据库原子性放进事务层。这个教学项目规模较小，Handler 直接调用 Store 是合理的简化；系统扩大后再增加业务层，而不是一开始堆很多空壳接口。

### 8.3 Middleware

Middleware 包裹 Handler，适合所有或一组路由共有的逻辑：

- 请求日志和 Request ID；
- panic recovery；
- 认证；
- CORS；
- 限流；
- 指标与 Trace；
- 超时和 Body 大小限制。

项目建立受保护路由组：

```go
authRoutes := router.Group("/").Use(authMiddleware(server.tokenMaker))
authRoutes.POST("/accounts", server.createAccount)
authRoutes.POST("/transfers", server.createTransfer)
```

Middleware 调用 `ctx.AbortWithStatusJSON(...)` 后终止后续链；验证成功则把 Payload 放入 Context，再 `ctx.Next()`。

### 8.4 认证中间件的职责边界

`authMiddleware` 应回答：

1. 有没有 Authorization Header；
2. 是否是支持的 Bearer 方案；
3. Token 是否真实、完整、未过期；
4. 当前请求身份是谁；
5. 把已验证身份安全地传给后续 Handler。

它不应该回答“账户 42 是否属于 Alice”，因为这依赖具体资源。资源所有权由账户/转账 Handler 或业务授权层检查。

这就是：

- Authentication（认证）：你是谁；
- Authorization（授权）：你能否做这件事。

当前 Middleware 使用：

```go
fields := strings.Fields(authorizationHeader)
if len(fields) < 2 { ... }
```

这里应特别注意：它只拒绝少于两个字段，却接受多于两个字段。例如 `Bearer token extra` 会取第二段为 Token、忽略其余内容。更严格的解析应要求恰好两段，或使用经过验证的 Bearer 解析方法。还应根据 Bearer 认证规范考虑返回合适的 `WWW-Authenticate` Header，而不是只返回 JSON 文本。

---

## 9. 参数绑定与校验：输入永远不可信

### 9.1 三种绑定

项目分别演示了：

```go
ctx.ShouldBindJSON(&req)  // JSON Body
ctx.ShouldBindUri(&req)   // /accounts/:id
ctx.ShouldBindQuery(&req) // ?page_id=1&page_size=10
```

`ShouldBind...` 返回错误，由 Handler 自己决定状态码和错误体。相比会自动写响应的绑定方法，它更便于统一 API 错误格式。

### 9.2 tag 校验只解决“结构是否合法”

例子：

```go
Amount int64 `binding:"required,gt=0"`
```

它能防止零和负数，但不能回答：

- 转出账户是否存在；
- 转出账户是否属于当前用户；
- 两个账户币种是否匹配；
- 余额是否充足；
- 同一个请求是否已执行过；
- 并发执行是否会透支。

因此验证至少分三层：

1. 语法/结构校验：JSON、类型、必填、范围；
2. 业务校验：所有权、币种、余额、状态机；
3. 数据库约束与事务：唯一性、外键、原子性、并发最终防线。

绝不能因为 Handler 检查过就删除数据库约束。其他服务、后台任务、脚本或未来新 API 都可能绕过这个 Handler。

### 9.3 自定义币种验证器

`api/server.go` 将 `currency` 注册到 Gin 使用的 validator：

```go
if v, ok := binding.Validator.Engine().(*validator.Validate); ok {
    v.RegisterValidation("currency", validCurrency)
}
```

`api/validator.go` 再调用 `util.IsSupportedCurrency`。好处是请求 struct 可声明式复用 `binding:"currency"`。

但币种列表是业务规则。工业系统通常会：

- 使用 ISO 4217 等明确编码体系；
- 明确大小写规范，通常在边界规范化为大写；
- 区分“系统支持币种”和“某产品允许币种”；
- 把数据库约束或引用表作为最终防线；
- 谨慎处理不同币种的小数位，金额使用最小货币单位或可靠 decimal 类型。

---

## 10. 分页：当前 offset/limit 与工业游标分页

当前列表请求把页码换算为 SQL Offset：

```go
Offset: (req.PageID - 1) * req.PageSize
Limit:  req.PageSize
```

SQL 在 `db/query/account.sql` 中按 `id` 排序：

```sql
SELECT * FROM accounts
WHERE owner = $1
ORDER BY id
LIMIT $2 OFFSET $3;
```

这是 offset/limit 分页。优点：

- 容易理解；
- 可以跳到指定页；
- 适合数据量小、变更少的后台页面。

缺点：

- Offset 很大时数据库需要跳过大量行，可能越来越慢；
- 翻页期间插入或删除数据，可能导致重复或漏项；
- 如果没有稳定、唯一的排序，分页结果会漂移。

工业高流量列表常用 cursor/keyset 分页：

```text
GET /v1/accounts?page_size=20&after_id=1000
```

对应思想：

```sql
WHERE owner = $1 AND id > $after_id
ORDER BY id
LIMIT $page_size;
```

响应包含 `next_page_token`。复杂排序时，游标应编码所有排序键并签名，避免客户端篡改。游标分页不方便任意跳页，所以后台报表仍可能采用 Offset；没有一种方案适合所有场景。

当前代码将 `page_size` 限制在 5 到 10，这能限制单次响应，但最小值 5 是产品选择，不是 HTTP 或 Gin 的要求。工业 API 常允许 1 到某个上限，并设置默认值，而不是要求每次显式提交。

---

## 11. 账户所有权与转账币种校验

### 11.1 创建账户

最初提交 `e2d19a3` 的创建账户请求包含客户端提交的 `owner`。这意味着客户端可以声称替任意用户创建账户。

加入认证后的当前代码删除了请求中的 Owner，改为：

```go
authPayload := ctx.MustGet(authorizationPayloadKey).(*token.Payload)
Owner: authPayload.Username
```

这是正确方向：安全主体必须来自已验证身份，而不是来自可伪造的 JSON。

### 11.2 查询账户

当前流程：

1. 验证 ID；
2. 查账户；
3. 读取认证 Payload；
4. 比较 `account.Owner` 与 `authPayload.Username`；
5. 不同则拒绝。

只做到“Token 有效”还不够。否则任何登录用户都能枚举其他人的账户 ID。

### 11.3 列表账户

当前 `ListAccounts` SQL 包含 `WHERE owner = $1`，Owner 来自 Token。比“先查所有账户，再在 Go 里过滤”更安全也更高效。

### 11.4 创建转账

当前 Handler 先分别查询两个账户并校验请求币种：

```text
请求 currency == 转出账户 currency
请求 currency == 转入账户 currency
```

再检查转出账户属于认证用户。这里的职责是合理的：

- 转出账户所有权：决定调用者有没有权扣款；
- 两端币种相同：当前系统没有外汇兑换，所以禁止跨币种直接转；
- 转入账户不必属于当前用户，否则就无法向别人转账。

但还存在关键边界：这些检查发生在 `TransferTx` 事务**之前**，检查到更新之间可能出现状态变化；而且其他调用方可以直接调用 Store。生产级设计应把关键业务不变量放进事务和数据库约束中，例如正金额、不同账户、余额不可为负，并考虑锁定或条件更新。API 层校验主要用于尽早返回友好错误，不是最终一致性保证。

另外，请求携带 `currency` 主要用于明确客户端意图并防止选错账户，但服务器仍必须以数据库账户币种为事实来源。

---

## 12. 错误映射与统一错误体

当前错误响应只有：

```json
{"error":"原始错误字符串"}
```

这个实现简单，适合教学早期，但工业 API 需要稳定、机器可读且不过度泄露内部信息。例如：

```json
{
  "code": "ACCOUNT_NOT_FOUND",
  "message": "account was not found",
  "request_id": "01J...",
  "details": []
}
```

或采用 RFC 9457 的 `application/problem+json`：

```json
{
  "type": "https://api.example.com/problems/account-not-found",
  "title": "Account not found",
  "status": 404,
  "detail": "The requested account does not exist.",
  "instance": "/requests/01J..."
}
```

设计原则：

- `code` 或 `type` 稳定，客户端不要解析人类文本；
- `message/detail` 可以本地化，但不要放 SQL、栈、密钥或内部拓扑；
- HTTP 状态码与 Body 中的 status 必须一致；
- 日志里记录内部错误链和 Request ID，对外只返回安全信息；
- 验证错误可包含字段路径与稳定原因，例如 `amount must be greater than 0`；
- 建立领域错误到 HTTP 的集中映射，避免每个 Handler 随意决定。

当前 `errorResponse(err)` 直接调用 `err.Error()`，可能把 PostgreSQL 错误细节返回客户端。这不仅造成不稳定契约，也可能泄露表名、约束名或输入细节。

---

## 13. 依赖注入：为什么 Server 持有 Store 接口

依赖注入听起来复杂，其实核心只是“不要在 Handler 里偷偷创建真实数据库”。

当前结构：

```go
type Server struct {
    config     util.Config
    store      db.Store
    tokenMaker token.Maker
    router     *gin.Engine
}
```

生产启动时注入真实 `db.Store`；单元测试时注入 `mockdb.MockStore`。

好处：

- Handler 只依赖能力接口，不依赖数据库连接细节；
- 测试不需要启动 PostgreSQL；
- 可以精确验证“错误输入时绝不调用数据库”；
- 可以模拟找不到、冲突、连接断开等难复现错误。

提交 `ebf8c36` 是这个设计的关键节点：`Store` 从具体实现演进为接口，并生成 gomock，随后加入 API 测试。

不要为了“依赖注入”引入巨大框架。Go 中构造函数参数和小接口通常足够。接口应该由使用方需要的行为驱动，而不是无脑给每个 struct 建一个同名接口。

---

## 14. httptest、gomock 与表驱动测试

### 14.1 httptest 不开真实端口也能测 HTTP

`httptest.NewRecorder()` 捕获响应：

```go
recorder := httptest.NewRecorder()
request, _ := http.NewRequest(http.MethodGet, "/accounts/1", nil)
server.router.ServeHTTP(recorder, request)
```

这仍然经过真实 Gin Router、Middleware、绑定和 Handler，只是没有监听 TCP 端口。它快、稳定，适合单元测试 HTTP 行为。

### 14.2 gomock 声明依赖交互

成功读取账户的期望：

```go
store.EXPECT().
    GetAccount(gomock.Any(), gomock.Eq(account.ID)).
    Times(1).
    Return(account, nil)
```

它不仅准备返回值，也验证 Handler 是否以正确参数调用一次 Store。无认证或非法 ID 用 `Times(0)`，证明请求在边界处已被拒绝，没有触碰数据库。

### 14.3 表驱动测试

Go 常把场景放进 slice：

```go
testCases := []struct {
    name          string
    setupAuth     func(...)
    buildStubs    func(...)
    checkResponse func(...)
}{
    {name: "OK", ...},
    {name: "NotFound", ...},
    {name: "InvalidID", ...},
}
```

再用 `t.Run` 创建子测试。优点：

- 输入、Mock 行为和期望输出并排；
- 新边界只增加一行场景结构；
- 测试失败时显示具体场景名；
- 避免复制大量搭建代码。

### 14.4 自定义 Matcher 为什么必要

bcrypt 每次哈希都有随机 salt，同一个密码得到的哈希字符串也不同。因此创建用户测试不能直接比较预先计算的哈希。

`eqCreateUserParamsMatcher` 的做法是：

1. 取出 Handler 生成的 `HashedPassword`；
2. 用 `CheckPassword` 验证它确实对应输入明文；
3. 再比较其他字段。

这比要求具体哈希字节完全相等更符合行为。

### 14.5 测试还缺什么

当前已有创建用户、登录、获取账户和认证 Middleware 等测试，但工业级还应补：

- Content-Type 错误、畸形 JSON、未知字段、超大 Body；
- Header 恰好两段和额外字段；
- 创建/列表账户、转账、续期的完整分支；
- 所有权和币种不匹配时 `TransferTx` 必须 `Times(0)`；
- 领域错误到状态码和错误码的映射；
- 幂等并发测试；
- 数据库真实约束的集成测试；
- Fuzz 测试输入解析；
- Race detector 覆盖共享状态代码。

Mock 测试不能证明 SQL、事务和数据库约束正确，所以单元测试与集成测试必须互补。

---

## 15. 回到 Git 历史：这一阶段是怎样长出来的

### 15.1 `e2d19a3`：第一次建立 Gin 账户 API

这个提交加入：

- `api/server.go`；
- 创建、获取、列表账户 Handler；
- Gin Router；
- `main.go` 启动 HTTP Server。

当时 `Server` 持有具体 `*db.Store`，创建账户请求由客户端提交 Owner，币种校验写死 `USD EUR`。它展示最小可运行 API，但还没有认证和良好可测试性。

### 15.2 `ee825a2`：配置从代码中移出

引入 Viper，从 `app.env` 与环境变量读取数据库驱动、连接地址和服务地址。原理是配置与程序分离，使开发、测试、容器环境使用不同值。

工业界进一步要求：

- 默认值与必填项清楚；
- 启动时校验配置并 fail fast；
- 密钥绝不提交 Git；
- Secret 由环境、Kubernetes Secret 或云 Secret Manager 注入；
- 日志不得打印完整 DSN 和密钥。

本文不会展示当前 `app.env` 中的任何秘密。

### 15.3 `ebf8c36`：Store 接口、gomock、HTTP 单元测试

这次演进把数据库依赖抽象为 `db.Store` 接口，生成 Mock，并为获取账户 API 加测试。这是从“能运行”向“可验证、可维护”迈出的关键一步。

### 15.4 `502b17c`：转账 API 和自定义币种验证

加入 `/transfers`，请求验证正金额、账户 ID 和币种；在事务前查询两端账户并验证币种。当时还没有认证，因此没有校验转出账户属于谁。

### 15.5 `414d981`：登录 API 与 Token

加入用户登录、密码检查、Access Token 生成，并把配置中的 Token Key 和持续时间注入 Server。随后项目继续演进出 Refresh Token 与 Session，当前 `api/user.go` 已比这个提交更完整。

### 15.6 `47561e0`：认证 Middleware 和资源所有权

这个提交把公开路由与受保护路由分组；创建账户的 Owner 改为 Token 中用户名；读取账户和转账加入所有权检查；列表 SQL 按 Owner 过滤。

提交信息说明当时测试尚未完成。

### 15.7 `3a77ae5`：补全 backend22 测试

加入 `UnauthorizedUser`、`NoAuthorization` 等获取账户场景，并断言无认证时 Store 不被调用。它体现安全逻辑也必须进入回归测试。

### 15.8 当前代码与历史版本不要混看

当前仓库后来已经切换到 pgx，错误判断使用 `db.ErrRecordNotFound` 和 PostgreSQL SQLSTATE 封装；用户模型也增加 Role，登录增加 Refresh Token 和 Session。阅读旧提交时看到 `database/sql`、`lib/pq` 或较旧 Token 方法签名是正常的版本演进，不是当前代码完全相同。

---

## 16. 仅供实验的 curl 示例

> **前提再次确认：当前 `main()` 不启动 Gin。以下路径只有在你单独接线并启动 `runGinServer`、准备好数据库与安全的本地配置后才适用。不要把真实密码或仓库里的任何密钥粘贴进命令、聊天或截图。**

假设实验 Gin 服务监听 `http://127.0.0.1:8081`。

### 16.1 创建用户

```bash
curl -i \
  -X POST http://127.0.0.1:8081/users \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"local-demo-password","full_name":"Alice","email":"alice@example.test"}'
```

### 16.2 登录并手工保存 Access Token

```bash
curl -i \
  -X POST http://127.0.0.1:8081/users/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"local-demo-password"}'
```

从本地响应中复制 Access Token，只在当前 shell 实验中替换下面的 `<access-token>`。不要把它提交到 Git。

### 16.3 创建账户

```bash
curl -i \
  -X POST http://127.0.0.1:8081/accounts \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <access-token>' \
  -d '{"currency":"USD"}'
```

### 16.4 分页列出自己的账户

```bash
curl -i \
  'http://127.0.0.1:8081/accounts?page_id=1&page_size=5' \
  -H 'Authorization: Bearer <access-token>'
```

### 16.5 转账

```bash
curl -i \
  -X POST http://127.0.0.1:8081/transfers \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <access-token>' \
  -d '{"from_account_id":1,"to_account_id":2,"amount":100,"currency":"USD"}'
```

这只是展示当前契约。当前接口没有幂等键，不要用真实资金场景测试，也不要在响应不确定时盲目重试。

---

## 17. 当前 `api/` 的真实代码审查清单

这一节不是要求你立刻全部重构，而是训练你从“教学代码”看到“生产差距”。

### 17.1 未启用与契约分裂

- `runGinServer` 未被调用；
- `api/` 与 `gapi/` 存在两套用户认证实现，容易行为漂移；
- 当前 Gateway 没有账户和转账定义；
- 如果未来同时启用 Gin 和 Gateway，还会争用同一个 HTTP 地址；
- 应明确选择唯一对外契约，或清楚划分兼容层并复用同一业务服务。

### 17.2 `renewAccessToken` 返回了错误的响应类型

`api/token.go` 定义了精简的：

```go
type renewAccessTokenResponse struct {
    AccessToken          string
    AccessTokenExpiresAt time.Time
}
```

但 Handler 实际构造的是 `loginUserResponse`。由于其他字段没有 `omitempty`，JSON 会额外出现零值 Session ID、空 Refresh Token、零时间和空 User。它可能不导致编译失败，却破坏续期接口契约；现有精简类型反而没有使用。

### 17.3 Authorization 解析不够严格

- 使用 `len(fields) < 2`，额外字段会被忽略；
- 未返回 `WWW-Authenticate`；
- 对外直接返回 Token 校验错误字符串，可能暴露实现细节；
- 认证失败应该统一安全响应，内部日志再记录具体原因。

### 17.4 状态码和错误体

- 创建资源使用 200，而非常见的 201；
- 重复冲突使用 403，而非常见的 409；
- 资源所有权失败使用 401，而不是 403/404 策略；
- 原始数据库错误直接暴露；
- 没有稳定错误码、Request ID 和字段错误结构。

### 17.5 服务级防护

Gin 的 `router.Run` 适合快速启动，但生产环境通常显式构造 `http.Server`，配置：

- `ReadHeaderTimeout`；
- 读取 Body 的期限与大小限制；
- `WriteTimeout`（需要结合流式响应场景设计）；
- `IdleTimeout`；
- 优雅关闭；
- 请求级 deadline 并向数据库传递取消信号。

超时不是越短越好。网关、应用和下游超时必须有预算关系，避免上游已经放弃，下游仍长时间占用资源。

### 17.6 CORS

CORS 是浏览器的跨源访问策略，不是认证，也不能阻止 curl 或服务端客户端调用。生产配置应明确允许：

- 哪些 Origin；
- 哪些方法和 Header；
- 是否允许凭证；
- 预检缓存多久。

使用 Cookie 凭证时不能随意把允许源设为 `*`；Token 放 Header 也仍需防 XSS 和前端存储泄露。

### 17.7 限流

登录、注册、邮箱验证、转账都需要不同维度的限流：IP、用户、设备、账户、全局容量。触发时通常返回 `429`，必要时提供 `Retry-After`。

分布式服务可用 Redis 或 API Gateway 协调限流，但 Redis 失败时采取 fail-open 还是 fail-closed 必须按接口风险决定。银行转账和公开内容读取的策略可能不同。

### 17.8 日志与隐私

日志应该帮助关联问题，但不得记录：

- Authorization/Refresh Token；
- 密码和完整请求 Body；
- SMTP 或数据库密钥；
- 银行隐私数据和不必要的个人信息。

建议记录 Request ID、路由模板、状态码、耗时、经过分类的错误码、调用方匿名标识。对账号、邮箱和 IP 应按隐私政策脱敏、限制访问和设置保留周期。

### 17.9 OpenAPI

OpenAPI 描述路径、参数、Schema、认证和响应，可用于：

- 生成交互文档；
- 生成或辅助生成客户端；
- 契约审查和兼容性检查；
- 网关校验与测试生成。

但文档必须与运行实现同步。当前 OpenAPI 是从 Proto/Gateway 生成，描述的是 gRPC 用户接口，不会自动覆盖遗留 Gin 的账户和转账路由。不要让 Swagger 页面制造“文档里有/代码里有，所以线上一定有”的错觉。

---

## 18. 推荐的工业分层（不是要求立刻照搬）

当业务增大，可以演进为：

```text
HTTP/Gin Adapter
  ├─ 路由、绑定、认证信息提取
  ├─ HTTP 状态码和 DTO
  ▼
Application / Use Case
  ├─ CreateAccount
  ├─ TransferMoney
  └─ 领域授权与业务编排
  ▼
Repository / Store Interface
  ▼
PostgreSQL / Redis / 外部服务
```

这样 Gin 与 gRPC 都调用同一 Use Case，不会复制两套业务规则。错误也先定义成领域错误，再分别映射到 HTTP Status 和 gRPC Code。

不过，当前 SimpleBank 规模小，直接 `Handler -> Store` 便于学习。只有当重复逻辑和复杂规则真实出现时再抽业务层。过早建立十层空接口，只会让初学者找不到代码真正执行的位置。

---

## 19. 动手练习

### 练习 1：修正续期响应类型

目标：让 `renewAccessToken` 使用 `renewAccessTokenResponse`，并写测试断言响应只包含访问 Token 和过期时间。

验收：

- 不出现 Session ID、Refresh Token、User；
- 无效 Token、Session 不存在、Session blocked、用户不匹配、过期都覆盖；
- Store 调用次数正确。

### 练习 2：严格解析 Authorization Header

增加场景：

- 空 Header；
- 只有 `Bearer`；
- `Basic xxx`；
- `Bearer token extra`；
- 过期 Token；
- 正常 Token。

验收：除恰好两段且方案正确、Token 有效外，全部 401；认证失败不调用业务 Handler。

### 练习 3：统一错误响应

设计稳定错误体，至少包含 `code`、`message`、`request_id`。把数据库原始错误仅记录到服务日志，对外返回安全文本。

验收：测试只依赖稳定 code，不解析错误 message；响应不出现 SQL、表名或约束名。

### 练习 4：修正状态码

建议练习映射：

- 创建成功：201 + Location；
- 重复用户/账户：409；
- 无凭证：401；
- 已认证但无所有权：选择 403 或统一 404，并写下安全理由；
- 记录不存在：404。

验收：表驱动测试覆盖每个映射。

### 练习 5：实现游标分页

新增按 `id` 的 `after_id` 查询，返回 `next_page_token` 或 `next_after_id`。

验收：连续翻页无重复；中间插入新 ID 时行为可解释；每页严格受最大上限控制。

### 练习 6：设计转账幂等性

先写设计，不急着编码：

- 幂等键由谁生成；
- 唯一约束是什么；
- 怎样保存请求摘要；
- 相同键不同参数怎么办；
- 记录和 TransferTx 怎样同事务；
- 并发两个相同键时怎样只执行一次；
- 处理中、成功、失败各返回什么。

### 练习 7：消除 Gin 与 gRPC 业务复制

把“创建用户”或“转账”抽成一个小 Use Case，由 Gin/Gateway 适配层共同调用。不要一次重构全项目。

验收：同一业务规则只有一个实现；两个传输层只负责各自 DTO 和错误码转换。

---

## 20. 自测题

先自己回答，再看下一节答案。

1. Path 参数和 Query 参数的主要区别是什么？
2. `POST /transfers` 为什么不能在超时后默认重试？
3. `401` 与 `403` 有什么区别？
4. 为什么 Token 中已有用户名，Handler 还要查账户 Owner？
5. `binding:"gt=0"` 为什么不能保证账户不会透支？
6. `ShouldBindJSON` 失败后为什么不应调用 Store？
7. 为什么 bcrypt 哈希不能在 gomock 中做普通字符串相等比较？
8. offset 分页在高页码和数据频繁变化时有什么问题？
9. 为什么统一错误体不应直接返回 `err.Error()`？
10. 当前启动程序为什么访问不到 Gin 的 `/accounts`？
11. gRPC-Gateway 的 Swagger 为什么不代表账户与转账已经对外提供？
12. Middleware 应该做认证，账户所有权为什么通常留给 Handler/授权层？
13. 数据库事务原子性为什么不等于 HTTP 请求幂等性？
14. CORS 能否阻止 curl 调用接口？
15. 当前 renew Handler 使用 `loginUserResponse` 会造成什么现象？

## 21. 自测题答案

1. Path 主要标识具体资源或层级，Query 主要表达过滤、排序、分页等可选条件。
2. POST 默认不保证幂等；请求可能已成功但响应丢失，重试可能再次扣款。需要服务端幂等键机制。
3. 401 表示没有有效认证身份；403 表示身份已确认但权限不足。出于防枚举策略也可对某些资源统一返回 404。
4. Token 只能说明请求者是谁；账户记录才说明资源属于谁。比较二者才能做资源级授权。
5. tag 只检查单个请求数值。余额、并发更新和最终不变量必须由事务、条件更新、锁和数据库约束保证。
6. 输入不合法时继续访问数据库浪费资源，也可能让危险输入进入下游。测试用 `Times(0)` 锁定这个边界。
7. bcrypt 带随机 salt；同一明文每次哈希不同。应使用 `CheckPassword` 验证行为。
8. 大 Offset 可能变慢；翻页间插入/删除会导致重复或漏项。稳定排序和游标分页能改善。
9. 原始错误不稳定，且可能泄露 SQL、约束、内部拓扑或安全信息。客户端需要稳定 code，内部细节留在受控日志。
10. 当前 `main()` 没调用 `runGinServer`，启动的是 gRPC、Gateway 和 Worker。
11. 当前 OpenAPI 从 Proto 生成，而 Proto 没定义账户和转账 RPC；`api/` 中存在代码不等于运行契约包含它。
12. Middleware 适合验证通用身份；所有权依赖具体资源数据。混在全局 Middleware 会产生耦合，也难表达不同资源规则。
13. 事务保证一次调用内要么全成功要么全失败；两次相同 HTTP 请求仍可能开启两次事务并各成功一次。
14. 不能。CORS 是浏览器执行的跨源策略，不是服务器端认证或防火墙。
15. 响应会带上登录响应的其他零值字段，而不是只返回新 Access Token 和过期时间，导致契约污染。

---

## 22. 掌握清单

当你能不看答案解释并实践以下内容，就算掌握这一阶段：

- [ ] 能画出请求从 Router、Middleware、Handler 到 Store 的调用链；
- [ ] 能区分 path、query、header、body 的用途；
- [ ] 能解释 GET/POST/PUT/PATCH/DELETE 的语义与幂等性；
- [ ] 能为常见成功、认证、授权、冲突和服务错误选择合理状态码；
- [ ] 能用 Go struct tag 绑定 JSON、URI、Query 并理解零值问题；
- [ ] 能解释认证与资源授权的边界；
- [ ] 能说明当前账户所有权与币种校验怎样工作、哪里还不是最终防线；
- [ ] 能解释 offset 与 cursor 分页的取舍；
- [ ] 能用构造函数注入 Store 接口；
- [ ] 能用 httptest 不开端口测试 Gin；
- [ ] 能用 gomock 声明调用参数、次数和返回值；
- [ ] 能写表驱动测试并覆盖成功与失败分支；
- [ ] 能设计稳定错误体并避免泄露内部错误；
- [ ] 能解释 OpenAPI 的价值及“文档必须与运行路径一致”；
- [ ] 能说明超时、限流、CORS、幂等、日志隐私各解决什么问题；
- [ ] 能准确说出当前 `api/` 未由 `main()` 启动，Gateway 也没有账户/转账 RPC；
- [ ] 能指出 Authorization 解析、状态码和 renew 响应类型的当前问题。

---

## 23. 事实依据与延伸阅读

### 仓库内核对入口

- `main.go`：当前实际启动路径与未调用的 `runGinServer`；
- `api/server.go`：Gin Server、路由、受保护路由组；
- `api/account.go`：账户绑定、分页和所有权；
- `api/transfer.go`：转账参数、币种和转出所有权；
- `api/user.go`：用户 DTO、登录与 Session；
- `api/token.go`：续期流程与响应类型问题；
- `api/middleware.go`：Bearer Token 解析；
- `api/*_test.go`：httptest、gomock、表驱动测试；
- `db/query/account.sql`：按 Owner 查询与 offset/limit；
- `proto/service_simple_bank.proto`：当前 Gateway 真正拥有的 RPC；
- 指定 Git 提交：`e2d19a3`、`ee825a2`、`ebf8c36`、`502b17c`、`414d981`、`47561e0`、`3a77ae5`。

### 正式标准与官方文档

- [RFC 9110: HTTP Semantics](https://www.rfc-editor.org/rfc/rfc9110.html)：HTTP 方法、Header、状态码、安全与幂等语义；
- [RFC 9457: Problem Details for HTTP APIs](https://www.rfc-editor.org/rfc/rfc9457.html)：`application/problem+json` 统一错误模型；
- [OpenAPI Specification](https://spec.openapis.org/oas/)：API 描述格式的正式规范入口；
- [Gin 官方文档](https://gin-gonic.com/docs/)：路由、绑定、Middleware 与测试用法；
- [Go `net/http/httptest`](https://pkg.go.dev/net/http/httptest)：Go 标准库 HTTP 测试工具。

读标准时不要试图一次记住全部条款。先用本文中的具体接口建立直觉，再回到 RFC 核对语义。真正可靠的后端能力，不是背出状态码列表，而是能让协议契约、业务规则、数据库不变量、测试和运行部署彼此一致。
