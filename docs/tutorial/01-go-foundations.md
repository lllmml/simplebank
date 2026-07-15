# 第一阶段：用 SimpleBank 系统掌握 Go 后端基础

> 本文边界：这一阶段只讲“读懂并能写出 SimpleBank 所使用的 Go 语言基础”。数据库事务、HTTP/gRPC 协议、令牌安全、Redis 异步任务会在后续阶段深入；本文只在它们能帮助理解 Go 语法和程序结构时引用。文中所有“本项目如何做”的判断都基于当前仓库代码，而不是根据课程标题推测。
>
> 前置知识：会使用终端，知道文件和目录是什么，能看懂最基本的“输入—处理—输出”。不要求有 Go 或后端经验。

## 1. 先建立一张地图：Go 后端程序到底是什么

一个 Go 后端程序并不神秘：它是若干 package（包）组成的可执行程序。程序启动后监听网络端口，接收请求，把请求转换成函数调用，再调用数据库、缓存或邮件服务，最后返回结果。

在当前 SimpleBank 中，启动路径从 `main.go` 的 `main()` 开始：

```text
main()
  ├── util.LoadConfig：读配置
  ├── pgxpool.New：创建数据库连接池
  ├── runDBMigration：执行数据库迁移
  ├── db.NewStore：构造数据访问对象
  ├── worker.NewRedisTaskDistributor：构造 Redis 任务生产者
  ├── go runTaskProcessor：并发启动任务消费者
  ├── go runGatewayServer：并发启动 HTTP Gateway
  └── runGrpcServer：在主 goroutine 中运行 gRPC Server
```

这张图已经包含本阶段的大多数语言知识：包和导入、变量、多返回值、结构体、接口、指针、错误、context、goroutine 和函数参数。后文会逐项拆开。

学习时先记住三个层次：

1. **语法层**：`:=`、`struct`、`interface`、`go` 等关键字是什么意思。
2. **设计层**：为什么把数据库抽象成接口，为什么传 `context.Context`。
3. **工程层**：怎样测试、生成代码、格式化和避免泄漏 goroutine。

只背语法无法写好后端；只谈架构而不会语法也无法落地。本文会一直在三个层次之间往返。

---

## 2. Module、package 与 import

### 2.1 Module：一个可版本化的 Go 工程

Go module 是依赖管理和导入路径的基本单元，由根目录的 `go.mod` 声明。当前项目第一行是：

```go
module github.com/lllmml/simplebank
```

因此仓库内 `util` 目录的完整导入路径是：

```go
import "github.com/lllmml/simplebank/util"
```

`go.mod` 还记录 Go 语言版本和直接、间接依赖。`require` 中没有 `// indirect` 的通常是项目直接导入的模块；带 `// indirect` 的通常是依赖的依赖。不要手工复制第三方源码到项目里。日常使用 `go mod tidy` 让模块文件与实际导入保持一致，但执行前应查看 diff，因为升级或清理依赖也属于代码变更。

当前 `go.mod` 声明 `go 1.26.4`。这是本仓库的事实；学习或运行时应以团队实际安装的工具链和 CI 配置为准，不要仅凭教程假定机器上的版本。

### 2.2 Package：代码的组织和封装边界

每个 `.go` 文件开头都有 `package`：

```go
package util
```

同一目录中的普通 Go 文件通常属于同一个包。包内标识符可以直接互相使用；包外只能访问首字母大写的导出标识符。例如：

- `util.HashPassword` 首字母大写，对其他包可见；
- `api.validCurrency` 首字母小写，只在 `api` 包内可见；
- `main` 包加上 `func main()` 构成可执行程序入口。

这里的“大写即 public”只是便于入门的说法。更准确地说，Go 以标识符首字母大小写决定是否从包中导出；导出不等于适合让所有业务随意调用，团队仍需要 API 设计和文档。

当前项目按职责分包：`util` 放通用小工具，`token` 负责令牌，`db/sqlc` 负责数据访问，`api` 是 Gin HTTP 层，`gapi` 是 gRPC 层，`worker` 是异步任务。包应高内聚：同一职责的代码放在一起，而不是建立一个不断膨胀、什么都放的 `utils` 包。

### 2.3 import 与别名、空白导入

普通导入：

```go
import "context"
import "github.com/lllmml/simplebank/util"
```

调用时使用包名：`context.Background()`、`util.LoadConfig()`。

当前代码常给数据库包起别名：

```go
db "github.com/lllmml/simplebank/db/sqlc"
```

因为目录名是 `sqlc`，但该目录文件声明的是 `package db`；显式写 `db` 能让读者立刻知道后续的 `db.Store` 指什么。别名应该提高可读性，不应用来隐藏名称冲突或制造缩写谜语。

`main.go` 还有空白导入：

```go
_ "github.com/golang-migrate/migrate/v4/database/postgres"
_ "github.com/golang-migrate/migrate/v4/source/file"
```

空白导入表示不直接引用包的导出名字，但仍执行该包的初始化逻辑。这里是让 migrate 注册 PostgreSQL 数据库驱动和文件来源驱动。空白导入依赖副作用，不够直观；工业项目应加注释说明目的，并定期清理无用驱动。当前 `main.go` 同时空白导入 `pgx/v5` 和 `lib/pq`，而运行时连接池使用 `pgxpool`，是否都还必要值得维护者核查，不能仅因“能编译”就长期保留。

### 2.4 工业界习惯与常见坑

- 包名短、明确、用小写，不使用 `common`、`base` 这类含义模糊的垃圾桶包。
- 避免 import cycle。A 导入 B、B 又导入 A，Go 编译器会拒绝；通常应把共同抽象移动到更稳定的下层，或让接口由使用方定义。
- 不要为了“分层”给每个 struct 建一个包。包太碎会让调用路径和依赖关系变复杂。
- 依赖版本变更要通过测试和代码评审，不应随意执行全量升级后直接提交。

---

## 3. 变量、常量、零值与类型

### 3.1 声明和类型推断

Go 是静态类型语言。变量的类型在编译期确定：

```go
var result TransferTxResult
amount := int64(10)
```

`var result TransferTxResult` 创建变量并赋该类型的零值；`:=` 在函数内部声明并由右侧推断类型。`:=` 不是普通赋值：左侧至少要有一个当前作用域中的新变量。

例如 `main.go`：

```go
config, err := util.LoadConfig(".")
if err != nil {
    // 处理错误
}
```

这里一次声明 `config` 和 `err`。后面可以写：

```go
connPool, err := pgxpool.New(context.Background(), config.DBSource)
```

