# 第二阶段：PostgreSQL、sqlc、事务与并发正确性

> 适合读者：第一次系统学习数据库、后端和 Go 的同学。
> 本章目标：不只会“把 SQL 跑起来”，还要能解释数据为什么不会在并发请求下悄悄出错。

这一阶段是 simplebank 最重要的地基。HTTP、gRPC、Token、Redis 都可以稍后替换，但账户余额一旦算错，后面的技术再漂亮也没有意义。请把本章的主线记成一句话：

> 先用数据库约束定义“什么数据允许存在”，再用事务定义“一组操作如何一起成功”，最后用锁、固定顺序、幂等和并发测试证明“多人同时操作也正确”。

本章先讲通用原理，再逐层回到仓库中的实际实现。文末有可执行实验、自测题、答案和掌握清单。

---

## 1. 先从 Git 历史看这一阶段解决了什么

不要把当前代码当成凭空出现的最终答案。这个项目的价值之一，是 Git 历史保留了问题被逐步发现和修正的过程。

| 提交 | 当时引入或解决的问题 | 应学会的能力 |
|---|---|---|
| `d5427cc`（backend5） | 首次加入 `accounts`、`entries`、`transfers`，Migration、CRUD SQL、sqlc 生成代码及数据库测试 | 关系建模、SQL、代码生成、集成测试 |
| `9cf70c9`（backend6） | 新增 `Store`、`execTx` 和 `TransferTx`；最初事务只写 transfer 和两条 entry，还留下余额检查 TODO | 多条 SQL 的原子提交与回滚 |
| `8f41e9a`（backend7） | 新增 `AddAccountBalance`，并发执行 5 次同向转账，检查中间及最终余额 | 原子余额更新、并发测试 |
| `b6268f8`（backend8） | 新增双向转账测试；按账户 ID 从小到大更新以避免死锁 | 行锁、死锁、统一锁顺序 |
| `cefdb44`（backend15） | 新增 `users`，令 `accounts.owner` 外键引用用户，并对 `(owner, currency)` 加唯一约束 | 业务约束进入数据库 |
| `1171bd0`（backend49） | 新增 `UpdateUser`，用 `sqlc.narg` 与 `COALESCE` 做部分更新 | SQL NULL 与“未提供字段”的表达 |
| `9fe766f`（backend66） | 从 `database/sql + lib/pq` 切换到 `pgx/v5 + pgxpool`；重生成 sqlc 代码；拆出 `exec_tx.go` | PostgreSQL 原生驱动、连接池、nullable 类型 |
| `e2c40e3`（backend67） | API 不再依赖 `pq.Error`，改用 pgx 的 `PgError` 和 SQLSTATE | 稳定、可分类的数据库错误处理 |

核对时要注意两个历史细节：

1. `cefdb44` 初次创建 `users.email` 时还不是唯一列；`4dd9f82` 才把当前 Migration 改为 `email varchar UNIQUE NOT NULL`。
2. `9fe766f` 切换了业务数据库驱动，但当前 `main.go` 中 golang-migrate 仍注册 `database/postgres` 驱动，仓库也仍保留 `lib/pq` 依赖与空白导入。因此“业务查询使用 pgx”不等于“仓库完全移除了 lib/pq”。

这两点都可以用下面的命令亲自验证：

```bash
git show d5427cc -- db/migration db/query sqlc.yaml
git show 9cf70c9 -- db/sqlc/store.go db/sqlc/store_test.go
git show 8f41e9a -- db/query/account.sql db/sqlc/store.go db/sqlc/store_test.go
git show b6268f8 -- db/sqlc/store.go db/sqlc/store_test.go
git show cefdb44 -- db/migration/000002_add_users.up.sql
git show 1171bd0 -- db/query/user.sql db/sqlc/user.sql.go
git show 9fe766f -- sqlc.yaml db/sqlc
git show e2c40e3 -- api gapi
```

---

## 2. 关系数据库到底在解决什么

### 2.1 表、行、列并不是 Excel 的换皮

关系模型把一种事实表示为一个“关系”，工程实现中通常就是一张表：

- 表描述同一类事实，例如“用户”“账户”“转账”；
- 一行是一条事实，例如“账户 42 属于 alice”；
- 一列是事实的一个属性，例如币种、余额、创建时间；
- 键和约束描述事实之间必须一直成立的规则。

数据库的关键价值不只是保存数据，而是让约束在所有写入入口上生效。即使未来有 HTTP 服务、后台任务、管理脚本和数据修复程序四种入口，它们只要写同一数据库，就都不能绕过主键、外键、唯一约束和 `CHECK`。

应用层校验当然仍然重要：它能给用户更友好的错误。但应用校验和数据库约束不是二选一：

- 应用层负责尽早拒绝、给出易懂反馈；
- 数据库负责最后防线，防止并发竞争、程序 Bug 或其他写入入口制造非法状态。

### 2.2 simplebank 当前的六张表

当前 Migration 最终形成六张核心表：

```text
users(username PK)
  ├── accounts(owner FK -> users.username)
  │     ├── entries(account_id FK -> accounts.id)
  │     ├── transfers(from_account_id FK -> accounts.id)
  │     └── transfers(to_account_id FK -> accounts.id)
  ├── sessions(username FK -> users.username)
  └── verify_emails(username FK -> users.username)
```

逐张看它们表达的事实。

#### `users`：身份主体

`username` 是自然业务键，也是主键；`email` 当前有唯一约束。密码字段保存哈希而不是明文。`password_changed_at` 的默认值是公元 1 年，sqlc 映射成 `time.Time` 后，测试用 `IsZero()` 判断“从未改过密码”。后续 Migration 又加入 `is_email_verified` 和 `role`。

#### `accounts`：某个用户在某个币种下的当前余额

`id` 是 `bigserial` 主键；`owner` 引用 `users.username`；`(owner, currency)` 唯一，表示一个用户同一币种最多一个账户。

`balance` 是“当前余额快照”。它读起来很快，却必须与流水保持一致。这个一致性不能靠愿望，需要同一事务、受控写入口和对账机制。

#### `entries`：账户余额变动流水

一条 entry 只属于一个账户。负数表示减少，正数表示增加。转账时转出账户记 `-amount`，转入账户记 `+amount`。

请注意：当前 `entries` 不是完整的工业级复式记账分录模型。它没有共同的 journal/transaction ID、科目体系、借贷方向约束，也没有数据库约束保证两边合计为零。

#### `transfers`：转账业务事实

