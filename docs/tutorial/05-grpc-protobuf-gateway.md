# 第 5 阶段：从 RPC 到 gRPC、Protocol Buffers、Gateway 与 OpenAPI

> 面向读者：第一次学习后端、Go 和 gRPC 的同学。本文以当前仓库 `ce0b978` 为事实基线，并回看 gRPC 相关提交的演进。文中的“当前项目”表示代码已经这样做；“工业实践”表示生产系统通常还应补齐的能力，二者不会混写。

这一阶段最重要的，不是记住几条 `protoc` 命令，而是建立一条完整的思维链：**接口契约如何定义，代码如何由契约生成，请求如何从网络进入 Go 方法，错误、身份和超时如何沿调用链传递，同一份契约又如何服务 gRPC 客户端和 HTTP/JSON 客户端。**

学完后，你应该能回答：

1. RPC、REST、HTTP/2 和 Protobuf 各自解决什么问题，它们不是怎样的同义词；
2. `.proto` 中的字段编号为什么比字段名更“不能改”；
3. `proto3 optional` 为什么是部分更新的关键；
4. `pb/*.go` 中哪些代码由哪个插件生成，为什么不应手改；
5. gRPC 状态码、错误详情、Metadata、Interceptor、Reflection、Deadline 各有什么职责；
6. grpc-gateway 在本项目里究竟是“网络代理”还是“进程内适配器”；
7. 当前项目已经实现什么、还缺什么，以及生产环境如何设计。

---

## 1. 先建立总图：一次调用到底经过了什么

当前 SimpleBank 同时启动两个入口：

```text
原生 gRPC 客户端
    │  HTTP/2 + Protobuf，默认地址 :9090
    ▼
grpc.Server ── Unary Interceptor ── gapi.Server 方法

HTTP/JSON 客户端
    │  HTTP + JSON，默认地址 :8081
    ▼
net/http ── HttpLogger ── grpc-gateway ServeMux
    │
    └── 进程内直接调用另一份 gapi.Server 方法
        （没有拨号 :9090，也不经过 grpc.Server 的 Unary Interceptor）

gapi.Server
    ├── db.Store → PostgreSQL
    ├── token.Maker
    └── worker.TaskDistributor → Redis/Asynq
```

请先记住两个容易误解的事实。

第一，**Gateway 不是把 JSON 原封不动转发给 9090**。`main.go` 调用的是：

```go
pb.RegisterSimpleBankHandlerServer(ctx, grpcMux, server)
```

这个注册方式由生成代码在当前进程里直接执行 `server.CreateUser(...)` 等方法。生成文件 `pb/service_simple_bank.pb.gw.go` 自己也明确提示：这种模式会让许多 gRPC 库能力不起作用，尤其是 gRPC Server Interceptor；流式 RPC 也不由这个本地注册模式支持。如果改用 `RegisterSimpleBankHandlerFromEndpoint`，Gateway 才会建立 `grpc.ClientConn`，经网络或本机套接字调用一个 gRPC endpoint。

第二，**当前 Proto 服务并不是完整银行服务**。`proto/service_simple_bank.proto` 只有四个 RPC：

- `CreateUser`
- `UpdateUser`
- `LoginUser`
- `VerifyEmail`

账户 CRUD 和转账虽然存在于旧的 Gin `api/` 中，但当前 `main()` 没有调用 `runGinServer`，Proto 中也没有 Account 或 Transfer RPC。因此从当前真正启动的服务入口，不能通过 gRPC/Gateway 创建账户或转账。

---

## 2. RPC 与 REST：比较的是两种接口思维，不是两种传输协议

### 2.1 RPC 是什么

RPC 是 Remote Procedure Call，远程过程调用。它试图让客户端像调用本地函数一样调用远端服务：

```text
LoginUser(LoginUserRequest) → LoginUserResponse
```

当然，远程调用永远不等于本地调用。远端可能超时、断网、重复执行、服务端已经成功但响应丢失；本地函数通常不会面对这些不确定性。一个成熟的 RPC 设计必须显式处理这些“分布式系统语义”。

gRPC 是一个 RPC 框架。它提供服务定义、跨语言代码生成、客户端连接、HTTP/2 传输、序列化、状态码、Metadata、Deadline、流式调用等完整机制。gRPC 默认用 Protocol Buffers 同时充当接口描述语言（IDL）和消息编码格式，但“RPC”这个概念本身并不等于 Protobuf。

### 2.2 REST 是什么

REST 是一种架构风格，强调把系统看作资源集合，通过统一接口操作资源。常见 HTTP API 会设计成：

```http
POST  /v1/users
GET   /v1/users/alice
PATCH /v1/users/alice
POST  /v1/transfers
```

HTTP 方法、URL、状态码、缓存语义共同表达操作。REST 并不强制 JSON，但现实中 HTTP/JSON 最常见。

本项目的 Gateway 路径是：

```http
POST  /v1/create_user
PATCH /v1/update_user
POST  /v1/login_user
GET   /v1/verify_email
```

这些 URL 更偏“动作式 RPC 映射”，不算特别资源化。例如工业界更常见 `POST /v1/users` 与 `PATCH /v1/users/{username}`。这不是说现有路径不能用，而是要明白“能通过 HTTP 调用”不自动等于“严格的 REST 资源建模”。

### 2.3 怎么选择

典型取舍如下：

| 维度 | gRPC | 常见 REST/HTTP JSON |
|---|---|---|
| 契约 | `.proto` 强类型，天然代码生成 | OpenAPI 可做到契约优先，也常被手写成弱约束 |
| 编码 | Protobuf 二进制，通常更紧凑 | JSON 可读性好，调试直观 |
| 传输 | 通常基于 HTTP/2 | HTTP/1.1、HTTP/2、HTTP/3 都可能 |
| 流式 | 原生支持四种调用形态 | 普通 REST 不擅长；常另用 SSE/WebSocket |
| 浏览器 | 浏览器不能直接使用完整原生 gRPC，常需 Gateway 或 gRPC-Web | 原生友好 |
| 跨语言 SDK | 生成体验很好 | 依赖 OpenAPI 生成器质量与契约质量 |
| 公网调试 | 工具要求稍高 | `curl`、浏览器很方便 |