`connPool` 是新变量，`err` 是同一作用域里已有变量，所以这个短声明合法。新手常误以为它重新创建了一个完全无关的 `err`。要注意代码块会产生新作用域；在内层 `if` 中使用 `:=` 可能 shadow（遮蔽）外层变量。

### 3.2 常用基本类型

本项目可见：

- `string`：用户名、币种、邮箱；
- `bool`：邮箱是否验证、session 是否 blocked；
- `int64`：账户 ID、余额、转账金额；
- `int32`：分页参数；
- `time.Duration`：令牌有效期；本质是以纳秒表示的整数类型，但应使用 `time.Minute` 等单位组合；
- `time.Time`：创建、签发、过期时间；
- `[]byte`：密钥、JSON 任务 payload 等字节数据。

金融金额不能随意使用 `float32/float64`，因为二进制浮点不能精确表示许多十进制小数。SimpleBank 数据模型用 `int64` 表示 balance 和 amount，这是避免浮点舍入误差的正确方向；但当前代码没有在类型名或文档中声明它代表元、分还是其他最小单位。工业系统通常明确规定最小货币单位，或者使用经过审查的 decimal/money 类型，并把币种、舍入规则、上下限写入领域模型和数据库约束。

### 3.3 零值

Go 为未显式初始化的值提供零值：数字为 `0`，bool 为 `false`，string 为 `""`，指针、slice、map、channel、函数和 interface 的零值为 `nil`，struct 的零值是各字段零值组合。

零值非常重要。例如 `TransferTx` 先写：

```go
var result TransferTxResult
```

事务每成功一步就填入一个字段；若失败，返回的 result 可能只被部分填充。因此调用方在 `err != nil` 时不应使用这个 result 做业务判断。

并非所有类型的零值都“可直接使用”：nil slice 可以读取、遍历和 `append`，但 nil map 不能写入；nil channel 上的发送和接收会永久阻塞；nil 指针解引用会 panic。

### 3.4 常量与枚举式设计

`util/currency.go`：

```go
const (
    USD = "USD"
    EUR = "EUR"
    CAD = "CAD"
)
```

`const` 值在编译期确定，不能被重新赋值。Go 没有传统 Java 风格的 enum；常见做法是定义一个具名类型再声明常量，例如 `type Currency string`，这样比到处传裸 `string` 更能表达领域含义。当前 SimpleBank 使用裸字符串，简单直接，但编译器无法阻止把任意字符串传入需要币种的函数，校验只能依赖 `IsSupportedCurrency` 和上层 validator。

### 3.5 工业界常见坑

- 不要用 `interface{}`/`any` 逃避建模；它把错误从编译期推迟到运行期。
- 区分“缺省值”和“业务上真的为零”。更新接口中，空字符串可能表示没传，也可能表示用户明确传空；当前 gRPC 更新代码通过 Proto optional 生成的指针区分两者。
- 不要在多个单位间传递裸数字，例如秒、毫秒、纳秒都用 `int64`。优先使用 `time.Duration` 或带领域意义的类型。
- 使用 `int` 还是 `int64` 要有理由。数据库 `bigint` 对应 `int64` 更清楚；slice 下标和 `len` 自然使用 `int`。

---

## 4. 函数、多返回值和命名返回值

### 4.1 函数是有类型的值

`util/password.go` 中：

```go
func HashPassword(password string) (string, error) {
    hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
    if err != nil {
        return "", err
    }
    return string(hashedPassword), nil
}
```

参数在前，返回类型在后。Go 通常用最后一个返回值 `error` 表示失败，而不是依靠异常。成功时 error 为 nil；失败时其他返回值通常是零值。

多返回值让函数自然表达“结果 + 错误”：

```go
config, err := util.LoadConfig(".")
```

不能忽略关键错误。确实不需要某个返回值时用 `_`，但 `_` 应表达“经过判断后明确不需要”，不是掩盖问题。

### 4.2 命名返回值与裸 return

`db/sqlc/tx_transfer.go` 的 `addMoney`：

```go
func addMoney(...) (account1 Account, account2 Account, err error) {
    account1, err = q.AddAccountBalance(...)
    if err != nil {
        return
    }
    account2, err = q.AddAccountBalance(...)
    return
}
```

返回值被命名后就是函数内变量，裸 `return` 会返回它们的当前值。短函数里尚可读；函数一长，读者很难知道裸 return 到底返回什么。工业界常倾向显式 `return account1, account2, err`，尤其在复杂流程中。

### 4.3 函数作为参数与回调

函数也有类型。事务辅助器：

```go
func (store *SQLStore) execTx(
    ctx context.Context,
    fn func(*Queries) error,
) error
```

`fn func(*Queries) error` 表示传入一个函数：接收 `*Queries`，返回 error。`TransferTx` 将业务步骤作为匿名函数传给它；`execTx` 统一处理 begin、rollback 和 commit。这是把“变化的业务步骤”注入“固定的事务骨架”。

### 4.4 工业界做法

- 函数只做一件可清晰命名的事，但不要把每两行代码都拆成函数。
- 参数过多且总是一起出现时，用参数 struct；SimpleBank 的 `TransferTxParams`、`CreateUserParams` 就是这种做法。
- 返回错误时提供上下文，但保留错误链；后文会讲 `%w`。
- API 边界优先接受抽象、返回具体值是一条常见经验，但不是机械定律。接口应由真正的替换需求驱动。

---

## 5. struct、字段标签与组合

### 5.1 struct 是一组有名字的字段

令牌 payload：

```go
type Payload struct {
    ID        uuid.UUID `json:"id"`
    Username  string    `json:"username"`
    Role      string    `json:"role"`
    IssuedAt  time.Time `json:"issued_at"`
    ExpiredAt time.Time `json:"expired_at"`
}
```

struct 用于表达一条完整数据。构造值时推荐带字段名：

```go
payload := &Payload{
    ID:        tokenID,
    Username:  username,
    Role:      role,
    IssuedAt:  time.Now(),
    ExpiredAt: time.Now().Add(duration),
}
```

带字段名不依赖声明顺序，新增字段时更安全。跨包构造 struct 时只能设置导出字段。

### 5.2 Tag 不是注释

反引号中的 struct tag 是可由反射读取的元数据：

```go
type createAccountRequest struct {
    Currency string `json:"currency" binding:"required,currency"`
}
```

- `json:"currency"` 告诉 JSON 编解码器字段名；
- `binding:"required,currency"` 被 Gin validator 读取；
- `util.Config` 使用 `mapstructure:"DB_SOURCE"` 让 Viper 将配置键映射到字段。