记录转出账户、转入账户和金额。它描述“发生了一笔转账”，两条 entries 描述“两个账户各自如何变化”。当前 transfer 与对应 entries 之间没有直接外键；只能依赖同一次 `TransferTx` 创建它们。

#### `sessions`：Refresh Token 的服务端会话

主键是 UUID，关联用户名，并保存 token、客户端信息、封禁和过期状态。外键能阻止会话引用不存在的用户。

#### `verify_emails`：邮箱验证挑战

保存用户名、邮箱、验证码、是否使用、创建时间和过期时间。它关联 users，但当前没有 `(username, secret_code)` 等索引，也没有唯一约束限制一个用户有多少条有效验证记录。

---

## 3. 键、约束与索引：相似，但职责不同

### 3.1 主键

主键同时表达两件事：

1. 每行可以被稳定、唯一地识别；
2. 主键列不能为 NULL。

PostgreSQL 会为主键自动创建唯一 B-tree 索引。例如 `accounts.id` 既是身份标识，也能高效支持 `WHERE id = $1`。

`bigserial` 不是一种独立的“分布式 ID 算法”。它是 PostgreSQL 传统的自增语法，会配套序列并给列设置默认取值。序列取号不保证无间隙：事务回滚、缓存或失败写入都可能留下空洞。因此业务代码只能依赖“唯一”，不能依赖 ID 连续。

### 3.2 外键

外键保证引用完整性。例如：

```sql
ALTER TABLE accounts
ADD FOREIGN KEY (owner) REFERENCES users(username);
```

它保证不存在“账户属于一个根本不存在的用户”。当前这些外键没有写 `ON DELETE CASCADE`，所以默认行为会阻止删除仍被引用的父行。这通常比误删整条资金链更安全，但删除策略必须由业务明确设计。

一个常见误区是“声明外键后两边都有索引”。实际是：

- 被引用列必须是主键、唯一约束或合适的唯一索引，因此被引用侧已有索引；
- PostgreSQL**不会自动为引用侧列建索引**，因为是否值得建、建何种组合索引取决于查询。

本项目为 `entries.account_id`、`transfers.from_account_id`、`transfers.to_account_id` 手动建了索引；但没有为 `sessions.username`、`verify_emails.username` 手动建索引。数据量大后，按用户查会话、删除用户时检查引用等操作可能需要扫描更多行。

### 3.3 唯一约束

当前两项重要唯一规则是：

```sql
users.email UNIQUE
UNIQUE (accounts.owner, accounts.currency)
```

PostgreSQL 会用唯一索引实现唯一/主键约束，所以不要再重复创建同列的普通索引。

`(owner, currency)` 是联合索引，列顺序有意义。B-tree 通常很适合：

- `WHERE owner = ?`
- `WHERE owner = ? AND currency = ?`

却通常不能同样有效地只服务 `WHERE currency = ?`。这也是所谓“最左前缀”思路。是否真正使用索引仍由查询规划器按统计信息和成本决定。

### 3.4 普通索引不是免费午餐

索引用额外空间维护一份可搜索结构，收益是减少某些读取所需访问的数据页；代价包括：

- `INSERT` 要写表，也要写每个相关索引；
- 修改索引列的 `UPDATE` 要维护索引；
- `DELETE`、VACUUM、备份和复制都承担额外成本；
- 索引会占内存缓存和磁盘；
- 索引过多会增加锁和发布维护复杂度。

当前 transfers 同时有：

```text
(from_account_id)
(to_account_id)
(from_account_id, to_account_id)
```

联合索引的首列与第一个单列索引重叠，是否都需要不能凭感觉判断。当前 `ListTransfers` 是：

```sql
WHERE from_account_id = $1 OR to_account_id = $2
```

规划器可能组合两个单列索引做 BitmapOr，也可能在小表上直接顺序扫描。应使用真实数据分布和：

```sql
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM transfers
WHERE from_account_id = 1 OR to_account_id = 1
ORDER BY id
LIMIT 20;
```

来观察，而不是把“有索引”等同于“必然更快”。`ANALYZE` 会真的执行 SQL；对写语句或生产环境使用前必须谨慎。

### 3.5 当前缺失的数据库约束

Migration 的注释说 transfer amount “must be positive”，但注释不会执行规则。当前缺少至少以下防线：

```sql
ALTER TABLE accounts
  ADD CONSTRAINT accounts_balance_nonnegative CHECK (balance >= 0);

ALTER TABLE transfers
  ADD CONSTRAINT transfers_amount_positive CHECK (amount > 0),
  ADD CONSTRAINT transfers_distinct_accounts
    CHECK (from_account_id <> to_account_id);

ALTER TABLE entries
  ADD CONSTRAINT entries_amount_nonzero CHECK (amount <> 0);
```

是否禁止负余额取决于是否允许透支；上面示例假设不允许。币种也缺少数据库约束，可选择受控 `currencies` 表加外键，或在币种集合极稳定时用 `CHECK (currency IN (...))`。工业系统还要考虑不同币种的最小单位和小数位，而不是把所有币种一律当两位小数。

`CHECK` 适合约束本行，不能用来查询另一张表、保证所有 entries 之和等于 account balance。跨行/跨表不变量应通过事务化业务操作、唯一/外键、受控写权限、账本设计和定期对账共同保证。

---

## 4. 数据类型：金额和时间为什么容易埋雷

### 4.1 为什么项目用 `bigint` / Go `int64` 存金额

二进制浮点数不能精确表示许多十进制小数。经典例子是 0.1 在 IEEE 754 二进制浮点中只能是近似值。反复累加、四舍五入和比较时，微小误差可能传播。因此不能直接用 `float32`/`float64` 表示需要精确对账的钱。

项目的做法是用整数保存最小货币单位：

```text
10.01 CNY -> 1001 分
10.01 USD -> 1001 cent
```

数据库 `bigint` 是 8 字节有符号整数，sqlc 映射成 Go `int64`。加减法精确、性能好，也很适合余额这种固定最小单位的场景。

但这套设计还少一个必须写进业务规范的约定：`balance` 和 `amount` 到底代表主单位还是最小单位？当前仓库没有明确说明。如果 API 传入 `10`，它可能被人理解为 10 元，也可能是 10 分。工业代码通常让字段名、API 文档或类型显式表达，例如 `amount_minor`。

整数方案也不是所有金融数值的万能答案：

- JPY 通常没有常用小数位，KWD 常用三位；
- 利率、汇率、税率、按比例分摊需要更多精度；
- 加密资产可能需要更多小数位；
- 乘法可能溢出 `int64`，必须检查范围和舍入规则。