工业界常见组合正是本项目的方向：内部服务间使用 gRPC，外部或浏览器入口提供 HTTP/JSON Gateway。但这不是硬规则；小型系统完全可以只用 HTTP/JSON，公开 API 也可以直接提供 gRPC。

---

## 3. HTTP/2 基础：gRPC 为什么依赖它

HTTP/2 仍然是 HTTP：有请求、响应、Header 和状态；它改变了数据在连接上的组织方式。

### 3.1 二进制分帧与 Stream

HTTP/2 把连接上的信息拆成二进制 Frame，并把属于同一次请求/响应的 Frame 归入同一个 Stream。每个 Stream 有编号，因此一条 TCP 连接上可以同时承载多组请求响应。

这叫多路复用。HTTP/1.1 常需要多个连接，或者遇到管线化限制；HTTP/2 可以让多个 Stream 的 Frame 交错传输。注意：它消除了 HTTP/1.1 应用层队头阻塞，但若底层使用 TCP，丢包仍可能造成 TCP 层队头阻塞。

### 3.2 Header 压缩、流量控制和长连接

HTTP/2 使用 HPACK 压缩重复 Header；还提供连接级、Stream 级流量控制，避免发送方无限灌入接收方处理不了的数据。gRPC 复用长连接，在一个连接上并发调用，减少反复握手的成本。

gRPC 在 HTTP/2 上规定了自己的消息封装、Content-Type、Header/Trailer 等约定。一次 RPC 的最终 gRPC 状态通常通过 Trailer 表达，而不是简单把 HTTP 状态码当成全部业务结果。不要把“HTTP 200”直接理解为“业务一定成功”；客户端应读取 gRPC status。

### 3.3 流式能力

HTTP/2 的双向 Stream 为 gRPC 的流式 RPC 提供基础。gRPC 定义四种方法形态：

```proto
// 一元：一个请求，一个响应
rpc GetUser(GetUserRequest) returns (GetUserResponse);

// 服务端流：一个请求，多条响应
rpc ListEvents(ListEventsRequest) returns (stream Event);

// 客户端流：多条请求，一个响应
rpc UploadChunks(stream Chunk) returns (UploadResult);

// 双向流：双方都可连续发送
rpc Chat(stream ChatMessage) returns (stream ChatMessage);
```

当前 SimpleBank 四个方法全部是一元 RPC，没有任何 `stream`。不要仅因为依赖了 gRPC 就声称项目已经使用流式处理。

---

## 4. Protocol Buffers：先懂 Schema，再看生成代码

### 4.1 `.proto` 是接口契约

看 `proto/rpc_login_user.proto`：

```proto
syntax = "proto3";

package pb;

import "user.proto";
import "google/protobuf/timestamp.proto";

option go_package = "github.com/lllmml/simplebank/pb";

message LoginUserRequest {
    string username = 1;
    string password = 2;
}
```

- `syntax = "proto3"` 选择 Proto3 语法；
- `package pb` 是 Protobuf 类型与服务的命名空间，服务全名因此是 `pb.SimpleBank`；
- `import` 引用别的消息定义；
- `go_package` 决定生成 Go 代码的导入路径；
- `message` 描述结构化消息；
- `= 1`、`= 2` 是字段编号，不是默认值，也不是显示顺序。

### 4.2 字段编号才是线上身份

Protobuf 二进制 Wire Format 主要通过“字段编号 + wire type”识别数据。字段名主要服务源码、JSON 映射和人类阅读。因此一旦契约发布：

- 不要修改现有字段编号；
- 不要把旧编号复用给含义不同的新字段；
- 新增字段使用全新编号；
- 删除字段后 `reserved` 旧编号，最好也保留旧名称；
- 不要随意改变字段类型；有些类型在二进制层面看似可解析，业务语义或 JSON 仍可能破坏。

正确的删除方式示例：

```proto
message User {
    reserved 6;
    reserved "legacy_phone";

    string username = 1;
    // ...
}
```

字段号 `1` 到 `15` 的 tag 编码通常更省空间，适合高频字段；这属于优化建议，绝不能为了省几个字节重排已经发布的编号。

### 4.3 默认值不等于“客户端明确传了这个值”

Proto3 标量默认值包括：

- `string` → `""`
- 数值 → `0`
- `bool` → `false`
- enum → 数值为 `0` 的枚举项
- repeated/map → 空集合语义

普通标量字段使用隐式 presence 时，读取“没传的字段”和“明确传了默认值的字段”得到相同结果。例如：

```proto
string full_name = 2;
```

服务端只看到 `GetFullName() == ""` 时，无法判断客户端是没打算修改姓名，还是打算把姓名设为空。这对 `PATCH` 是致命歧义。

### 4.4 `optional` 与字段存在性

当前 `proto/rpc_update_user.proto` 正确地使用了：

```proto
message UpdateUserRequest {
    string username = 1;
    optional string full_name = 2;
    optional string email = 3;
    optional string password = 4;
}
```

生成的 Go 结构在 `pb/rpc_update_user.pb.go` 中大致是：

```go
FullName *string
Email    *string
Password *string
```

于是：

```text
字段没传       → req.Email == nil
明确传 ""     → req.Email != nil，且 req.GetEmail() == ""
明确传有效邮箱 → req.Email != nil，且 Getter 返回该邮箱
```

Getter 只返回值；要判断 presence，必须检查指针是否为 `nil`。本项目的完整部分更新链路非常值得记住：

```text
Proto optional
  ↓
生成的 Go *string 是否为 nil
  ↓
gapi/rpc_update_user.go 设置 pgtype.Text.Valid
  ↓
Valid=false 作为 SQL NULL 参数
  ↓
db/query/user.sql 的 sqlc.narg(...) 生成可空参数
  ↓
COALESCE(NULL, 原列) 保留原值
```

对应代码是：

```go
Email: pgtype.Text{
    String: req.GetEmail(),
    Valid:  req.Email != nil,
},
```

对应 SQL 是：

```sql
email = COALESCE(sqlc.narg(email), email)
```

因此“未提供 email”不会覆盖旧值。若明确提供空字符串，`Valid=true`，但 RPC 校验会拒绝非法空邮箱。这是 API presence、Go 类型、数据库 NULL 语义协同的实例。

工业界复杂 PATCH 常使用 `google.protobuf.FieldMask`，尤其当字段本身允许默认值、嵌套对象更新、清空字段等语义越来越复杂时。`optional` 适合当前这种简单局部更新；FieldMask 则把“要改哪些路径”显式列出来。