编译器一般不会替你检查第三方 tag 的业务语义。拼错 `binding` 可能导致校验静默失效，因此需要测试。不要把秘密字段无意标为可 JSON 序列化；数据库 `User` 含 `HashedPassword`，API 返回前当前项目用 `gapi/converter.go` 转成 protobuf User，避免把密码哈希直接暴露出去，这是正确的边界意识。

### 5.3 匿名字段和嵌入

`CreateUserTxParams`：

```go
type CreateUserTxParams struct {
    CreateUserParams
    AfterCreate func(user User) error
}
```

`CreateUserParams` 是匿名嵌入字段，因此可将其字段提升到外层访问。它表达“创建用户事务参数包含普通创建用户参数，再加一个回调”。

`gapi.Server` 还嵌入：

```go
pb.UnimplementedSimpleBankServer
```

这是 protobuf/gRPC 生成的默认实现，用于满足和向前兼容服务接口要求。

嵌入是组合和方法提升，不是传统面向对象继承。外层值“拥有一个”被嵌入值，并不自动建立父子类型替换关系。过度嵌入会让字段和方法来自哪里变得模糊；仅在语义上确实是组合、且提升行为有价值时使用。

---

## 6. 指针、方法和接收者

### 6.1 指针到底是什么

`*T` 是指向 T 值的指针类型，`&value` 取得地址，`*ptr` 解引用。指针让函数访问同一个对象，也能用 nil 表达“不存在”。它不等于 C/C++ 中可以任意做地址运算的裸指针，也不代表你无需思考并发访问。

```go
func (payload *Payload) Valid() error
```

这是方法：`Valid` 的接收者是 `*Payload`。调用 `payload.Valid()` 时，方法可以读取或修改同一个 payload。

### 6.2 值接收者还是指针接收者

值接收者会复制接收者值；指针接收者操作原值，并避免复制大 struct。常见选择：

- 方法需要修改接收者：用指针；
- struct 较大或包含不应复制的同步对象：用指针；
- 同一类型的方法接收者尽量一致；
- 小型、不可变语义的值类型可使用值接收者。

当前服务方法都以 `*Server` 为接收者：

```go
func (server *Server) CreateUser(...)
```

Server 持有配置、Store、TokenMaker 和任务分发器，应该共享同一个服务实例，而不是每次调用复制它。

### 6.3 方法集与接口的关键细节

如果一个类型只用指针接收者实现接口方法，通常是 `*T` 实现接口，不是 T。SimpleBank 构造器返回指针：

```go
return &RedisTaskDistributor{client: client}
```

它的方法接收者是 `*RedisTaskDistributor`，因此该指针满足 `TaskDistributor`。

### 6.4 nil 与指针的坑

- 对 nil 指针解引用会 panic。
- interface 中装入一个“类型为 `*T`、值为 nil”的指针后，interface 本身不等于 nil，因为它仍携带动态类型。返回 error 时尤其要避免 typed nil。
- 指针只解决共享同一值的问题，不解决数据竞争。多个 goroutine 同时读写同一字段仍需同步。
- 不要为了“性能”把所有参数都改成指针。小 struct 的值传递往往更简单，是否优化应由 profile 证明。

---

## 7. interface：后端可替换边界的核心

### 7.1 隐式实现

Go 类型不写 `implements`。只要方法集匹配，就自动实现接口：

```go
type Maker interface {
    CreateToken(username string, role string, duration time.Duration) (string, *Payload, error)
    VerifyToken(token string) (*Payload, error)
}
```

`*PasetoMaker` 有这两个方法，所以它实现 `Maker`。`api.Server` 和 `gapi.Server` 依赖 `token.Maker`，业务代码不必绑定到具体 PASETO 实现。

隐式实现降低了类型和接口之间的耦合，也让测试替身容易接入。但它不是“所有东西都要接口化”的理由。

### 7.2 Store 的接口嵌入

`db/sqlc/store.go`：

```go
type Store interface {
    Querier
    TransferTx(ctx context.Context, arg TransferTxParams) (TransferTxResult, error)
    CreateUserTx(ctx context.Context, arg CreateUserTxParams) (CreateUserTxResult, error)
    VerifyEmailTx(ctx context.Context, arg VerifyEmailTxParams) (VerifyEmailTxResult, error)
}
```

`Querier` 由 sqlc 生成，包含所有单条 SQL 方法。接口嵌入后，Store 等于“全部基础查询 + 三个手写事务方法”。`SQLStore` 嵌入 `*Queries` 并实现事务方法，因此满足 Store。API 测试则使用 `db/mock/store.go` 中生成的 MockStore 替代真实数据库。

这形成依赖倒置：服务层依赖能力契约 `db.Store`，不是直接依赖 `*pgxpool.Pool`。

### 7.3 接口应放在哪里

Go 社区常见的实用原则是：接口由使用方按需要定义，保持小而聚焦。当前 Store 很大，因为它嵌入了全部 Querier；优点是注入简单，缺点是任何只需要 `GetAccount` 的 handler 在类型层面也依赖全部数据库能力，Mock 生成文件也较大。

工业项目可按业务用例定义更小接口，例如：

```go
type accountGetter interface {
    GetAccount(context.Context, int64) (db.Account, error)
}
```

但也不要在没有替换或测试需求时提前制造几十个一方法接口。好的接口来自调用方真实需要，而不是为了套设计模式。

### 7.4 编译期断言

重要实现有时会添加：

```go
var _ token.Maker = (*PasetoMaker)(nil)
```

它不创建运行时对象，只让编译器验证方法集。当前仓库没有这样写，因此不能声称已经实现了编译期断言；这是可选的增强做法。

---

## 8. slice 与 map

### 8.1 slice 不是数组本身

slice 可理解为对底层数组一段区域的描述，包含指针、长度和容量。常见创建：

```go
currencies := []string{EUR, USD, CAD}
opts := []asynq.Option{
    asynq.MaxRetry(10),
    asynq.ProcessIn(10 * time.Second),
    asynq.Queue(worker.QueueCritical),
}
```

`len(s)` 是元素数，`cap(s)` 是从起点到底层数组末端的容量。`append` 可能复用底层数组，也可能分配新数组并返回新 slice，所以必须接住返回值：

```go
s = append(s, value)
```

当前 gRPC 校验函数使用：

```go
violations = append(violations, fieldViolation("email", err))
```

返回值 `violations` 初始为 nil slice；nil slice 可以安全 append，这体现了有用的零值。