需要可配置小数精度时，PostgreSQL `numeric(p, s)` 是精确十进制类型，代价通常比整数运算高。无论选整数还是 `numeric`，都必须规定舍入模式、币种精度和溢出策略。

### 4.2 `timestamptz` 实际保存什么

当前所有业务时间几乎都用 `timestamptz`。它是 PostgreSQL 对 `timestamp with time zone` 的简称。

正确理解是：

1. 输入若带时区偏移，PostgreSQL 会换算成同一时间点；
2. 内部按 UTC 时间点保存；
3. 查询输出时按当前数据库会话的 `TimeZone` 显示；
4. 原始输入写的是 `Asia/Shanghai` 还是 `+08:00` 不会被原样保留。

所以 `timestamptz` 适合 `created_at`、`expires_at` 这类“世界上的一个时刻”。如果业务必须保留用户选择的时区，还要另存 IANA 时区名，例如 `Asia/Shanghai`。

项目在 `sqlc.yaml` 中把 `timestamptz` override 为 `time.Time`，避免生成 pgx nullable 时间类型；因为这些列都是 `NOT NULL`，这个映射合理。生产中通常统一以 UTC 传输和记录日志，只在展示层转换时区。

---

## 5. Migration：数据库结构也要像代码一样版本化

### 5.1 up/down 是什么

Migration 是按顺序执行的结构变更。项目使用 golang-migrate，每个版本有一对文件：

```text
000001_init_schema.up.sql
000001_init_schema.down.sql
000002_add_users.up.sql
000002_add_users.down.sql
...
```

- `up`：把数据库从旧版本升级到新版本；
- `down`：撤销这一版本的结构变化。

当前版本顺序是：

1. 创建 accounts、entries、transfers 及索引/外键；
2. 创建 users，给 accounts 增加 owner 外键和 `(owner, currency)` 唯一约束；
3. 创建 sessions；
4. 创建 verify_emails，并给 users 增加邮箱已验证字段；
5. 给 users 增加 role。

Makefile 提供：

```bash
make migrateup
make migrateup1
make migratedown1
make new_migration name=add_transfer_constraints
```

`main.go` 当前也会在服务启动时调用 `migration.Up()`；`ErrNoChange` 被当作正常情况。

### 5.2 down 不等于“安全撤销”

例如 drop column/table 会直接丢数据。即使 down SQL 语法正确，也不代表生产回滚业务上安全。实际发布常采用：

- 破坏性变化优先“向前修复”，不是盲目 down；
- 回滚前确认备份、恢复时间目标和数据兼容性；
- 对删除列先停止写、观察、延迟多个发布周期再删；
- 回填大量数据时分批执行，避免长事务和表膨胀。

### 5.3 工业界常用 Expand–Migrate–Contract

假设要把 `full_name` 拆成 `first_name`、`last_name`，不要在一次发布中直接删旧列：

1. **Expand**：先加新列，旧代码仍能运行；
2. **Migrate**：新代码双写或兼容读，后台分批回填并校验；
3. **Contract**：所有实例升级、观察稳定后，才停止旧列并在后续版本删除。

这能支持滚动发布期间“新旧应用实例同时在线”。原则是：

- 已发布的 Migration 不要原地修改；新增下一版本修正；
- schema 与应用要至少在相邻版本间前后兼容；
- 大表建普通索引可能阻塞写，PostgreSQL 可考虑 `CREATE INDEX CONCURRENTLY`；它不能在事务块中执行，必须确认迁移工具和发布方式；
- 新约束可考虑先 `NOT VALID`、清洗历史数据，再 `VALIDATE CONSTRAINT`，但要理解各类约束和 PostgreSQL 版本的支持差异；
- Migration 最好由一次性的发布 Job 执行。每个应用副本启动时都尝试迁移，虽可能有迁移锁保护，仍会把部署、权限、失败恢复和启动时延绑在一起。

---

## 6. SQL CRUD 与 `RETURNING`

CRUD 是 Create、Read、Update、Delete。

项目在 `db/query/account.sql` 中写：

```sql
-- name: CreateAccount :one
INSERT INTO accounts (owner, balance, currency)
VALUES ($1, $2, $3)
RETURNING *;
```

`$1`、`$2`、`$3` 是 PostgreSQL 参数占位符。参数化查询把 SQL 结构与数据分离，避免手拼字符串造成 SQL 注入，也方便驱动传递正确类型。

`RETURNING *` 让 INSERT/UPDATE 在同一条语句中返回最终行，包括数据库生成的 ID、默认创建时间、触发器可能修改的值。它避免“先写再查”的额外往返和竞争窗口。

读取和分页：

```sql
SELECT * FROM accounts
WHERE owner = $1
ORDER BY id
LIMIT $2 OFFSET $3;
```

必须有稳定 `ORDER BY`，否则分页顺序不受保证。`OFFSET` 越大通常扫描/丢弃越多行；高数据量 API 常改用 keyset/cursor pagination：

```sql
WHERE owner = $1 AND id > $2
ORDER BY id
LIMIT $3;
```

删除当前使用 `:exec`，只返回 error，不告诉调用者是否真的删到一行。若业务要区分“不存在”和“已删除”，可使用 sqlc 支持的受影响行数注解，或 `DELETE ... RETURNING`。

---

## 7. sqlc：保留 SQL，又获得 Go 的类型检查

### 7.1 sqlc 不是 ORM

ORM 往往让你用对象/链式 API 构造查询；sqlc 的路线是：

```text
手写 SQL + schema
      ↓ sqlc generate
Go 参数结构体、结果结构体、查询方法、接口
```

你仍然需要懂 SQL、索引和事务。sqlc 的主要价值是减少手写 Scan、参数顺序和类型转换样板，并在生成阶段尽早发现 SQL/schema 不匹配。

当前 `sqlc.yaml` 的关键配置：

```yaml
engine: "postgresql"
queries: "./db/query/"
schema: "./db/migration/"
sql_package: "pgx/v5"
emit_interface: true
emit_empty_slices: true
```

因此当前生成代码使用 pgx/v5，并生成 `Querier` 接口；`:many` 无结果时生成空 slice 而非 nil slice。

### 7.2 注解决定生成方法的形状

sqlc 查询注解形式是：

```sql
-- name: MethodName :command
```

项目主要使用：

- `:one`：预期一行，生成 `(T, error)`；没有行时 pgx 返回 `pgx.ErrNoRows`；
- `:many`：生成 `([]T, error)`；
- `:exec`：只执行并返回 error。