### 4.5 二进制兼容不等于 JSON 兼容，也不等于业务兼容

新增字段通常对 Protobuf 二进制是向前/向后兼容的：老程序遇到不认识的字段可以忽略，新程序读老消息则取得默认值。但仍要同时检查：

1. 生成代码层面是否让调用方编译失败，例如 exhaustive enum switch；
2. ProtoJSON 的字段名和枚举文本是否兼容；
3. 业务规则是否允许新旧服务在滚动发布期间共存；
4. 数据库存储和消息队列中是否长期保存了旧消息。

生产团队通常在 CI 中加入 breaking-change 检查，并制定“只新增、先弃用、后删除、永不复用 tag”的演进策略。可使用 Buf 等工具做 lint 和 breaking 检查；本仓库当前尚未配置 Buf。

---

## 5. `protoc` 与生成物：契约如何变成 Go 代码

### 5.1 编译器和插件各做什么

`protoc` 负责解析 `.proto` 和调用插件。它本身不会凭空生成所有语言、Gateway 和 Swagger 代码。当前 Makefile 的 `proto` 目标依次使用：

| 参数/插件 | 当前生成结果 |
|---|---|
| `--go_out=pb` / `protoc-gen-go` | 消息类型、字段 Getter、反射描述，文件名通常为 `*.pb.go` |
| `--go-grpc_out=pb` / `protoc-gen-go-grpc` | `SimpleBankClient`、`SimpleBankServer`、注册函数与 Handler，文件为 `*_grpc.pb.go` |
| `--grpc-gateway_out=pb` | HTTP 路由和 JSON↔Protobuf 转码 Handler，文件为 `*.pb.gw.go` |
| `--openapiv2_out=doc/swagger` | OpenAPI v2（Swagger）JSON |
| `statik` | 把 `doc/swagger` 静态文件转为 `doc/statik/statik.go`，嵌入二进制 |

`--go_opt=paths=source_relative` 表示生成文件相对路径跟源 Proto 文件对应，而不是按 Go import path 创建多层目录。Gateway 插件也设置了同样选项。

`--openapiv2_opt=allow_merge=true,merge_file_name=simple_bank` 把多个 Proto 输入合并为 `simple_bank.swagger.json`。Makefile 在生成前先删除旧 `pb/*.go` 和旧 Swagger JSON，避免删除 RPC 后旧生成文件残留。

当前命令还带：

```text
--experimental_allow_proto3_optional
```

这是因为生成文件头显示仓库所用 `protoc` 为 `v3.12.4`；Proto3 optional 在 3.12 时需要实验标志，从 3.15 起默认支持。生成文件同时显示插件版本，例如 `protoc-gen-go v1.36.11` 和 `protoc-gen-go-grpc v1.6.2`。**编译器版本和插件版本是两回事。**

完整生成入口是：

```bash
make proto
```

这要求本机 PATH 中已经安装兼容版本的 `protoc`、`protoc-gen-go`、`protoc-gen-go-grpc`、`protoc-gen-grpc-gateway`、`protoc-gen-openapiv2` 和 `statik`。当前仓库没有把这些工具版本完整锁进容器或统一脚本；因此不同开发机可能生成不同 diff。工业项目通常用 Buf、Docker 工具镜像、Make/Taskfile 加版本检查，或 CI 重生成后检查工作区是否干净，保证可复现。

### 5.2 为什么不要手改 `pb/`

生成文件第一行明确写着 `Code generated ... DO NOT EDIT.`。手改的问题是：

- 下一次 `make proto` 会全部覆盖；
- 源契约和运行代码不一致；
- 其他语言客户端无法得到相同修改；
- Code Review 无法区分契约变化和临时补丁。

正确流程是：修改 `proto/*.proto` → `make proto` → 在 `gapi/` 实现生成接口 → 更新测试和文档。生成文件应提交还是由 CI 构建，是团队策略问题；当前仓库选择提交 `pb/` 和 Swagger/Statik 生成结果。

### 5.3 生成的 Server 为什么要嵌入

`gapi.Server` 嵌入：

```go
pb.UnimplementedSimpleBankServer
```

生成的 `SimpleBankServer` 接口包含四个 RPC 和一个防止外部随意实现的嵌入要求。嵌入默认实现的好处是：将来 Proto 新增 RPC 时，旧服务端可以先以 `Unimplemented` 响应，而不是立刻因为缺方法无法编译；当前新版生成代码还要求按值嵌入，避免 nil 指针问题。

随后：

```go
pb.RegisterSimpleBankServer(grpcServer, server)
```

将业务实现注册到 gRPC Server。客户端侧生成的 `pb.NewSimpleBankClient(conn)` 则返回强类型 Client Stub，内部根据完整方法名（例如 `/pb.SimpleBank/LoginUser`）调用连接。

---

## 6. gRPC Client、Server 与调用生命周期

### 6.1 服务端启动

当前 `runGrpcServer` 的关键步骤：

```go
grpcLogger := grpc.UnaryInterceptor(gapi.GrpcLogger)
grpcServer := grpc.NewServer(grpcLogger)
pb.RegisterSimpleBankServer(grpcServer, server)
reflection.Register(grpcServer)
listener, _ := net.Listen("tcp", config.GRPCServerAddress)
grpcServer.Serve(listener)
```

可以按五层理解：

1. `net.Listen` 监听 TCP 地址；
2. `grpc.NewServer` 创建协议服务器；
3. Register 把“完整方法名”映射到 `gapi.Server`；
4. Interceptor 包裹一元调用；
5. `Serve` 接收连接并阻塞运行。

### 6.2 客户端连接不是“每次调用一条新 TCP”

Go gRPC 中的 `ClientConn` 更接近长期复用的逻辑 Channel：它管理连接状态、名称解析、负载均衡等。通常应用启动时创建并复用，不应每个请求都 Dial/Close。一次具体调用由生成 Client 方法执行，并传入 `context.Context`。

示意代码：

```go
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
defer cancel()

conn, err := grpc.NewClient(
    "localhost:9090",
    grpc.WithTransportCredentials(insecure.NewCredentials()), // 仅本地开发示意
)
if err != nil { /* handle */ }
defer conn.Close()

client := pb.NewSimpleBankClient(conn)
rsp, err := client.LoginUser(ctx, &pb.LoginUserRequest{
    Username: "learner01",
    Password: "example-password",
})
```