工业实践中要防止两个 slice 意外共享底层数组导致互相修改；也要防止从巨大 buffer 切出很小 slice 后长期持有，导致整块底层内存不能回收。跨边界保存数据时，有时需要 `copy` 明确断开共享。

### 8.2 map 是哈希映射

转账并发测试：

```go
existed := make(map[int]bool)
existed[k] = true
```

map 查找可以使用“comma ok”：

```go
value, ok := m[key]
```

`ok` 区分“键不存在”和“键存在但值恰好为零值”。用 `map[T]struct{}` 可以表达集合，避免 bool 值；测试中的 bool map 易懂，也没有错误。

必须牢记：

- nil map 可读取，不能写入；写前用 `make` 或字面量初始化；
- 普通 map 不保证迭代顺序；业务输出需要稳定顺序时显式排序；
- 普通 map 不能在无同步时被多个 goroutine 并发读写；使用锁、单 goroutine 所有权或在合适场景下使用 `sync.Map`；
- 从 map 读出的 struct 是值副本，不能直接对其字段赋值，通常取出、修改、再放回，或存指针。

---

## 9. error：显式失败、包装与分类

### 9.1 error 是接口，不是异常

`error` 是标准接口：只要求 `Error() string`。Go 代码把失败作为普通返回值传递：

```go
account, err := server.store.GetAccount(ctx, req.ID)
if err != nil {
    // 分类并转换为 API 错误
}
```

这种风格看似重复，优点是每层都必须决定：处理、转换、补充上下文，还是继续返回。

不要在普通可恢复错误上 panic。panic 适合“不变量被破坏、程序无法合理继续”的情况；输入非法、数据库暂时不可用、记录不存在都应该是 error。

### 9.2 哨兵错误与 errors.Is

`db/sqlc/error.go`：

```go
var ErrRecordNotFound = pgx.ErrNoRows
```

API 使用：

```go
if errors.Is(err, db.ErrRecordNotFound) {
    // 返回 NotFound
}
```

`errors.Is` 会沿着错误包装链检查，不应使用 `err == target` 替代它，因为中间层可能用 `%w` 包装错误。

令牌包也定义：

```go
var (
    ErrInvalidToken = errors.New("token is invalid")
    ErrExpiredToken = errors.New("token has expired")
)
```

哨兵错误适合调用方只需要判断稳定类别的场景。错误文本不是稳定 API；不要通过 `strings.Contains(err.Error(), ...)` 分类。

### 9.3 类型错误与 errors.As

PostgreSQL 会返回包含 SQLSTATE 的 `*pgconn.PgError`。项目写法：

```go
func ErrorCode(err error) string {
    var pgErr *pgconn.PgError
    if errors.As(err, &pgErr) {
        return pgErr.Code
    }
    return ""
}
```

`errors.As` 沿错误链寻找可赋给目标类型的具体错误，并把它放入 `pgErr`。这适合需要读取结构化字段（这里是 Code）时使用。项目定义 `23503` 为外键冲突，`23505` 为唯一约束冲突；这些是当前代码实际判断的两个 SQLSTATE。

### 9.4 `%w` 与错误上下文

任务生产者：

```go
return fmt.Errorf("failed to enqueue task: %w", err)
```

`%w` 既添加“在哪一步失败”的上下文，又保留原错误供 `errors.Is/As` 判断。`%v` 只格式化文本，不建立可解包的链。

当前 `db/sqlc/exec_tx.go` 在 rollback 也失败时写：

```go
fmt.Errorf("tx err: %v, rb err: %v", err, rbErr)
```

这样保留了文本，却没有可供 `errors.Is/As` 遍历的包装关系。现代 Go 可以考虑 `errors.Join`，或至少明确包装主要错误并记录 rollback 错误。是否改变要结合调用方分类需求和日志策略，不能机械替换。

### 9.5 工业级错误边界

典型分层方式：

1. 基础设施层保留数据库/网络原始错误并加操作上下文；
2. 业务层将已知情况映射为稳定领域错误；
3. API 层将领域错误映射为 HTTP/gRPC 状态；
4. 日志记录内部细节，对客户端只返回安全、稳定的信息。

当前项目已经使用 `Is/As/%w`，方向正确；但部分 gRPC 返回直接拼入底层 `err` 文本，`main.go` 若干 fatal 日志没有 `.Err(err)`，会丢失根因。工业环境还应带 request/trace ID，并避免把数据库语句、令牌或用户隐私暴露给客户端。

---

## 10. defer：把清理动作绑定到当前函数

`defer f()` 将调用安排在当前函数返回前执行，多个 defer 按后进先出顺序执行。最常见用途是“资源获取成功后，立刻登记清理”：

```go
ctrl := gomock.NewController(t)
defer ctrl.Finish()
```

当前 `api/account_test.go` 使用这种写法。当前 gomock 版本与 `testing.T` 的集成可能自动做清理，但仓库显式 defer 仍清楚表达了生命周期。

`main.go` 的 Gateway：

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
```

这保证 `runGatewayServer` 返回时释放 context 相关资源。但当前 `http.Serve` 正常情况下长期阻塞，而且程序没有 signal 驱动的优雅关闭，所以这个 defer 并不能单独构成完整 shutdown 方案。

### 10.1 defer 的求值时机

defer 语句执行时，函数参数通常就已求值：

```go
defer logValue(x) // 此刻捕获 x 的值
```

如果希望退出时读取最新 x，可用闭包：

```go
defer func() { logValue(x) }()
```

闭包捕获变量，而非一定复制当时的值；这又会引出循环变量和并发问题。

### 10.2 常见坑

- 在很大的循环中 defer 文件关闭，会等整个函数返回才释放，可能耗尽文件描述符；把单次循环抽成小函数。
- 资源获取失败前不要登记依赖非 nil 资源的 defer。
- `os.Exit`、`log.Fatal` 会直接退出进程，不执行普通 defer。当前 `main.go` 使用 zerolog 的 `log.Fatal` 处理启动失败，因此不能指望它之后的 defer 完成清理。
- 不要用 defer 悄悄吞掉 close/commit 错误；关键持久化操作的错误要明确处理。

---

## 11. context：请求的取消、截止时间和请求级元数据

### 11.1 context 解决什么问题

假设客户端已经断开，但服务仍在执行慢 SQL、调用 Redis 和 SMTP。如果没有取消信号，下游工作会白白占用连接和 goroutine。`context.Context` 用于沿调用链传递：

- 取消信号；
- deadline/timeout；
- 少量请求级元数据。

它不是业务参数袋，不应把 username、数据库连接、配置等所有东西塞进去。

约定是 `ctx context.Context` 放在函数第一个参数，沿调用链原样传下去：

```go
func (server *Server) UpdateUser(ctx context.Context, req *pb.UpdateUserRequest) ...