例如：

```sql
-- name: AddAccountBalance :one
UPDATE accounts
SET balance = balance + sqlc.arg(amount)
WHERE id = sqlc.arg(id)
RETURNING *;
```

`sqlc.arg(amount)` 给参数一个清晰名字。生成结果是：

```go
type AddAccountBalanceParams struct {
    Amount int64
    ID     int64
}
```

### 7.3 生成边界必须守住

这些是“源文件”：

- `db/migration/*.sql`
- `db/query/*.sql`
- `sqlc.yaml`

这些是生成物：

- `db/sqlc/models.go`
- `db/sqlc/*sql.go`
- `db/sqlc/querier.go`
- `db/sqlc/db.go`

生成文件顶部明确写着 `Code generated by sqlc. DO NOT EDIT.`。要改变查询，修改源 SQL 后运行：

```bash
make sqlc
git diff -- db/sqlc
```

手改生成文件，下次 generate 会被覆盖。工业 CI 常运行 generate 后检查工作区是否干净，以防开发者忘记提交与 SQL 同步的生成物。

sqlc 能证明的是“SQL 可以按给定 schema 解析、Go 参数与扫描类型一致”；它不能证明查询在生产数据量下高效，也不能证明业务不变量、事务顺序、鉴权和幂等正确。

---

## 8. pgx、pgxpool、Queries、Querier 与 Store

### 8.1 pgx 和连接池分别是什么

pgx 是面向 PostgreSQL 的 Go 驱动/工具包；`pgxpool` 是建立在 pgx 上的并发安全连接池。

建立一次 PostgreSQL 物理连接需要网络握手、认证、服务端进程/会话资源。若每个请求都新建连接，延迟和数据库负担都会很高。连接池维护有限数量的可复用连接：请求借用，完成后归还。

当前启动代码：

```go
connPool, err := pgxpool.New(context.Background(), config.DBSource)
store := db.NewStore(connPool)
```

一个容易忽略的官方语义是：`pgxpool.New` 返回时不保证已经成功建立连接。若希望启动即验证数据库可达，应调用 `Ping` 或立即 `Acquire`。当前项目没有 Ping，也没有在退出时 `connPool.Close()`；这属于后续生产化缺口。

事务开始后会占用池中的一条连接直到 commit/rollback，因此：

- 事务必须短；不要在事务中等用户输入、发 HTTP 或做慢邮件；
- 所有数据库调用都传递带超时/取消的 `context.Context`；
- 池不能无限放大，总连接上限应结合数据库 `max_connections`、应用副本数和后台任务计算。

### 8.2 当前几层抽象如何配合

sqlc 生成 `DBTX`：

```go
type DBTX interface {
    Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error)
    Query(context.Context, string, ...interface{}) (pgx.Rows, error)
    QueryRow(context.Context, string, ...interface{}) pgx.Row
}
```

连接池和事务对象都满足这组方法。因此同一套 `Queries` 既能直接在池上执行单条 SQL，也能绑定到某个 transaction：

```go
q := New(tx)
```

sqlc 生成 `Querier` 接口，包含全部单条查询。项目手写的 `Store` 再嵌入它，并增加复合事务：

```go
type Store interface {
    Querier
    TransferTx(...)
    CreateUserTx(...)
    VerifyEmailTx(...)
}
```

`SQLStore` 嵌入 `*Queries`，所以自动拥有全部 CRUD；它另外持有 `*pgxpool.Pool`，用于开启事务。`NewStore` 返回接口而不是具体类型，API 测试就能换成 gomock 生成的 `MockStore`。

这里的边界很清楚：

- `Queries/Querier`：一条查询一个方法，由 sqlc 生成；
- `SQLStore/Store`：跨多条查询的业务事务和便于 Mock 的依赖边界，由项目手写。

### 8.3 连接池应监控什么

pgxpool 的 `Stat()` 可提供连接池快照。工业监控至少关注：

- `AcquiredConns`：当前借出的连接数；
- `IdleConns`：空闲连接数；
- `MaxConns`：池上限；
- acquire 次数、累计等待时长；
- 因 context 取消的 acquire；
- 因最大生命周期/空闲时长销毁的连接数。

如果长期 `AcquiredConns ≈ MaxConns`，并且 acquire 等待、超时上升，可能是慢 SQL、长事务、连接泄漏或池容量不合理。不能只看到“把 MaxConns 加大”：所有服务副本总连接数可能反过来压垮数据库。

---

## 9. 事务与 ACID：一起成功只是第一层

### 9.1 为什么转账必须是一个事务

一笔项目内转账包含五个结果：

1. 新增一条 transfer；
2. 给转出账户新增负 entry；
3. 给转入账户新增正 entry；
4. 扣减转出余额；
5. 增加转入余额。

如果第三步后进程崩溃，而前三步已经各自提交，数据库会出现“有转账和流水，但余额没变”的半成品。事务把这五步包在同一个边界里。

当前 `execTx` 的结构是：

```go
tx, err := store.connPool.Begin(ctx)
q := New(tx)
err = fn(q)
if err != nil {
    tx.Rollback(ctx)
    return err
}
return tx.Commit(ctx)
```

闭包只能拿到绑定于 `tx` 的 `q`，从而降低误用池外查询的概率。任何一步返回错误都会 rollback；全部成功才 commit。

### 9.2 ACID 分别是什么意思

#### Atomicity，原子性

事务中的写入全部提交或全部回滚。它解决半完成状态，但只覆盖同一个数据库事务。PostgreSQL 事务不能自动原子包含 Redis、邮件或另一个数据库。

#### Consistency，一致性

提交前后都满足不变量，例如外键存在、唯一邮箱不重复、转账两边金额守恒。数据库不会自动猜出所有业务规则；一致性来自约束、事务代码和正确模型。当前缺少正金额、非负余额、不同账户等约束，所以“使用了事务”仍可能原子地提交一笔错误转账。

#### Isolation，隔离性

多个并发事务的中间状态不能随意互相干扰。隔离并非等同于“所有请求排队串行”，而是通过 MVCC、快照、锁和隔离级别定义可见性与冲突处理。

#### Durability，持久性

成功 commit 后，即使进程崩溃，数据库应依靠 WAL 等机制恢复已提交结果。但持久性不是“永不丢数据”的承诺：磁盘故障、错误删除、错误配置、灾难仍需备份、复制和恢复演练。

---

## 10. Read Committed、MVCC、行锁与 Lost Update