生产环境不要照搬 `insecure.NewCredentials()`；应使用 TLS，内部零信任或高安全场景可使用 mTLS，让客户端也以证书向服务端证明身份。

### 6.3 Deadline、Timeout 与 Cancellation

Deadline 是“最晚等到哪个时刻”，Timeout 是“最多等多久”，后者可转换为前者。gRPC 默认不会自动替客户端设置一个业务合理的 deadline；不设置就可能无限等待，所以客户端应显式设置。

当客户端超时，可能收到 `DeadlineExceeded`；当调用被主动取消，通常是 `Canceled`。但要特别小心：**客户端看到 DeadlineExceeded，不代表服务端一定没成功。**服务端可能已经提交数据库，只是响应返回太迟。这正是写操作重试必须考虑幂等性的原因。

Go 服务端把请求 Context 继续传给 `store` 是正确方向：数据库查询可感知取消。但业务若自行启动 goroutine、调用不接受 Context 的库，仍要自己检查 `ctx.Done()` 并停止工作。当前项目没有在 gRPC/HTTP 入口统一设置最大请求时间，也没有展示客户端 deadline；这是生产化缺口。

在多服务调用链中，剩余时间应向下游传播，不能每层重新给完整 5 秒，否则总耗时膨胀。还应留出返回与清理预算，例如上游剩 200ms 时不再发一个预计 500ms 的下游请求。

---

## 7. 错误模型：Code、Message 和 Details 各负责什么

### 7.1 不要把所有错误都变成 `Internal`

gRPC Status 由 code、文本 message 和可选 details 构成。常用含义：

| Code | 应用语义 |
|---|---|
| `InvalidArgument` | 参数本身非法，与系统当前状态无关 |
| `Unauthenticated` | 没有有效身份凭证 |
| `PermissionDenied` | 已识别身份，但无权操作 |
| `NotFound` | 目标资源不存在 |
| `AlreadyExists` | 创建的资源已存在 |
| `FailedPrecondition` | 当前系统状态不允许操作，先修复前置条件 |
| `Aborted` | 并发冲突/事务中止，常需在更高层重做流程 |
| `ResourceExhausted` | 限额或资源耗尽 |
| `Unavailable` | 暂时不可用，可能适合退避重试 |
| `Internal` | 内部不变量被破坏或未分类服务端故障 |
| `DeadlineExceeded` | 超过截止时间 |

当前项目已经有一些正确映射：创建用户唯一约束 → `AlreadyExists`；更新不存在用户 → `NotFound`；没有合法 Token → `Unauthenticated`；普通用户修改他人 → `PermissionDenied`；字段验证失败 → `InvalidArgument`。

但仍有不足：

- `VerifyEmailTx` 的所有失败都变成 `Internal`，无法区分验证码不存在、过期、已使用与数据库故障；
- `UpdateUser` 的内部错误文本仍写成 `failed to create user`，语义错误；
- 登录把用户不存在和密码错误都映射成 `NotFound`，且返回不同文本，既不理想表达认证失败，也可能辅助用户名枚举；生产系统常统一成 `Unauthenticated` 和模糊文案；
- 一些 `Internal` 响应直接拼接底层错误，可能泄露数据库细节；应在服务端日志保留原始 error，对客户端返回稳定、安全的错误；
- `main.go` 多个 fatal 日志没有 `.Err(err)`，真正原因没有写入日志。

### 7.2 结构化错误详情

`gapi/error.go` 使用 `google.rpc.BadRequest`：

```go
badRequest := &errdetails.BadRequest{FieldViolations: violations}
statusInvalid := status.New(codes.InvalidArgument, "invalid parameters")
statusDetails, err := statusInvalid.WithDetails(badRequest)
```

这样客户端不必解析字符串，就能知道 `username`、`password`、`email` 分别哪里错了。结构化 details 是工业界很重要的契约：可以进一步使用 `ErrorInfo`、`RetryInfo`、`PreconditionFailure` 等标准详情，但需要稳定定义 error reason、domain 和 Metadata，不能随意变化。

Gateway 会把 gRPC status 转换为 HTTP 错误响应；HTTP status 与 gRPC code 有默认映射。外部 API 若需要特殊错误格式，应通过 Gateway error handler 统一定制，并确保 gRPC 与 HTTP 两种入口的业务语义一致。

---

## 8. Metadata、Interceptor 与 Reflection

### 8.1 Metadata：RPC 的请求头与响应头

Metadata 是字符串键到一个或多个值的映射，常承载：

- `authorization: bearer ...`
- trace/request ID
- locale、客户端版本
- User-Agent
- 分布式追踪上下文

它适合调用级控制信息，不适合塞大型业务对象。二进制 Metadata 的 key 通常以 `-bin` 结尾。

当前 `gapi/authorization.go` 从 Incoming Context 读取 `authorization`，验证 Bearer Token；`gapi/metadata.go` 读取 `grpcgateway-user-agent`、`user-agent`、`x-forwarded-for`，并尝试从 `peer.FromContext` 读取对端地址。登录时这些值写入 Session。

安全上要注意：`X-Forwarded-For` 只能信任由受控反向代理写入/清洗的值，不能直接相信公网客户端。当前代码还会在存在 peer 时覆盖先前 ClientIP，并保存可能带端口的 `Addr.String()`；部署到代理后到底记录客户端还是代理地址，需要明确策略。

### 8.2 Interceptor：横切逻辑的调用包装器

Server Interceptor 类似 Gin Middleware，但作用于 gRPC 调用。适合实现：

- 认证/授权；
- 结构化日志；
- panic recovery；
- metrics 与 tracing；
- 限流；
- 审计；
- 一致的错误转换。

一元拦截器的核心形状是：在调用 `handler(ctx, req)` 前后做工作。当前 `gapi.GrpcLogger` 记录协议、完整方法名、status code、耗时，出错时附上 error。

当前仅通过 `grpc.UnaryInterceptor(gapi.GrpcLogger)` 安装一个一元拦截器。要安装多个，工业界使用 `grpc.ChainUnaryInterceptor(...)`，并认真安排顺序，例如 recovery 放在能捕获后续 panic 的外层，认证在业务前，tracing/metrics 覆盖完整耗时。流式 RPC 需要单独的 `ChainStreamInterceptor`。