user, err := server.store.UpdateUser(ctx, arg)
```

pgx、gRPC、Asynq 都接受 context，因此取消能够在支持的边界继续传播。

### 11.2 Background、WithCancel 与 timeout

- `context.Background()`：根 context，适合 main、测试和初始化，不会自行取消；
- `context.WithCancel(parent)`：返回子 context 和 `cancel`；
- `context.WithTimeout(parent, d)`：到期自动取消；
- `context.WithDeadline(parent, t)`：指定绝对截止时间。

创建可取消 context 后通常立即 `defer cancel()`，即使提前完成也及时释放计时器等资源。

### 11.3 本项目现状

- gRPC handler 接收框架提供的请求 context，并传给 Store；这是正确方向。
- Gin 的 `*gin.Context` 在当前 pgx 调用中被直接作为 context 参数。Gin Context 提供相关方法，但工程中更常见的清晰写法是把标准请求 context `ctx.Request.Context()` 传给基础设施层，避免业务/数据层依赖 Web 框架语义。
- `main.go` 创建连接池时使用 `context.Background()`，未设置启动超时；服务启动和下游调用缺少统一 timeout 策略。
- 当前没有基于 SIGINT/SIGTERM 取消根 context 和优雅关闭所有服务的实现。

工业系统通常在入口创建 signal-aware 根 context，为外部依赖设置符合 SLO 的超时，并确保数据库、HTTP 客户端真正接受该 context。不要在深层函数随手换成 `context.Background()`，那会切断调用方的取消链。

---

## 12. goroutine：并发执行，但不是自动安全

### 12.1 goroutine 基础

在函数调用前加 `go`，调用会在新的 goroutine 中并发执行：

```go
go runTaskProcessor(config, redisOpt, store)
go runGatewayServer(config, store, taskDistributor)
runGrpcServer(config, store, taskDistributor)
```

当前 main 用两个 goroutine 启动 Worker 和 HTTP Gateway，主 goroutine 运行 gRPC Server。goroutine 很轻量，但不是免费资源；它仍占栈、调度和所持连接。无限创建 goroutine 会造成内存和下游服务压力。

“并发”是多个任务在一段时间内推进；“并行”是同一时刻在多个执行单元上运行。goroutine 表达并发，是否真正并行由调度器、CPU 和阻塞情况决定。

### 12.2 数据竞争与 happens-before

多个 goroutine 同时访问同一变量，至少一个写，且没有同步，就可能产生 data race。结果不只是“最后一次写覆盖”，Go 内存模型下行为不可依赖。使用 channel、`sync.Mutex`、`sync/atomic` 或明确的单所有者模型建立同步。

数据库本身也有并发控制，但它只保护数据库状态，不会自动保护进程内 map 或 struct 字段。

开发和 CI 应运行：

```bash
go test -race ./...
```

Race detector 只能发现测试实际执行路径中的竞争，不是数学证明；仍需设计正确的所有权和同步策略。

### 12.3 生命周期和错误传播

当前 main 的两个后台 goroutine 若提前失败，会在内部调用 fatal 结束进程；它们没有将错误传回统一协调者。工业界常使用 `errgroup.WithContext`：一个关键服务失败时取消其他服务，收集首个错误，随后按顺序优雅关闭 HTTP/gRPC、Worker、Redis 和数据库连接池。

最危险的 goroutine 是“启动了但没人知道何时结束”。创建 goroutine 时要回答：

1. 谁拥有它？
2. 如何停止？
3. 错误交给谁？
4. 如果下游变慢，是否无限堆积？

---

## 13. channel：goroutine 之间的同步和传值

### 13.1 无缓冲 channel

转账集成测试创建：

```go
errs := make(chan error)
results := make(chan TransferTxResult)
```

没有容量参数，这是无缓冲 channel。一次发送必须等到另一个 goroutine 接收，接收也会等待发送，因此它既传值又形成同步点。

测试启动 5 个 goroutine，每个执行转账后：

```go
errs <- err
results <- result
```

主测试 goroutine 接收 5 次并断言。这样可以观察并发事务结果。

### 13.2 缓冲 channel、close 和 range

`make(chan T, n)` 创建容量 n 的缓冲 channel。在缓冲未满时发送可以先完成，但它不是“永不阻塞的队列”。容量应来自吞吐和背压设计，而不是随便设一个巨大值掩盖消费者过慢。

`close(ch)` 表示以后不会再发送。只有发送方/拥有发送生命周期的一方应关闭；向已关闭 channel 发送会 panic，重复关闭也会 panic。接收方可用：

```go
v, ok := <-ch
```

`ok == false` 表示已关闭且数据耗尽；也可以 `for v := range ch` 读到关闭。

当前转账测试知道精确的发送次数，所以没有 close，也不会 range；这是可行的。若某个 goroutine 在发送前 panic 或永久阻塞，测试主 goroutine也会一直等。工业测试应结合 context/timeout，并考虑使用 `errgroup` 简化错误收集。

### 13.3 select

`select` 在多个 channel 操作间等待，常与取消组合：

```go
select {
case result := <-results:
    return result, nil
case <-ctx.Done():
    return zero, ctx.Err()
}
```

这是实现超时、取消和背压的基础。不要用 `time.Sleep` 猜测并发任务何时完成；应通过 channel、WaitGroup 或 errgroup 建立明确同步。

---

## 14. 闭包：函数携带外部环境

匿名函数可引用外层变量，这种函数值称为闭包。事务代码：

```go
err := store.execTx(ctx, func(q *Queries) error {
    result.Transfer, err = q.CreateTransfer(ctx, ...)
    // ...
    return err
})
```

闭包捕获 `ctx`、`arg`、`result`。`execTx` 不需要知道转账业务，只需在事务中执行闭包。

创建用户流程还有业务回调：

```go
AfterCreate: func(user db.User) error {
    return server.taskDistributor.DistributeTaskSendVerifyEmail(ctx, ...)
},
```

它捕获 server 和请求 ctx。这里要区分语言事实与架构正确性：闭包确实让回调方便；但把 Redis 入队放在 PostgreSQL 事务回调中，并不能让两个系统形成真正原子事务。这个跨系统一致性问题留到异步阶段学习。

### 14.1 循环闭包坑

在循环里启动 goroutine 时，要确保每个闭包拿到预期的迭代值。当前 `TestTransferTxDeadlock` 每轮定义 `fromAccountID` 和 `toAccountID` 后再启动闭包。在维护跨 Go 版本代码或改动循环结构时，最清晰的方式是显式传参：

```go
go func(fromID, toID int64) {
    // 使用 fromID、toID
}(fromAccountID, toAccountID)
```

这样读者无需依赖对特定循环变量语义的记忆。闭包还可能延长被捕获大对象的生命周期；长期运行回调不要无意捕获整个 request 或巨大 buffer。

---

## 15. 测试基础：让“我觉得能跑”变成可重复证据

### 15.1 testing 与约定

Go 测试文件以 `_test.go` 结尾，测试函数形如：

```go
func TestPassword(t *testing.T)
```

`go test ./...` 测试当前 module 的所有包。项目 Makefile 的 `test` 目标实际运行：

```bash
go test -v -cover -short ./...
```

- `-v` 输出详细测试名；
- `-cover` 统计覆盖率；
- `-short` 允许测试跳过慢或有外部副作用的场景，当前真实邮件测试会检查 `testing.Short()`。

覆盖率只说明代码被执行过，不说明断言正确，也不等同于业务风险覆盖。金融事务更需要不变量测试，而不是追求一个漂亮百分比。

### 15.2 Arrange—Act—Assert

`util/passsword_test.go`（文件名当前确实拼作三个 s）展示了清晰流程：

1. Arrange：生成 password；
2. Act：调用 `HashPassword`；
3. Assert：检查无错误、结果非空；
4. 再验证正确密码成功、错误密码失败、同一密码两次 hash 不同。

项目使用 `testify/require`。`require.NoError(t, err)` 失败会停止当前测试，适合后续断言依赖该结果时使用。

### 15.3 表驱动测试与子测试

`api/account_test.go` 定义 test case slice，每项包含名字、输入、鉴权设置、Mock 期望和响应检查，然后：

```go
for i := range testCases {
    tc := testCases[i]
    t.Run(tc.name, func(t *testing.T) {
        // 执行一例
    })
}
```

这种结构让成功、未授权、未找到、内部错误、非法 ID 等分支并列，新增案例只需加一项。`tc := testCases[i]` 也让闭包捕获的测试项明确。

### 15.4 Mock 的能力和边界

API 测试使用 gomock：

```go
store.EXPECT().
    GetAccount(gomock.Any(), gomock.Eq(account.ID)).
    Times(1).
    Return(account, nil)