### 10.1 项目当前使用的隔离级别

`pgxpool.Begin(ctx)` 没传自定义 `TxOptions`，所以使用服务器默认事务特征；PostgreSQL 默认隔离级别是 **Read Committed**。

在 Read Committed 中，每条普通 `SELECT` 看到该语句开始前已经提交的数据。**同一事务中的两条 SELECT 可能看到不同快照**，因为中间可能有其他事务提交。这与 Repeatable Read 的事务级快照不同。

Read Committed 不是“不安全模式”。许多系统在它之上配合原子更新、唯一约束、显式行锁和条件写实现正确业务。关键是清楚自己要保护的不变量。

### 10.2 MVCC 与行锁并不矛盾

PostgreSQL 使用多版本并发控制（MVCC），普通读取通常不阻塞普通写入。更新同一行时仍需要冲突协调：`UPDATE` 会取得相应行锁；另一个事务试图更新同一行会等待前者结束，然后根据规则继续或报错。

项目还生成了：

```sql
SELECT * FROM accounts
WHERE id = $1
FOR NO KEY UPDATE;
```

它会显式锁住选中的行，直到事务结束。不过当前 `TransferTx` **没有调用** `GetAccountForUpdate`；实际余额同步依靠两条 `UPDATE ... balance = balance + amount` 自动取得的行锁。

### 10.3 Lost Update 是怎样发生的

假设余额 100，两个请求各扣 10，都这样写：

```text
事务 A：SELECT balance -> 100
事务 B：SELECT balance -> 100
事务 A：在 Go 中算出 90，UPDATE balance = 90
事务 B：在 Go 中也算出 90，UPDATE balance = 90
最终余额：90，少扣了一次
```

这叫丢失更新。问题在于“读旧值—应用计算—写固定新值”被拆成多个步骤。

项目改为单条原子 SQL：

```sql
UPDATE accounts
SET balance = balance + $1
WHERE id = $2
RETURNING *;
```

两个并发 `-10` 不会都把余额写成 90。后来的 UPDATE 等待并在可更新版本上执行表达式，最终是 80。

但不要过度概括：原子加减只能解决这种简单增量。如果要先检查币种、状态、限额、余额充足，再修改多行，仍需条件更新或显式锁定相关行。例如防止余额不足的一种单行写法是：

```sql
UPDATE accounts
SET balance = balance - $1
WHERE id = $2
  AND balance >= $1
RETURNING *;
```

返回零行意味着不存在或余额不足，还要进一步区分。涉及两账户统一锁序时，也可以先按 ID 排序 `SELECT ... FOR NO KEY UPDATE` 锁住两行，验证后再更新。

---

## 11. 死锁：不是数据库坏了，而是等待形成了环

### 11.1 两笔反向转账如何死锁

账户 1 和 2 同时反向转账：

```text
事务 A（1 -> 2）              事务 B（2 -> 1）
锁住账户 1                    锁住账户 2
等待账户 2                    等待账户 1
```

A 等 B，B 又等 A，等待图形成环。PostgreSQL 会检测死锁并中止其中一个事务，而不是永久卡住；哪一个被中止不应被业务依赖。

### 11.2 项目如何破坏等待环

`b6268f8` 将更新顺序改成按账户 ID 从小到大：

```go
if arg.FromAccountID < arg.ToAccountID {
    // 先小 ID，后大 ID
} else {
    // 即使业务方向相反，仍先小 ID，后大 ID
}
```

于是所有事务抢多把锁时都遵守同一全序，不会出现 A 先 1 后 2、B 先 2 后 1 的环。这正是 PostgreSQL 官方文档推荐的主要防线。

固定锁序会降低死锁概率，但工业系统仍必须准备重试，因为复杂系统中可能有其他表、触发器或代码路径形成死锁。通常仅对可识别的可重试错误重试，使用指数退避加随机抖动，并保证整个业务请求幂等。

### 11.3 当前并发测试证明了什么

`TestTransferTx` 启动 5 个 goroutine，从 account1 向 account2 各转 10：

- 每笔 transfer 和两条 entries 存在；
- 每个返回余额对应 1 到 5 次不同的累计变化；
- 最终 account1 减少 `5*10`；
- 最终 account2 增加 `5*10`。

`TestTransferTxDeadlock` 启动 10 个 goroutine，奇偶次数交替执行相反方向转账。每个方向次数相同，所以最终两个余额应回到初始值，并要求所有事务无错误。

这是很好的并发集成测试，因为普通顺序单元测试无法暴露锁序问题。但它仍不是完整证明：

- 金额固定且账户只有两个；
- 没测余额不足、0/负金额、同账户、币种不同；
- 没断言 transfer 数量与 entry 数量严格增加多少；
- 没测故障注入和回滚；
- 没对死锁 SQLSTATE 和重试逻辑做测试；
- 使用共享数据库且不清理数据，测试之间缺少强隔离。

---

## 12. SQLSTATE：不要解析数据库错误文本

数据库错误文本可能随版本、语言和上下文改变。PostgreSQL 为错误类别提供稳定的五字符 SQLSTATE。

当前 `db/sqlc/error.go` 定义：

```go
const (
    ForeignKeyViolation = "23503"
    UniqueViolation     = "23505"
)

var ErrRecordNotFound = pgx.ErrNoRows

func ErrorCode(err error) string {
    var pgErr *pgconn.PgError
    if errors.As(err, &pgErr) {
        return pgErr.Code
    }
    return ""
}
```

`errors.As` 能沿着 `%w` 包装的错误链寻找 `*pgconn.PgError`。API 再把：

- `23503` 外键冲突映射为业务可理解错误；
- `23505` 唯一冲突映射为“已存在”；
- `pgx.ErrNoRows` 用 `errors.Is` 识别为“未找到”。

工业代码通常还会关心：

- `23514`：CHECK violation；
- `40001`：serialization failure；
- `40P01`：deadlock detected。

不要把所有数据库错误原文直接返回客户端：原文可能泄露表名、约束名和内部结构。服务层应记录完整错误及 request/trace ID，对外返回稳定的领域错误。

当前 `ErrUniqueViolation` 是手工构造的 `PgError`，主要方便 Mock 测试返回一个能被 `ErrorCode` 识别的错误；真实数据库错误仍由 pgx 返回。

---

## 13. 部分更新：NULL、`sqlc.narg`、`COALESCE` 与 pgtype

### 13.1 “没传”不等于“传空字符串”

用户更新接口需要区分：