再次强调：当前 HTTP Gateway 通过 `RegisterSimpleBankHandlerServer` 直接调用业务 Server，因此**不会经过 `grpc.NewServer` 上的 `GrpcLogger`**。HTTP 路径只经过 `gapi.HttpLogger` 和 Gateway 自身。若把认证完全搬入 gRPC interceptor，当前 Gateway 请求也会绕过它，除非同时给 Gateway 配 Middleware，或改成 FromEndpoint 让请求真正经过 gRPC Server。

当前 `HttpLogger` 还有生产风险：它把非 200 响应 Body 完整记录下来；错误体可能含敏感详情。它也只把 200 当成功，其他 2xx 会按 error 记录。工业实现应做敏感字段脱敏、Body 大小限制、按状态范围分级，记录 request/trace ID、客户端身份和路由模板而不是带敏感 query 的原始 URI。

### 8.3 Reflection：工具如何发现服务

`reflection.Register(grpcServer)` 允许 Evans、grpcurl 等工具在没有本地 `.proto` 的情况下查询服务、方法和消息描述。Makefile 的：

```bash
make evans
```

实际执行 `evans --host localhost --port 9090 -r repl`，其中 `-r` 使用 reflection。

Reflection 对开发调试很方便。生产环境是否开放要结合网络边界：它不会自动绕过认证调用业务，但会暴露 API Schema，通常只在内网、管理端口或受控环境开放。

---

## 9. grpc-gateway：把 HTTP/JSON 转成 Protobuf 方法调用

### 9.1 HTTP 注解决定路由

`proto/service_simple_bank.proto` 使用 Google API HTTP Annotation：

```proto
rpc UpdateUser(UpdateUserRequest) returns (UpdateUserResponse) {
    option (google.api.http) = {
        patch: "/v1/update_user"
        body: "*"
    };
}
```

`body: "*"` 表示请求消息字段从整个 JSON Body 解析。`VerifyEmail` 是 GET 且没写 body，因此 `email_id` 和 `secret_code` 从 Query String 填充：

```http
GET /v1/verify_email?email_id=123&secret_code=654321
```

把 Secret 放 Query String 容易进入浏览器历史、代理日志、监控和 Referer；生产验证链接常不得不用 URL token，但应使用高熵、单次、短期 token，并确保日志脱敏，避免六位可预测码与标识组合暴露。

### 9.2 当前是同进程直调模式

Gateway 启动时重新构造了一份 `gapi.Server`，然后注册：

```go
err = pb.RegisterSimpleBankHandlerServer(ctx, grpcMux, server)
```

生成代码的 local request 函数会调用：

```go
server.UpdateUser(ctx, &protoReq)
```

它的优点是少一次本机网络跳转，部署简单；缺点是绕过 gRPC transport 和 grpc.Server interceptor，Gateway 与 gRPC 入口可能出现行为差异，而且这种 local registration 不支持生成文件所说的 streaming path。

另一种拓扑是：

```text
HTTP Client → 独立 Gateway → TLS/mTLS gRPC → Backend
```

使用 `RegisterSimpleBankHandlerFromEndpoint` 或 Client 注册。优点是边界清晰、拦截器/负载均衡/服务发现路径一致、Gateway 可独立扩缩；代价是多一跳、连接与证书管理更复杂。没有“永远最佳”的拓扑：单体或小服务可用同进程，复杂平台常拆分边缘 Gateway。

### 9.3 `protojson` 不是普通 `encoding/json`

Protobuf JSON 有自己的标准映射，例如 64 位整数、枚举、Timestamp、字段名和未知字段都有规定，因此应使用 `protojson`。当前配置：

```go
MarshalOptions: protojson.MarshalOptions{
    UseProtoNames: true,
},
UnmarshalOptions: protojson.UnmarshalOptions{
    DiscardUnknown: true,
},
```

`UseProtoNames: true` 让输出使用 Proto 源字段名，如 `full_name`，而不是默认的 lowerCamelCase `fullName`。

`DiscardUnknown: true` 表示 HTTP JSON 中服务端不认识的字段直接忽略。这有两面性：

- 兼容收益：新版客户端提前发送新增字段给旧版服务端时，旧服务端不会因为未知字段整体拒绝；滚动发布更宽容；
- 风险：用户把 `username` 误拼成 `usernmae` 时不会得到“未知字段”错误，而会像没传一样进入默认值与业务校验；若误拼字段是 optional，甚至可能无声地“不更新”。

因此内部严格 API 常选择拒绝未知 JSON 字段，尽早发现 SDK/调用错误；公网兼容 API 可能选择丢弃，但必须配合版本治理、契约测试、监控。不要把它与 Protobuf 二进制的未知字段兼容机制混为一谈。

---

## 10. OpenAPI、Swagger UI 与 Statik

OpenAPI 是描述 HTTP API 的标准格式；Swagger 是这套生态的历史名称，Swagger UI 是读取 OpenAPI 文档并生成交互页面的前端工具。当前插件是 `protoc-gen-openapiv2`，生成的是 OpenAPI v2 JSON，不是 OpenAPI v3。

`service_simple_bank.proto` 中的文件级 option 设置标题、版本、联系人；每个 RPC 的 operation option 设置 summary/description。当前 UpdateUser 的 description 仍误写为 `create a new user`，说明生成文档不会自动理解业务，注解质量仍需人工 Review。

提交 `99de024` 最初生成并从磁盘目录提供 Swagger；`4b8fccb` 引入 Statik。当前 `doc/swagger` 不只有 JSON，还包含一整套 Swagger UI 静态文件。`statik -src=./doc/swagger -dest=./doc` 把它们打包进生成的 `doc/statik/statik.go`。`main.go` 通过空白导入注册嵌入文件系统：

```go
_ "github.com/lllmml/simplebank/doc/statik"
```

再通过：

```go
statikFS, _ := fs.New()
mux.Handle("/swagger/", http.StripPrefix("/swagger/", http.FileServer(statikFS)))
```

访问开发配置下的 `http://localhost:8081/swagger/` 可打开 UI；JSON 位于 `/swagger/simple_bank.swagger.json`。

Statik 解决“单个 Go 二进制不依赖运行目录里的前端文件”。代价是每次 Swagger/UI 改动都要重新运行生成命令，否则嵌入内容过期；`doc/statik/statik.go` 体积大、diff 不适合人工 Review。现代 Go 也内置 `//go:embed`，新项目常直接使用标准库 `embed`，但当前仓库真实使用的是 Statik。