```

它验证 handler 是否以正确参数调用 Store、调用多少次、如何处理返回值。Mock 适合隔离 API 逻辑，但不能证明 SQL 正确、事务并发正确或数据库约束存在。因此项目还有 `db/sqlc` 中连接真实 PostgreSQL 的集成测试。

好的测试组合通常是：大量快速单元测试 + 足量数据库/协议集成测试 + 少量端到端测试。不要把所有依赖都 Mock 掉后宣称系统工作；也不要让每个小函数都必须启动完整容器环境。

### 15.5 并发测试在验证什么

`db/sqlc/store_test.go` 的 `TestTransferTx` 不仅检查“没有 error”，还检查每笔流水、转账记录、两个账户余额变化和最终总结果；`TestTransferTxDeadlock` 让相反方向的转账并发执行。它们验证重要的不变量。

当前测试仍有可改善点：并发等待没有超时；数据库测试依赖环境；随机测试数据可增加覆盖面，但失败重现需要记录 seed/输入；测试代码中存在 `TODO: check accounts balance` 注释，虽然后文最终余额已经有断言，说明注释可能过时。工业维护应及时清理误导性 TODO。

### 15.6 最低工程检查

每次改动至少考虑：

```bash
gofmt -w <你修改的.go文件>
go test ./...
go vet ./...
go test -race ./...
```

数据库和外部服务测试需要相应依赖。不能因为本机没有 PostgreSQL 就把失败说成代码 bug，也不能因为跳过集成测试就说“所有测试通过”；报告必须写清实际运行范围。

---

## 16. 生成代码边界：源文件和产物不能混为一谈

当前仓库有三类明显生成代码：

1. `db/sqlc/*.sql.go`、`models.go`、`querier.go`：根据 `db/query/*.sql` 和 schema 由 sqlc 生成；
2. `pb/*.pb.go`、`*_grpc.pb.go`、`*.pb.gw.go`：根据 `proto/*.proto` 由 protoc 及插件生成；
3. `db/mock/store.go`、`worker/mock/distributor.go`：由 mockgen 根据接口生成。

例如 `db/sqlc/account.sql.go` 文件头明确写：

```go
// Code generated by sqlc. DO NOT EDIT.
```

`pb/user.pb.go` 也写：

```go
// Code generated by protoc-gen-go. DO NOT EDIT.
```

正确变更路径：

```text
数据库查询变化
  db/query/account.sql
      ↓ make sqlc
  db/sqlc/account.sql.go

API 契约变化
  proto/*.proto
      ↓ make proto
  pb/*.go + doc/swagger 产物

接口变化
  db.Store / worker.TaskDistributor
      ↓ make mock
  db/mock / worker/mock
```

不要直接修改生成文件：下次生成会覆盖，代码评审也无法判断真实源头。生成产物是否提交到 Git 取决于团队策略；当前仓库已经提交这些产物，所以更改源文件后应一并再生成并检查 diff。

工业界还应固定生成工具版本，避免不同开发机生成不同输出；在 CI 中执行生成并检查工作树是否产生差异，能防止忘记提交产物。当前 Makefile 提供 `sqlc`、`proto`、`mock` 命令，但没有从当前代码中看到“CI 强制验证生成文件未过期”的证据，因此不能声称已经做到。

生成代码也不代表可信边界消失：SQL 和 Proto 源文件仍需评审，生成结果仍要编译测试，工具版本升级仍可能改变 API。

---

## 17. 当前项目在 Go 工程方面做得好与不足

### 17.1 做得好的地方

- 以 module 和职责包组织代码，入口和各层基本清楚。
- 使用多返回值和显式 error，而不是隐藏失败。
- 使用 `db.Store`、`token.Maker`、`TaskDistributor`、`EmailSender` 等接口隔离基础设施，便于 Mock。
- 使用 struct 参数表达一组业务输入，避免长参数列表。
- 使用 `context.Context` 贯穿 gRPC、数据库和任务调用。
- 使用 `%w`、`errors.Is`、`errors.As` 处理可分类错误。
- 有单元测试、数据库集成测试、表驱动测试和并发事务测试。
- 生成文件带有清晰标记，Makefile 有再生成入口。

### 17.2 不足和学习时不要照抄的地方

- 当前多个手写 Go 文件格式不统一；Go 项目应把 `gofmt` 作为不可协商的基线。
- `main.go` 启动多个长期服务，却没有统一错误传播、信号处理和优雅关闭。
- 部分 fatal 日志没有附带 `.Err(err)`，定位启动失败会缺根因。
- `execTx` 合并事务错误和 rollback 错误时使用 `%v`，错误链信息不足。
- `Store` 是一个较大的接口，测试方便但调用方依赖面偏宽。
- 一些注释与真实函数不一致，例如 `CreateUserTx` 注释仍说执行 money transfer，说明复制粘贴后未修正。
- `util/random.go` 使用包级 `math/rand` 和 `init` seed，适合课程测试数据，但不应用来生成安全验证码、令牌或密钥；当前 Worker 的验证码也调用 `RandomString(32)`，这是后续安全阶段必须修正的问题。
- Gin 层把 `*gin.Context` 直接向 Store 传递；更清晰的基础设施边界通常使用标准 `ctx.Request.Context()`。
- 当前代码包含较多注释掉的旧实现，应通过版本控制保留历史，而不是让死代码长期干扰阅读。

这些不足不否定项目的教学价值。恰恰相反，它们帮助你学习一个重要能力：读代码时区分“这个语法如何工作”和“这个工程选择是否已经生产化”。

---

## 18. 循序练习：从会读到会写

以下练习建议在新分支完成，不要直接修改生成文件。每个练习都给出验收标准，而不是只说“写完即可”。

### 练习 1：建立 package 和测试

新建一个 `money` 包，定义具名类型：

```go
type Amount int64
type Currency string
```

实现 `IsSupportedCurrency`，支持当前项目的 USD/EUR/CAD。

验收：

- 表驱动测试覆盖三个合法值、空字符串、小写 usd 和未知值；
- `go test ./money` 通过；
- 所有导出标识符有说明用途的注释；
- 代码经 gofmt。

### 练习 2：理解多返回值和错误

实现：

```go
func ParseAmount(raw string) (Amount, error)
```

只接受十进制整数且必须大于 0。定义 `ErrInvalidAmount`，底层解析错误用 `%w` 包装。

验收：

- `errors.Is(err, ErrInvalidAmount)` 对所有非法输入为 true；
- 错误文本含原输入的安全上下文；
- 不使用 panic；
- 测试覆盖 1、0、负数、空字符串、非数字和超大整数。

### 练习 3：小接口与替身

定义只包含 `GetAccount` 的接口，并写一个内存 fake：map 保存账户，方法返回账户或 `db.ErrRecordNotFound`。

验收：

- 通过编译期断言证明 fake 实现接口；
- 找不到时测试使用 `errors.Is`；
- map 在构造函数初始化，不会对 nil map 写；
- 明确说明该 fake 是否允许并发访问。若允许，使用 mutex 并跑 `go test -race`。

### 练习 4：context 取消

写 `WaitForApproval(ctx context.Context) error`，模拟等待一个 channel；若 ctx 先取消，返回 `ctx.Err()`。

验收：

- 一个测试发送批准信号并成功；
- 一个测试使用 `context.WithTimeout` 并得到 deadline exceeded；
- 测试没有 `time.Sleep` 猜执行时序；
- 所有创建的 cancel 都被调用。

### 练习 5：受控并发

启动固定数量 worker，从 jobs channel 接收 100 个整数，计算平方并把结果发送到 results channel。使用 WaitGroup 在全部 worker 退出后关闭 results。

验收：

- 不泄漏 goroutine，不向已关闭 channel 发送；
- 结果正好 100 个、不重复、不遗漏；
- 支持 context 取消；
- `go test -race` 通过；
- 能口头解释由谁关闭 jobs 和 results，以及为什么。

### 练习 6：改进事务错误（设计题）

不要立刻改生产代码。先写一份小设计：`execTx` 中业务错误和 rollback 错误同时发生时，调用方需要保留哪些分类能力？比较 `%v`、单一 `%w` 和 `errors.Join`。

验收：

- 示例证明 `errors.Is/As` 在三种方案下的差异；
- 说明日志记录与返回客户端错误的边界；
- 选择方案时写出兼容性和调用方影响，而不是只说“新 API 更好”。

### 练习 7：识别生成边界

给账户查询增加一个只读 SQL（例如按 owner 和 currency 查询），从 `db/query` 源文件开始，执行 sqlc 生成并增加测试。

验收：

- 没有直接手改 `*.sql.go`；
- 能从 Git diff 指出源文件与生成产物；
- SQL 名称、参数类型和返回数量正确；
- 相关数据库集成测试在已准备的 PostgreSQL 环境中通过；若环境不可用，报告“未执行”而不是声称通过。

---

## 19. 自测题

先独立作答，再看下一节答案。

1. `module github.com/lllmml/simplebank` 与 `package util` 分别解决什么问题？
2. 为什么 `connPool, err := ...` 在已有 err 时仍可能合法？
3. `var s []string` 和 `m := make(map[string]int)` 的零值/初始化行为有何区别？
4. 为什么账户金额用 `int64` 通常比 `float64` 更合适？当前项目还缺少什么单位约定？
5. struct tag 由谁解释？拼错 tag 为什么可能编译仍通过？
6. 嵌入是不是继承？`pb.UnimplementedSimpleBankServer` 在当前 Server 中做什么？
7. `PasetoMaker` 为什么无需声明 `implements Maker`？
8. 若方法都定义在 `*T` 上，T 和 `*T` 谁满足该接口？
9. `errors.Is` 与 `errors.As` 各适合什么场景？
10. `%w` 与 `%v` 对错误链有什么差异？
11. `defer cancel()` 能否自动让一个没有退出条件的 HTTP Server 优雅停止？
12. 为什么不应该在深层 Store 方法中换用 `context.Background()`？
13. 无缓冲 channel 的一次发送何时完成？谁应该关闭 channel？
14. goroutine 很轻量，是否意味着可以无限创建？
15. 闭包捕获了什么？为什么循环里显式把变量作为匿名函数参数更清楚？
16. Mock Store 测试通过，是否证明 SQL 正确？
17. 为什么不能手改 `db/sqlc/account.sql.go`？真正的源文件在哪里？
18. 当前 main 的并发启动方式离工业级生命周期管理还差什么？

## 20. 自测题答案

1. module 是依赖和导入路径的版本化工程边界；package 是源代码的组织、命名和导出边界。
2. 短声明左侧只要至少有一个当前作用域的新变量就合法；connPool 新建，err 被重新赋值。要留意内层作用域可能遮蔽外层 err。
3. nil slice 可读取、遍历和 append；map 必须初始化后才能写。这里 `m` 已经 make，可以写。
4. 整数避免二进制浮点对十进制金额的精度问题。当前项目没有清楚声明整数代表元、分或其他最小单位，也缺更强的领域类型。
5. `encoding/json`、Gin validator、Viper 等库通过反射解释各自 tag；Go 编译器不知道多数第三方 tag 的业务含义，所以拼错可能只是运行行为失效。
6. 不是继承，而是组合和方法提升。嵌入生成的未实现服务，为 gRPC 服务接口提供默认方法并满足生成代码要求。
7. Go 按方法集隐式判断接口实现；方法签名匹配即可。
8. 通常是 `*T`；指针接收者方法不属于 T 的方法集。
9. Is 判断某个稳定目标/哨兵错误是否在链上；As 查找某个具体错误类型并读取其结构化字段。
10. `%w` 包装错误，可被 Unwrap、Is、As 遍历；`%v` 只把错误格式化进字符串。
11. 不能。defer 只有函数返回时才执行；还需要 signal、Server Shutdown/GracefulStop、goroutine 协调和资源关闭流程。
12. 它会切断上游请求取消和 deadline，客户端断开后数据库工作可能继续。
13. 无缓冲发送与对应接收会合后才完成。知道“不再有发送”的发送侧/生命周期拥有者负责关闭；接收方通常不应擅自关闭。
14. 不能。goroutine 占内存、调度资源，也可能持有连接；无界创建会造成资源耗尽和下游过载。
15. 闭包引用外层变量和环境。显式传参使每个 goroutine 使用哪个值一目了然，也减少对循环变量细节的依赖。
16. 不能。Mock 只证明调用交互和 handler 分支，需要真实数据库集成测试验证 SQL、schema 和事务。
17. 它明确是 sqlc 生成产物，下次生成会覆盖。查询源文件在 `db/query/account.sql`，schema 来源还涉及 migration/sqlc 配置。
18. 缺少统一错误传播、signal-aware 根 context、取消协调、HTTP/gRPC/Worker 优雅停止、Redis 和连接池关闭，以及 shutdown 超时。

---

## 21. 术语表

| 术语 | 本文中的准确含义 |
|---|---|
| Module | 由 `go.mod` 定义的依赖与导入路径单元 |
| Package | 同一职责的一组 Go 源文件及其命名/导出边界 |
| 导出 | 标识符首字母大写，可由其他包引用 |
| 零值 | 变量未显式初始化时由 Go 提供的默认值 |
| Shadowing | 内层作用域的新变量遮蔽同名外层变量 |
| Struct | 由具名字段组合成的值类型 |
| Struct tag | 附在字段上的字符串元数据，由库通过反射解释 |
| 指针 | 指向某个值的地址型引用，类型写作 `*T` |
| 方法接收者 | 方法名前绑定的类型或类型指针 |
| Interface | 一组方法签名定义的能力契约 |
| 隐式实现 | 类型只要方法集匹配就满足接口，无需声明 implements |
| 嵌入 | 匿名组合字段或接口，并产生字段/方法提升；不是继承 |
| Slice | 对底层数组某段区域的动态视图，带长度和容量 |
| Map | 键到值的哈希映射，普通 map 不支持无同步并发写 |
| Sentinel error | 可供调用方用 `errors.Is` 判断的稳定错误值 |
| 错误包装 | 用 `%w` 等方式为错误加上下文并保留错误链 |
| defer | 在当前函数返回前按后进先出执行的延迟调用 |
| Context | 传递取消、deadline 和少量请求级元数据的对象 |
| Goroutine | 由 Go runtime 调度的并发执行单元 |
| Channel | goroutine 间传值并建立同步的类型 |
| Data race | 多 goroutine 无同步访问同一内存，且至少一个写 |
| Closure | 引用了外层变量的函数值 |
| Table-driven test | 用测试用例表和子测试批量覆盖输入/分支的写法 |
| Mock | 按预期模拟依赖交互的测试替身 |
| Integration test | 使用真实组件边界验证组合行为的测试 |
| Generated code | 由 sqlc/protoc/mockgen 等工具从源契约生成的产物 |

## 22. 阶段完成清单

当你能诚实勾选以下全部项目，才算完成第一阶段：

- [ ] 能从 `main.go` 画出启动调用链，并指出哪些调用在新 goroutine 中。
- [ ] 能解释 module、package、导出、普通导入、别名导入和空白导入。
- [ ] 能正确使用 `var`、`:=`，并识别一个变量遮蔽问题。
- [ ] 能解释常用基本类型、零值，以及为什么金额不用浮点。
- [ ] 能写返回 `(value, error)` 的函数，并在每个调用点处理错误。
- [ ] 能定义 struct、理解三类 tag，并知道 tag 由库解释。
- [ ] 能区分值接收者与指针接收者，知道 nil 指针和 typed nil 风险。
- [ ] 能解释 `Maker` 和 `Store` 如何被隐式实现，以及嵌入为什么不是继承。
- [ ] 能安全使用 slice 和 map，知道底层数组共享与 map 并发写的风险。
- [ ] 能按场景使用 `errors.Is`、`errors.As`、`%w`，不解析错误文本分类。
- [ ] 能说明 defer 的执行时机、顺序及 `log.Fatal` 不执行 defer 的影响。
- [ ] 能将请求 context 继续传给下游，并写出可取消、可超时的等待逻辑。
- [ ] 能创建有明确所有者、退出条件和错误去向的 goroutine。
- [ ] 能解释无缓冲/缓冲 channel、close 的所有权和 select 取消。
- [ ] 能识别闭包捕获，并避免循环闭包和大对象生命周期问题。
- [ ] 能写普通测试、表驱动测试，理解 Mock 与数据库集成测试的边界。
- [ ] 能从 SQL/Proto/接口源文件重新生成代码，绝不手改 `DO NOT EDIT` 文件。
- [ ] 已完成至少练习 1—5，并运行 `gofmt`、`go test`；条件允许时运行 `go vet` 和 `go test -race`。

完成这一阶段后，你应该不只是“看得懂几行 Go”，而是能说明一段后端 Go 代码的类型边界、失败路径、并发生命周期和测试证据。带着这些能力进入数据库阶段，事务、锁和连接池才不会变成需要死记的黑箱。