- 没提供 `full_name`：保留原值；
- 提供 `full_name = "Alice"`：改成 Alice；
- 提供空字符串：这是一个真实输入，应由业务校验决定拒绝还是接受。

如果 Go 参数只有 `string`，零值 `""` 无法表达“没提供”。SQL 的 NULL 很适合表达缺席。

### 13.2 项目的 SQL 链路

当前查询：

```sql
UPDATE users
SET
  hashed_password = COALESCE(sqlc.narg(hashed_password), hashed_password),
  password_changed_at = COALESCE(sqlc.narg(password_changed_at), password_changed_at),
  full_name = COALESCE(sqlc.narg(full_name), full_name),
  email = COALESCE(sqlc.narg(email), email),
  is_email_verified = COALESCE(sqlc.narg(is_email_verified), is_email_verified)
WHERE username = sqlc.arg(username)
RETURNING *;
```

`sqlc.narg` 强制该参数按 nullable 生成。pgx/v5 下生成：

```go
FullName          pgtype.Text
PasswordChangedAt pgtype.Timestamptz
IsEmailVerified   pgtype.Bool
```

以 `pgtype.Text` 为例：

```go
pgtype.Text{Valid: false}                 // 传 SQL NULL -> COALESCE 取旧列
pgtype.Text{String: "Alice", Valid: true} // 更新为 Alice
pgtype.Text{String: "", Valid: true}      // 确实更新为空字符串
```

这条链是：

```text
调用方是否提供字段
  -> pgtype.*.Valid
  -> SQL 参数为值或 NULL
  -> COALESCE(参数, 原列)
  -> 更新或保留
```

这里 `COALESCE` 的局限也要知道：它把 SQL NULL 专门解释为“不更新”，所以无法用同一写法把一个允许 NULL 的列主动设置为 NULL。本项目相关列是 `NOT NULL`，暂时没有这个需求。若业务列允许 NULL，一般要额外的 `set_field` 布尔参数、动态 SQL，或明确的 patch 操作表示三态。

---

## 14. 隔离级别如何选择，而不是越高越好

PostgreSQL 实际可用的主要级别是 Read Committed、Repeatable Read、Serializable；Read Uncommitted 在 PostgreSQL 中按 Read Committed 处理。

### Read Committed

吞吐和并发性好，每条语句一个快照。适合大量常规业务，但复合“先读后判断再写”必须用条件写、行锁或约束保护。当前 simplebank 使用它。

### Repeatable Read

事务看到稳定快照，避免不可重复读；并发更新可能导致事务失败，需要重试。它仍不应被简单理解为“任何业务都自动串行正确”。

### Serializable

目标是让成功提交的事务效果等价于某种串行顺序。PostgreSQL 可能中止出现危险依赖的事务并返回 `40001`，应用必须重试整个事务。它提高了正确性保障，但带来更多失败重试、监控和容量成本，不是给配置改一个单词就结束。

选择原则：先写出不变量和并发场景，再选成本最低、能证明正确的方案。simplebank 的两账户加减在 Read Committed + 原子 UPDATE + 固定锁序下可以保证并发增量不丢失；但余额不足、幂等和更复杂账本规则尚未完成。

无论级别如何，都应：

- 保持事务短小；
- 设置合理的 statement、lock 和请求超时；
- 监控 deadlock、serialization retry、锁等待和慢查询；
- 对可重试事务设置次数上限、退避和幂等；
- 不在数据库事务中等待外部网络服务。

---

## 15. 工业界的资金模型：当前实现只是起点

### 15.1 余额快照与不可变账本

当前模型同时维护：

- `accounts.balance`：快速读取的余额快照；
- `entries`：余额变化流水。

这是常见起点，但存在“双份事实”的一致性风险。当前公开的 `UpdateAccount` 可以直接把余额设置成任意值，不会生成 entry；数据库也不能证明余额等于历史 entries 总和。

更严谨的资金系统通常采用不可变复式账本：

- 一次业务交易有全局 transaction/journal ID；
- 每笔至少有成对 posting，借贷总额为零；
- 已入账记录不原地 UPDATE/DELETE，纠错用反向分录；
- 可用余额、账面余额按账本计算或作为受控缓存维护；
- 定期执行账本—余额对账，差异触发告警；
- 数据库角色禁止普通应用直接任意改余额。

这不意味着所有小系统一开始就要建成银行核心。但你必须知道当前 `entries + balance` 是教学简化，不应仅因有 transaction 就宣称达到金融级审计。

### 15.2 幂等是转账 API 必备规则

客户端可能因为超时重试同一请求。如果第一次其实已经提交，只是响应丢了，第二次再执行会重复扣款。

常见设计是让客户端提供 `idempotency_key`，并在服务端数据库中建立唯一约束：

```sql
ALTER TABLE transfers ADD COLUMN idempotency_key uuid;
CREATE UNIQUE INDEX transfers_idempotency_key_key
ON transfers(idempotency_key);
```

真正实现还要定义 key 的作用域、请求参数指纹、处理中状态、原响应保存、过期策略和并发争抢行为。同一个 key 配不同金额应报冲突，而不是静默返回旧结果。

当前 transfers 没有幂等键，因此网络重试可能产生两笔合法但业务上重复的转账。

### 15.3 余额不足和币种校验必须在锁内成立

“请求进入时查到余额足够”并不够。两个并发请求都可能读到相同余额后一起扣款。余额检查必须与扣减形成原子条件，或在事务中锁住后检查。

当前 TransferTx 还没有：

- `amount > 0` 数据库 CHECK；
- `from_account_id <> to_account_id` CHECK；
- `balance >= 0` 或条件扣减；
- 两账户币种必须相同的事务内验证；
- 请求幂等键；
- 领域状态（冻结、关闭、限额、合规拦截）。

因此它很好地教学“事务与死锁”，但不能直接作为真实银行转账核心。

---

## 16. 当前数据库测试的隔离问题

`db/sqlc/main_test.go` 从项目配置读取真实连接串，创建一个全局 pool 和 `testStore`。各测试用随机数据往同一个 `simple_bank` 数据库持续插入，未清理，pool 也未关闭。

风险包括：

- 本地开发数据和测试数据混在一起；
- 多次测试后数据持续膨胀；
- 随机值并非数学上绝不会冲突，唯一约束可能造成偶发失败；
- 测试顺序、并行进程或 CI shard 可能互相影响；
- 测试失败时留下半套夹具，后续定位困难。

工业实践可选择：