工业界还会：

- 在 CI 校验 Proto、Gateway 路由和 OpenAPI 同步；
- 对外文档去除内部字段和敏感示例；
- 给文档端点设置访问控制或仅在非生产开放；
- 做 SDK 生成和契约测试；
- 明确 API version 与服务发布版本不是同一个概念。

---

## 11. 回到项目：四个 RPC 的真实调用链

### 11.1 CreateUser

```text
CreateUserRequest
→ validateCreateUserRequest
→ bcrypt HashPassword
→ store.CreateUserTx
→ 事务回调投递验证邮件任务
→ convertUser
→ CreateUserResponse
```

验证错误带 `BadRequest.FieldViolation`；数据库 unique violation 映射 `AlreadyExists`。这里涉及的数据库与异步一致性问题在其他阶段详讲。

### 11.2 LoginUser

```text
LoginUserRequest
→ 参数校验
→ store.GetUser
→ CheckPassword
→ 创建 access/refresh token
→ extractMetadata
→ store.CreateSession
→ LoginUserResponse
```

Response 含 `google.protobuf.Timestamp`。`gapi/converter.go` 和登录 RPC 使用 `timestamppb.New` 将 `time.Time` 转成 Protobuf Timestamp。生产代码还应在边界校验 Timestamp 是否有效。

### 11.3 UpdateUser

```text
Metadata Authorization
→ authorizeUser（banker/depositor）
→ 字段校验
→ banker 可改他人，depositor 只能改自己
→ optional presence 转 pgtype.Valid
→ store.UpdateUser
→ UpdateUserResponse
```

这是当前唯一明确需要认证的 RPC，也是理解 Metadata、Status、RBAC 和 optional 的最佳入口。

### 11.4 VerifyEmail

```text
GET Query / 原生 gRPC Request
→ 参数校验
→ store.VerifyEmailTx
→ 返回 is_verified
```

当前把全部事务错误返回 `Internal`，错误语义仍需细分。

### 11.5 两份 `gapi.Server`

`runGrpcServer` 和 `runGatewayServer` 各调用一次 `gapi.NewServer`。它们共享同一个 `store` 和 `taskDistributor` 接口实例，但分别创建自己的 `tokenMaker`；由于配置密钥相同，Token 可互相验证。这个设计目前能工作，但共享依赖、启动/关闭和拦截器行为更容易出现漂移。工业代码通常构造一份业务服务，再把不同 transport adapter 接到同一应用层，或清楚划分 Handler 与 Use Case 层。

---

## 12. 安全地调用当前服务

先准备 PostgreSQL、Redis、迁移和不含真实秘密的本地环境配置，然后运行：

```bash
make server
```

不要把真实 Token、密码、邮箱应用密码粘贴进文档、Git、命令历史或共享日志。下面只用示例值。

### 12.1 HTTP 创建用户

```bash
curl -i -X POST http://localhost:8081/v1/create_user \
  -H 'Content-Type: application/json' \
  -d '{
    "username": "learner01",
    "full_name": "Backend Learner",
    "email": "learner01@example.com",
    "password": "example-password"
  }'
```

### 12.2 HTTP 登录

```bash
curl -i -X POST http://localhost:8081/v1/login_user \
  -H 'Content-Type: application/json' \
  -d '{"username":"learner01","password":"example-password"}'
```

返回值包含访问令牌和刷新令牌，真实环境不要打印到 CI 日志。后续只在当前 shell 临时保存访问令牌，并避免提交：

```bash
curl -i -X PATCH http://localhost:8081/v1/update_user \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <ACCESS_TOKEN>' \
  -d '{"username":"learner01","full_name":"New Name"}'
```

这里没传 email/password，它们应保持不变。若传 `"email":""`，presence 为 true，随后应被参数校验拒绝。

### 12.3 使用 Evans 调原生 gRPC

服务启用 Reflection 后：

```bash
make evans
```

在 Evans REPL 中选择 `pb.SimpleBank` 服务，再调用 `LoginUser`。具体交互命令随 Evans 版本略有差异，因此应以本机 `help` 为准。也可以用 grpcurl：

```bash
grpcurl -plaintext localhost:9090 list
grpcurl -plaintext localhost:9090 describe pb.SimpleBank
```

`-plaintext` 只符合当前本地明文服务；生产应使用 TLS 并验证服务证书。

---

## 13. Git 历史告诉我们的设计演进

| 提交 | 实际变化 | 应理解的知识 |
|---|---|---|
| `2d4bda2` | 首次加入 Proto、生成代码、gRPC Server、Reflection、Evans 命令 | IDL、Server 注册、代码生成、服务发现 |
| `31d9d5e` | 实现 CreateUser/LoginUser 与 DB→Proto converter | Transport Handler、状态码、模型转换 |
| `4e07a0d` | 加 grpc-gateway、HTTP 注解、protojson 配置 | 同一契约暴露 HTTP/JSON；进程内直调 |
| `5549b0a` | 提取 User-Agent/IP Metadata 写入 Session | Metadata、peer、代理头 |
| `f21d275` | 修正 refresh token 错误格式调用 | 错误 API 的正确使用 |
| `99de024` | 生成 OpenAPI v2，并从目录提供文档 | 契约文档、Swagger |
| `4b8fccb` | 使用 Statik 嵌入 Swagger 静态资源 | 单二进制资源打包 |
| `7b2a91e` | 参数校验与 `BadRequest` details | 结构化错误 |
| `1171bd0` | SQL `sqlc.narg` + `COALESCE` 部分更新 | 数据库 NULL 与 Patch 语义 |
| `b346234` | UpdateUser RPC，Proto `optional` | 字段 presence 贯穿到 SQL |
| `7e29361` | UpdateUser 加认证和所有权授权 | Metadata、Unauthenticated 与 PermissionDenied |
| `ce7994a` | Zerolog 与 gRPC Unary Logger Interceptor | 横切逻辑、结构化日志 |
| `b48ade6` | Gateway 外层加 HTTP Logger | 两种 transport 分别观测 |
| `e8fa39e` | 新增 VerifyEmail RPC 与 Gateway 路由 | 契约继续演进 |

这段历史展示了一个健康的学习顺序：先定义和跑通最小 RPC，再加 Gateway、Metadata、文档、校验、部分更新、认证与可观测性。但生产开发最好在设计初期就确定错误规范、Deadline、TLS、兼容性和观测标准，而不是上线后才补。

---

## 14. 当前实现距离工业生产还差什么

### 14.1 契约与版本治理

- 建立 Proto lint 和 breaking-change CI；删除字段必须 `reserved`；
- 设计资源化路径、分页、FieldMask、幂等键和统一错误详情；
- OpenAPI 文案 Review，修复 UpdateUser 错误 description；
- 明确哪些 RPC 对外、哪些仅内部，避免把数据库模型直接当公开 API；
- 不要在响应中意外扩散密码哈希、内部角色或隐私字段；当前 converter 没返回哈希，这是正确的边界意识。

### 14.2 Deadline、重试与幂等

客户端必须设置现实 Deadline；服务端和下游数据库/Redis/SMTP 调用必须尊重 Context。重试只针对明确的暂时性错误，并使用指数退避、抖动和次数上限。

不要仅看到 `Unavailable` 就无脑重试写操作。CreateUser 可能已经成功但响应丢失；重复调用可能得到 AlreadyExists，或重复发任务。生产中用请求 ID/Idempotency-Key、唯一约束、幂等结果表或业务幂等语义确保重试安全。读取 RPC 通常更容易重试，但也要防止重试风暴。

### 14.3 TLS、mTLS 与边界

当前 gRPC `grpc.NewServer` 未配置 credentials，HTTP 也是 `http.Serve` 明文。生产流量应至少由可信入口终止 TLS；服务间高安全场景启用 mTLS、证书轮换和服务身份授权。不要把“在 Kubernetes 内网”当作天然安全。

### 14.4 健康检查与优雅关闭

项目启用了 Reflection，却没有注册标准 gRPC Health Service；也没有 readiness/liveness HTTP 端点。健康检查应区分：进程活着、能接流量、关键依赖可用。

当前没有 SIGTERM 流程。工业服务应停止接新流量，调用 `grpcServer.GracefulStop()`（并设置超时后强制 Stop）、`http.Server.Shutdown(ctx)`，再关闭 Worker、Redis 和 pgxpool，给在途请求留完成时间。

### 14.5 拦截器链与一致的两入口语义

应设计统一链：recovery、request ID、认证、授权、日志、metrics、tracing、限流，并清楚哪些放在 transport 层，哪些必须在应用层再次保证。当前 local Gateway 绕过 gRPC interceptor，最危险的不是少一条日志，而是未来若认证只放 interceptor，HTTP 入口可能失去保护。

可选方案：

1. 保留同进程直调，但为 Gateway 安装等价 Middleware，并把关键授权放业务层；
2. Gateway 使用生成 Client 连接真正的 gRPC Server，让 gRPC interceptor 成为统一入口；
3. 提取 application service，在两个 adapter 之前共享统一的认证授权/业务策略，transport 只做协议转换。

### 14.6 可观测性

日志只是起点。生产系统通常需要：

- Prometheus/OpenTelemetry 指标：QPS、延迟直方图、按 code 的错误率、在途请求；
- 分布式 Trace：Gateway→gRPC→DB/Redis 的 span 与 context propagation；
- 结构化日志：trace ID、request ID、方法、主体、状态、耗时；
- SLO 和告警：例如成功率和 P99，而不是“日志里有 error”；
- 敏感信息治理：不记 Authorization、Token、密码、完整错误 Body 和验证 Secret。

注意指标 label 的基数，不能把 username、原始 URL、error message 当无限变化的 label，否则监控系统本身会被拖垮。

### 14.7 容量与协议限制

还要设置最大消息大小、Header/Metadata 上限、并发与速率限制、Keepalive 策略，防止超大请求和连接滥用。流式 RPC 要处理背压、半关闭、客户端断连和 goroutine 泄漏。反向代理/负载均衡器必须真正支持 HTTP/2 gRPC，而不是只支持普通 HTTP/1.1。

---

## 15. 循序渐进的练习

### 练习 1：验证 presence

给 `UpdateUserRequest` 分别发送：

1. 只传 username；
2. 传 username 和 `full_name: ""`；
3. 传 username 和合法 full_name。

在测试中断言：第一种 `req.FullName == nil`；后两种不为 nil；第二种随后被校验拒绝。不要通过打印真实用户 Token 做测试。

### 练习 2：新增只读 RPC

新增 `GetUserProfile`：

1. 创建 request/response Proto；
2. 分配从未用过的字段号；
3. 在 service 中加 RPC 和 GET 映射；
4. `make proto`；
5. 在 `gapi/` 实现；
6. 为 NotFound、Unauthenticated、成功写测试；
7. 检查 Swagger UI。

限制：不要返回 password hash、Token 或内部 Session。

### 练习 3：给客户端加 Deadline

写一个小 Go Client，用 1 纳秒和 2 秒两个 timeout 调用同一只读 RPC，观察 code。再思考为什么不能根据 DeadlineExceeded 推断写操作一定没执行。

### 练习 4：修复错误语义

为 VerifyEmail 区分：参数非法、验证码不存在、已使用、过期、数据库不可用。设计 code 和 details，写表驱动测试。客户端不应看到 SQL 和表名。

### 练习 5：验证 Gateway 绕过拦截器

在测试环境给 `GrpcLogger` 或一个计数 interceptor 加可观察计数，分别调用 9090 与 8081。解释为什么只有原生 gRPC 进入 Unary Interceptor。然后做两个版本：Gateway local Middleware 与 FromEndpoint，比较调用链。

### 练习 6：契约破坏实验

不要在主分支做。临时把一个已发布字段从 `= 2` 改成 `= 9`，用旧代码序列化、新代码反序列化，观察数据含义。恢复后删除字段并使用 `reserved 2; reserved "old_name";`，运行 protoc 看编译器如何阻止复用。

### 练习 7：生产化最小闭环

为 gRPC 加标准健康检查、服务端 TLS、Unary interceptor chain、OpenTelemetry 指标与 graceful shutdown；为 HTTP Server 配置 `ReadHeaderTimeout`、`ReadTimeout`、`WriteTimeout`、`IdleTimeout`。用集成测试验证 SIGTERM 时在途短请求能结束，新请求被拒绝。

---