1. 每次测试任务创建独立临时数据库，迁移后运行，结束销毁；
2. 每个测试 suite 使用唯一 schema 和 `search_path`；
3. 单条查询测试在事务中运行，测试结束 rollback；
4. 用 Testcontainers 启动版本固定的 PostgreSQL 容器；
5. 并发/提交语义测试不能简单靠外层 rollback，应使用独立数据库并显式清理。

还应让测试连接配置与开发/生产彻底分离，并在启动时校验数据库名带有 `_test` 等安全标识，避免清理脚本误伤真实数据。

---

## 17. 可执行学习实验

以下实验建议在专用测试数据库进行。先启动 PostgreSQL 并执行 Migration：

```bash
docker compose up -d postgres
make migrateup
```

不要在保存重要数据的数据库上做 drop、deadlock 或故障实验。

### 实验 1：观察表、约束和索引

进入 psql 后执行：

```sql
\dt
\d users
\d accounts
\d entries
\d transfers
\d sessions
\d verify_emails
```

目标：找出哪些索引来自主键/唯一约束，哪些来自 Migration 中的显式 `CREATE INDEX`；确认 sessions 和 verify_emails 的 username 外键没有自动生成引用侧索引。

### 实验 2：亲眼看到外键和唯一约束

用事务包住实验，最后回滚：

```sql
BEGIN;

INSERT INTO accounts(owner, balance, currency)
VALUES ('user-does-not-exist', 1000, 'USD');

-- 上一句应报外键错误。事务进入失败状态后先：
ROLLBACK;
```

然后插入一个专用用户和重复账户：

```sql
BEGIN;
INSERT INTO users(username, hashed_password, full_name, email)
VALUES ('learn_db', 'not-a-real-password-hash', 'Learner', 'learn-db@example.com');

INSERT INTO accounts(owner, balance, currency)
VALUES ('learn_db', 1000, 'USD');

INSERT INTO accounts(owner, balance, currency)
VALUES ('learn_db', 2000, 'USD');
-- 应命中 (owner, currency) 唯一约束
ROLLBACK;
```

观察错误中的 SQLSTATE：外键 `23503`，唯一 `23505`。

### 实验 3：证明浮点不适合作为精确金额

写一个临时 Go 程序或测试：

```go
var x float64
for i := 0; i < 10; i++ {
    x += 0.1
}
fmt.Printf("%.20f\n", x)
```

再用 `int64` 累加 10 次 10 分，观察结果严格等于 100 分。目标不是记住某个打印值，而是理解“二进制浮点是近似表示，整数最小单位是精确表示”。

### 实验 4：复现 Lost Update

创建独立演示表：

```sql
CREATE TABLE IF NOT EXISTS balance_demo (
  id bigint PRIMARY KEY,
  balance bigint NOT NULL
);
INSERT INTO balance_demo(id, balance) VALUES (1, 100)
ON CONFLICT (id) DO UPDATE SET balance = EXCLUDED.balance;
```

打开两个 psql 窗口 A/B：

```sql
-- A
BEGIN;
SELECT balance FROM balance_demo WHERE id = 1; -- 100
```

```sql
-- B
BEGIN;
SELECT balance FROM balance_demo WHERE id = 1; -- 100
```

然后 A 执行 `UPDATE ... SET balance = 90` 并提交；B 也执行固定值 90 并提交。最终为 90，而不是 80。

重置到 100，再让 A/B 都执行：

```sql
UPDATE balance_demo SET balance = balance - 10 WHERE id = 1;
```

最终应为 80。解释第二种为什么不会丢一次扣减。

### 实验 5：手工制造并观察死锁

```sql
CREATE TABLE IF NOT EXISTS lock_demo (
  id bigint PRIMARY KEY,
  value bigint NOT NULL
);
INSERT INTO lock_demo VALUES (1, 0), (2, 0)
ON CONFLICT (id) DO UPDATE SET value = 0;
```

窗口 A：

```sql
BEGIN;
UPDATE lock_demo SET value = value + 1 WHERE id = 1;
-- 暂停，等 B 锁住 id=2
UPDATE lock_demo SET value = value + 1 WHERE id = 2;
```

窗口 B：

```sql
BEGIN;
UPDATE lock_demo SET value = value + 1 WHERE id = 2;
-- 暂停，等 A 锁住 id=1
UPDATE lock_demo SET value = value + 1 WHERE id = 1;
```

PostgreSQL 应中止一方并报告 deadlock detected（SQLSTATE `40P01`）。然后重做实验，让 A/B 都先更新 id=1 再更新 id=2；第二个事务会等待，但不形成环。

### 实验 6：运行项目并发测试

```bash
go test -v ./db/sqlc -run 'TestTransferTx|TestTransferTxDeadlock' -count=10
```

不要只看绿灯。逐行阅读 `store_test.go`，回答：goroutine 数量是多少、channel 传了什么、最终余额公式是什么、双向转账为什么能回到原值。

### 实验 7：给转账加约束（建议在学习分支完成）

```bash
make new_migration name=add_transfer_constraints
```

在新 up 文件中加入正金额和不同账户 CHECK，在 down 文件中按约束名删除。然后：

```bash
make migrateup1
make sqlc
```

补充集成测试，分别尝试 amount=0、amount=-1、同账户。不要修改已经执行过的 `000001`；这正是练习 Migration 版本化。

### 实验 8：检查查询计划和池状态

先给测试库生成足够多但可清理的数据，再对 `ListTransfers` 运行：

```sql
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM transfers
WHERE from_account_id = 1 OR to_account_id = 1
ORDER BY id
LIMIT 20;
```

记录顺序扫描、索引扫描或 bitmap scan，以及 estimated rows 与 actual rows。小表选择顺序扫描是合理行为，不代表索引失效。

再在临时诊断代码中打印 `connPool.Stat()` 的 acquired、idle、max 和 acquire duration，运行并发测试时观察变化。完成实验后删除诊断代码，不要把临时打印留在业务代码。

---

## 18. 自测题

先独立回答，再看下一节。

1. 为什么在 API 校验 `amount > 0` 后，数据库仍值得加 CHECK？
2. `PRIMARY KEY`、`UNIQUE`、普通索引分别主要表达什么？
3. PostgreSQL 声明外键后，会自动在外键引用侧创建索引吗？
4. 为什么金额不应使用 `float64`？`bigint` 方案又需要什么业务约定？
5. `timestamptz` 会保留用户原始输入的时区名吗？
6. `RETURNING *` 相比 INSERT 后再 SELECT 有什么价值？
7. sqlc 能替你保证转账业务正确吗？
8. `Querier` 和 `Store` 在本项目分别负责什么？
9. Read Committed 下，同一事务两次 SELECT 一定看到同一快照吗？
10. `balance = balance + amount` 为什么比“读到 Go、计算、写回固定值”更安全？
11. 死锁的等待环如何形成？固定 ID 顺序破坏了什么？
12. 有事务为什么还可能允许余额变成负数？
13. `pgtype.Text{Valid:false}` 在当前 UpdateUser 中意味着什么？
14. SQLSTATE 比解析错误字符串好在哪里？
15. 为什么客户端超时重试可能造成重复扣款？
16. 为什么生产 Migration 常用 expand–migrate–contract？
17. 为什么不能通过无限调高 `MaxConns` 解决连接池等待？
18. 当前 entries 和 balance 为什么仍可能不一致？

---

## 19. 参考答案

1. API 可能有 Bug，也可能有后台脚本、其他服务和并发竞争；CHECK 是所有写入口的最后防线。
2. 主键标识行且非空唯一；唯一约束表达业务不可重复；普通索引主要优化查询。主键/唯一在 PostgreSQL 中会由唯一索引支撑。
3. 不会自动为引用侧建索引；被引用侧因主键/唯一已有索引。引用侧是否建、按什么列序建要根据查询决定。
4. 二进制浮点不能精确表示许多十进制金额。bigint 要明确它代表哪个币种的何种最小单位，并处理精度、舍入和溢出。
5. 不会。它保存统一时间点并按会话时区输出；原始时区名需另存。
6. 一次往返就能拿到数据库生成/最终的整行，避免额外查询和竞争窗口。
7. 不能。sqlc 保证查询解析和 Go 类型生成，不证明业务不变量、索引效率、锁序和幂等。
8. Querier 是 sqlc 生成的单条查询接口；Store 嵌入它并增加手写的多语句事务，也作为 Mock 边界。
9. 不一定。Read Committed 通常每条语句获得新快照。
10. 它把读取当前值和增量更新放进同一条 UPDATE，由数据库在行锁协调下执行，避免两个调用都基于同一旧值写固定结果。
11. A 持锁 1 等锁 2，B 持锁 2 等锁 1。所有事务按统一 ID 顺序加锁后，不能形成相反的循环等待。
12. 事务只保证一组操作原子，不会发明业务规则；当前没有余额 CHECK、条件扣减或锁内余额校验。
13. 参数编码为 SQL NULL，COALESCE 选择旧列，因此不更新该字段。
14. SQLSTATE 是稳定的机器可读类别，不受错误文本措辞和语言影响。
15. 第一次可能已经 commit，只是响应丢失；没有幂等键时第二次会成为另一笔合法交易。
16. 滚动发布中新旧实例会共存；分阶段扩展、迁移、收缩能保持相邻版本兼容并降低锁表/回滚风险。
17. 数据库总连接资源有限；副本数乘每池上限可能耗尽数据库连接和内存。应先定位慢 SQL、长事务或泄漏，再做容量设计。
18. 当前允许直接 `UpdateAccount` 改余额而不生成 entry，且没有数据库规则证明 entries 总和与 balance 相等，也没有完整复式账本和对账机制。

---

## 20. 掌握清单

当你可以不看答案完成下列事项，才算真正完成第二阶段：

- [ ] 能画出六张表及所有外键方向，解释每张表保存的事实。
- [ ] 能区分主键、外键、唯一约束、CHECK 与普通索引。
- [ ] 能解释索引为何加速读、拖慢写，并用 `EXPLAIN (ANALYZE, BUFFERS)` 验证。
- [ ] 能解释 bigint 最小单位方案、浮点风险、numeric 的适用场景。
- [ ] 能准确说明 `timestamptz` 的存储和显示语义。
- [ ] 能新建一对 up/down Migration，并知道生产 down 可能丢数据。
- [ ] 能手写参数化 CRUD、分页与 `RETURNING`。
- [ ] 能从 SQL 注解预测 sqlc 生成的方法签名，不手改生成文件。
- [ ] 能解释 pgx、pgxpool、DBTX、Queries、Querier、SQLStore、Store 的关系。
- [ ] 能写出 begin/rollback/commit 事务模板，并逐项解释 ACID。
- [ ] 能解释 PostgreSQL 默认 Read Committed 的“每条语句快照”。
- [ ] 能现场演示 Lost Update，并用原子 UPDATE 修复简单增量更新。
- [ ] 能画出双向转账死锁等待图，并解释固定 ID 锁序。
- [ ] 能写 goroutine + channel 的并发集成测试并验证最终不变量。
- [ ] 能用 SQLSTATE 分类外键、唯一、CHECK、序列化失败和死锁错误。
- [ ] 能解释 `sqlc.narg -> pgtype.Valid -> SQL NULL -> COALESCE` 全链路。
- [ ] 能指出当前项目的正金额、余额不足、同账户、币种、幂等和测试隔离缺口。
- [ ] 能说明工业界为什么采用不可变账本、余额快照、对账和幂等键。
- [ ] 能设计 expand–migrate–contract 发布流程和连接池核心监控指标。

---

## 21. 事实核对来源

本章的项目事实以当前仓库和上述 Git 提交为准；通用技术语义以官方资料为主：

- [PostgreSQL：事务隔离](https://www.postgresql.org/docs/current/transaction-iso.html)
- [PostgreSQL：显式锁、行锁与死锁](https://www.postgresql.org/docs/current/explicit-locking.html)
- [PostgreSQL：约束](https://www.postgresql.org/docs/current/ddl-constraints.html)
- [PostgreSQL：数值类型](https://www.postgresql.org/docs/current/datatype-numeric.html)
- [PostgreSQL：日期与时间类型](https://www.postgresql.org/docs/current/datatype-datetime.html)
- [PostgreSQL：索引类型](https://www.postgresql.org/docs/current/indexes-types.html)
- [sqlc：查询注解](https://docs.sqlc.dev/en/latest/reference/query-annotations.html)
- [sqlc：命名参数与 `sqlc.narg`](https://docs.sqlc.dev/en/latest/howto/named_parameters.html)
- [pgxpool 官方 Go 文档](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool)
- [golang-migrate 官方仓库](https://github.com/golang-migrate/migrate)

读完后的最佳下一步不是继续背概念，而是完成实验 4、5、7：亲眼看到丢失更新，亲手制造死锁，再通过一个新 Migration 把业务规则真正放进数据库。那一刻，“事务正确性”才会从词汇变成你的工程能力。