## 16. 自测题与答案

### 题 1：Protobuf 字段名和字段编号，哪个更不能修改？

答：在线二进制兼容上字段编号是身份，绝不能随意修改或复用。字段名也影响 ProtoJSON、TextFormat、生成 API 和调用方源码，因此也不能草率修改；删除时最好同时 reserved 编号与名称。

### 题 2：普通 `string email = 3` 为什么不适合 PATCH？

答：隐式 presence 下，未传与明确传空字符串都读取为 `""`，无法表达“保持不变”和“设置成默认值”的差别。`optional` 或 FieldMask 可以显式表达更新意图。

### 题 3：`req.GetEmail() == ""` 能证明请求传了 email 吗？

答：不能。应检查 `req.Email != nil`。Getter 在字段未提供时也返回默认空字符串。

### 题 4：本项目 HTTP 请求是否会经过 `gapi.GrpcLogger`？

答：不会。Gateway 使用 `RegisterSimpleBankHandlerServer` 在进程内直调 `gapi.Server`，绕过 `grpc.Server` 的 Unary Interceptor；HTTP 请求经过的是 `gapi.HttpLogger`。

### 题 5：为什么仍然能在 HTTP UpdateUser 中读取 Authorization Metadata？

答：Gateway 生成 Handler 会从 HTTP 请求构造/注解 gRPC 风格 Context 和 Metadata，再直调业务方法。能读 Metadata 不代表它经过了真正的 gRPC transport 或 Server Interceptor。

### 题 6：`DiscardUnknown: true` 一定更兼容、更好吗？

答：它提高新客户端对旧服务端发送新增 JSON 字段时的宽容度，但会吞掉字段误拼，可能让更新静默失效。应根据 API 兼容策略选择，并用契约测试和监控弥补风险。

### 题 7：看到 `DeadlineExceeded` 后，CreateUser 可以直接重试吗？

答：不一定。服务端可能已提交但响应迟到。只有写操作具备幂等语义、幂等键或可安全识别重复结果时，才能按明确策略重试。

### 题 8：OpenAPI 和 Protobuf 哪个是当前项目的源契约？

答：当前流程以 `.proto` 和其中的 HTTP/OpenAPI annotation 为源，再生成 Gateway 与 OpenAPI v2 JSON。手改 `simple_bank.swagger.json` 会在下次 `make proto` 被覆盖。

### 题 9：当前有账户和转账 gRPC 吗？

答：没有。Proto 服务只有 CreateUser、UpdateUser、LoginUser、VerifyEmail。账户和转账 Handler 在未启动的 Gin 路径中。

### 题 10：Reflection 是健康检查吗？

答：不是。Reflection 让工具查询 Schema；Health Service 告诉客户端/负载均衡器服务是否可用，职责不同。当前只启用了前者。

---

## 17. 掌握清单

如果你能不看答案解释下面每一项，就算真正掌握本阶段：

- [ ] 能区分 RPC、gRPC、REST、HTTP/2、Protobuf 和 JSON；
- [ ] 能解释 HTTP/2 Stream、多路复用、Header 压缩和流式基础；
- [ ] 能写 message、service、import、go_package 和 HTTP annotation；
- [ ] 知道字段编号不可重排，删除字段要 reserved；
- [ ] 能解释 Proto3 默认值、隐式/显式 presence、optional 与 FieldMask；
- [ ] 能说出当前 Makefile 中五类生成步骤及各自产物；
- [ ] 不手改 `pb/*.go`、Swagger JSON 和 Statik 生成代码；
- [ ] 能读懂生成的 Client/Server 接口、Register 函数和完整方法名；
- [ ] 能正确选择 InvalidArgument、Unauthenticated、PermissionDenied、NotFound、AlreadyExists、Unavailable、Internal；
- [ ] 能用 `status.WithDetails` 返回结构化字段错误；
- [ ] 理解 Metadata、peer 与受信代理头的安全边界；
- [ ] 能解释 Unary/Stream Interceptor 以及 chain 顺序；
- [ ] 知道 Reflection 便于 Evans/grpcurl，但不等于健康检查；
- [ ] 会为 Client 设置 Deadline，并理解取消传播；
- [ ] 知道重试必须与错误分类、退避、限次和幂等一起设计；
- [ ] 能画出本项目 9090 和 8081 两条不同调用链；
- [ ] 能明确说明 Gateway 进程内直调，不会拨号 9090；
- [ ] 能解释 `optional → *string → pgtype.Valid → sqlc.narg → COALESCE`；
- [ ] 理解 `UseProtoNames` 与 `DiscardUnknown` 的收益和风险；
- [ ] 知道当前生成 OpenAPI v2，并用 Statik 嵌入 Swagger UI；
- [ ] 知道生产还需要 TLS/mTLS、健康检查、优雅关闭、观测、限流和兼容性 CI。

---

## 18. 事实来源与进一步阅读

项目事实来自当前仓库的 `proto/`、`pb/`、`gapi/`、`main.go`、`Makefile`、`db/query/user.sql`、`db/sqlc/user.sql.go`，以及本文表格列出的 Git 提交。进一步核对协议原理时，优先阅读官方资料：

- [gRPC Introduction](https://grpc.io/docs/what-is-grpc/introduction/) 与 [Core concepts](https://grpc.io/docs/what-is-grpc/core-concepts/)
- [Protocol Buffers Proto3 Language Guide](https://protobuf.dev/programming-guides/proto3/)
- [Protocol Buffers Field Presence](https://protobuf.dev/programming-guides/field_presence/)
- [gRPC Status Codes](https://grpc.io/docs/guides/status-codes/)
- [gRPC Deadlines](https://grpc.io/docs/guides/deadlines/)
- [gRPC Retry](https://grpc.io/docs/guides/retry/)
- [gRPC Metadata](https://grpc.io/docs/guides/metadata/)
- [gRPC Interceptors](https://grpc.io/docs/guides/interceptors/)
- [gRPC Health Checking](https://grpc.io/docs/guides/health-checking/)
- [gRPC-Gateway Documentation](https://grpc-ecosystem.github.io/grpc-gateway/docs/)

最后用一句话收束本阶段：**Proto 是长期契约，生成代码是机械适配，gRPC/Gateway 是传输入口，真正可靠的后端还必须把兼容性、错误语义、Deadline、幂等、安全与可观测性一起设计。**
