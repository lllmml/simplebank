# 第 7 阶段：把“能运行”变成“可安全运营”——SimpleBank 生产化教程

> 面向第一次接触生产后端的学习者。本章不修改项目配置，也不把课程目录中尚未落地的内容说成仓库事实。文中出现的配置和命令都是教学示例；所有凭据都使用占位符，绝不复述仓库中已经提交的秘密。

## 0. 先建立一个正确认识：生产化不是“部署到服务器”

在本地运行成功，只能证明程序在某一台机器、某一组依赖、某一个时间点上走通了快乐路径。生产系统还必须面对：机器重启、依赖变慢、网络分区、流量尖峰、错误配置、版本回滚、数据库扩容、密钥泄漏、操作失误和凌晨告警。

因此，“生产就绪”不是一个布尔值，也不是装上 Docker 或 Kubernetes 就完成。它是一组持续维护的能力：

1. **可配置**：同一份代码能安全地运行在开发、测试、预发布和生产环境。
2. **可发布**：构建物可重复、可追踪，数据库和应用可以兼容地演进。
3. **可观察**：出问题时能从日志、指标和链路定位，而不是靠猜。
4. **可恢复**：实例、依赖和发布失败时，系统能降级、退出、回滚或恢复数据。
5. **可保护**：秘密、网络、依赖和供应链都有边界。
6. **可运营**：有 SLO、告警、值班手册、备份恢复演练和容量计划。

对银行类系统还要再加一条：**正确性优先于表面的可用性**。一次转账超时，客户端不知道是“没执行”还是“执行成功但响应丢了”。如果客户端盲目重试，可能重复扣款。因此生产化不能只考虑“接口能不能返回 200”，还必须考虑幂等、审计、一致性和不确定结果。

本章先讲通用原理，再逐项回到 SimpleBank 当前分支。阅读仓库事实时请始终区分三个层次：

- **README 课程目录说将会讲什么**；
- **Git 历史实际提交过什么**；
- **当前 `ft/RABC` 分支最终保留了什么**。

课程目录不是实现证明。仓库中没有代码，就不能因为 README 有一节课标题而说项目已经具备该能力。

---

## 1. 配置、环境分层与秘密管理

### 1.1 什么是配置

配置是“随部署环境变化，而业务代码不应该变化”的值，例如：

- PostgreSQL、Redis 的地址；
- HTTP、gRPC 监听地址；
- Token 密钥和有效期；
- SMTP 服务地址与凭据；
- 日志级别、功能开关、第三方服务端点。

十二要素应用方法强调把配置与代码分离，并把数据库、Redis、SMTP 等网络服务当作可替换的“附加资源”。核心目的不是迷信环境变量，而是让一份构建物在不同环境中只通过配置连接不同资源。官方概念可参阅 [The Twelve-Factor App](https://12factor.net/) 和 [Config](https://12factor.net/config)。

### 1.2 配置不等于秘密

所有秘密都是配置，但配置不全是秘密：

| 类型 | 例子 | 是否可进入 Git |
|---|---|---|
| 普通配置 | 监听端口、日志级别 | 通常可以 |
| 敏感配置 | 内网拓扑、客户标识 | 应限制暴露 |
| 秘密 | 数据库口令、Token 密钥、SMTP 凭据 | 不可以 |

环境变量也不是保险箱。它只是注入渠道，仍可能在进程信息、崩溃转储、部署清单、CI 日志或调试输出中泄漏。工业界通常把秘密保存到云 Secret Manager、Vault 或平台 Secret 中，以短期身份读取；权限遵循最小化原则，并保留访问审计。

### 1.3 环境分层

常见环境如下：

- **local**：开发者个人环境，可以使用 Compose、假邮件服务和测试数据；
- **test/CI**：每次运行尽量干净、可重复，不依赖个人机器；
- **staging**：拓扑尽量接近生产，但使用隔离账户和脱敏数据；
- **production**：真实流量和数据，变更必须审计、可回滚。

不要用大量 `if production { ... }` 让各环境走不同代码路径。更好的做法是同一个二进制、同一套协议，通过资源地址、权限和功能开关控制差异。预发布环境若与生产差别太大，就无法暴露生产问题。

### 1.4 回到 SimpleBank

[util/config.go](../../util/config.go) 定义 `Config`，使用 Viper 读取 `app.env` 并允许环境变量覆盖。这是配置外置的起点，但当前实现有几个边界：

1. `LoadConfig` 总会调用 `ReadInConfig`。即使生产环境已经提供全部环境变量，只要 `app.env` 不存在，函数仍会返回错误。也就是说，它还不是严格的“环境变量即可启动”。
2. 当前没有对关键值做启动校验。例如地址为空、Token 密钥长度错误、Duration 为零，可能到更深层才失败。
3. [Dockerfile](../../Dockerfile) 把 `app.env` 复制进最终镜像，使“构建物”和“某套环境配置”绑定，还把秘密带进镜像层。
4. `app.env` 已经被 Git 跟踪，且包含数据库、Token、邮箱相关秘密。这里必须把它们视为**已经暴露**，即使仓库从未公开，也不能只删文件后继续使用。

本教程绝不展示这些值。正确的轮换流程是：

1. **控制影响面**：盘点秘密出现在哪些提交、镜像、CI 日志、开发机和部署环境，限制仓库与镜像访问。
2. **先换秘密，不要先改历史**：在各服务端创建新凭据；数据库尽量用新用户或短暂双凭据窗口，SMTP 创建新应用凭据。
3. **处理 Token 特性**：当前 PASETO 使用单个对称密钥。直接更换会让旧 Token 全部失效。要么明确接受强制重新登录，要么先实现带 key ID 的密钥环，在过渡期“新密钥签发、旧新密钥均可验证”。
4. **通过秘密管理器部署新版本**，验证健康状态、登录、续期和邮件链路。
5. **撤销旧凭据**，检查是否仍有旧凭据访问。
6. **最后处理 Git 历史**：必要时重写历史、清理制品并协调所有协作者重新同步。重写历史不能替代轮换，因为已经复制出去的秘密收不回来。
7. 增加 secret scanning、提交前扫描和代码评审规则，防止复发。

还应提供一个只含字段名和安全示例的 `app.env.example`，但本章按任务要求不会实际创建它。开发和测试默认应使用假 SMTP、捕获邮件的本地服务或 Mock；不应让一次普通测试默认向真实 Gmail 发信。当前 [mail/sender_test.go](../../mail/sender_test.go) 的真实发信测试只有在非 `-short` 模式才运行，而 `make test` 带 `-short`，这是避免 CI 副作用的一道保护，但仍需要明确的集成测试环境和专用测试账户。

---

## 2. Docker：镜像、容器与可重复构建

### 2.1 镜像和容器是什么

镜像是只读文件系统层和启动元数据的集合；容器是镜像运行后的进程与隔离环境。镜像不是虚拟机快照，容器也不应保存必须长期存在的业务数据。应用实例应该尽量无状态，把数据交给 PostgreSQL、对象存储等持久系统。

多阶段构建把“编译环境”和“运行环境”分开：第一阶段含 Go 编译器和源码，第二阶段只复制二进制与运行所需文件。好处是运行镜像更小、攻击面更窄，也避免把编译工具带入生产。

### 2.2 `EXPOSE` 与端口映射

`EXPOSE 8080` 只是镜像元数据，告诉使用者容器预期监听哪个端口；它不会自动开放端口。Compose 中 `"8081:8081"` 才表示把宿主机 8081 映射到容器 8081。应用最终监听什么仍由 `HTTP_SERVER_ADDRESS` 决定。

因此，`EXPOSE` 写错不一定让程序立刻不能访问，但会误导维护者、镜像扫描器和自动化平台。

### 2.3 回到 SimpleBank 的 Dockerfile

提交 `3a955a6` 首次增加多阶段 [Dockerfile](../../Dockerfile)，这是正确的教学起点；`116fc3e` 加入 Compose、启动脚本和等待脚本；`ffcfd13` 又把迁移从 `start.sh` 移入 Go 的启动流程。

当前 Dockerfile 的事实是：

- Builder 使用 Go Alpine 镜像编译 `main.go`；Runtime 使用 Alpine；
- 最终镜像复制二进制、`app.env`、`start.sh`、`wait-for.sh` 和迁移文件；
- 声明 `EXPOSE 8080`，但当前 HTTP 配置和 Compose 映射为 8081，存在 **8080 与 8081 不一致**；
- 仓库没有 `.dockerignore`，`COPY . .` 会把整个构建上下文送入构建器，既影响缓存和速度，也增加不必要文件进入构建阶段的机会；
- 没有声明非 root 用户、镜像健康检查、构建来源标签或制品签名；
- 基础镜像使用版本标签而非内容摘要，标签可读性好，但无法单独提供完全不可变的供应链身份。

工业实践不是机械追求“最小镜像”。需要同时考虑：CA 证书、时区、动态链接依赖、调试能力、漏洞修复频率。可采用 distroless、scratch 或精简发行版，但必须先验证 TLS、DNS 和运行依赖。构建阶段通常先复制 `go.mod/go.sum` 下载依赖，再复制源码，以提高缓存命中；同时使用 `CGO_ENABLED`、目标架构和 `-trimpath` 等明确构建参数，并为二进制注入版本、提交 SHA 和构建时间。

一个生产镜像还应做到：

- 不包含源代码、Git 元数据、测试凭据和环境专属配置；
- 以非 root 用户运行；
- 在 CI 中生成 SBOM、扫描系统包和 Go 依赖；
- 使用唯一、不可变标签，例如提交 SHA，而不是只覆盖 `latest`；
- 推送后签名，在部署侧验证来源；
- 定期重建，即使业务代码未变，也要吸收基础镜像安全修复。

---

## 3. Compose、服务依赖与健康检查

### 3.1 “进程启动”不等于“服务就绪”

PostgreSQL 容器进入 running 状态时，数据库可能还在初始化；端口可连接时，目标数据库、用户和 schema 也未必可用。Redis、SMTP 同理。因此要区分：

- **startup**：进程是否完成启动；
- **liveness**：进程是否陷入无法自愈的状态，需要重启；
- **readiness**：当前实例是否应该接收新流量。

Liveness 不宜深度检查所有依赖。假设 PostgreSQL 故障，如果每个应用实例 liveness 都失败，平台会不停重启全部实例，反而制造雪崩。通常 liveness 只检查进程自身事件循环；readiness 可以检查必要依赖，但应有短超时、缓存和明确降级策略。Kubernetes 对三种探针的正式定义可参阅 [Liveness, Readiness, and Startup Probes](https://kubernetes.io/docs/concepts/workloads/pods/probes/)。

### 3.2 `depends_on` 和 `wait-for.sh` 的边界

Compose 的 `depends_on` 默认解决创建顺序，不自动保证依赖已经可服务。官方文档也明确区分 running 和 ready；若要等待就绪，应为依赖配置 `healthcheck` 并使用 `service_healthy` 条件，参阅 [Docker Compose startup order](https://docs.docker.com/compose/how-tos/startup-order/)。

当前 [docker-compose.yaml](../../docker-compose.yaml) 有 PostgreSQL、Redis 和 API 三个服务。它做到了：

- PostgreSQL 映射本机端口，并使用命名卷持久化数据；
- Redis 与 API 在默认 Compose 网络内可通过服务名互访；
- API 映射 HTTP 8081 和 gRPC 9090；
- API 用 [wait-for.sh](../../wait-for.sh) 等待 PostgreSQL TCP 端口。

但也存在清晰边界：

1. API 的 `depends_on` 只有 PostgreSQL，**没有 Redis**。
2. `wait-for.sh` 只等待 `postgres:5432`，**不等待 Redis**。
3. TCP 端口成功只证明能建立连接，不证明迁移完成或业务查询成功。
4. PostgreSQL 和 Redis 没有 Compose `healthcheck`；API 也没有健康检查。
5. Compose 中的数据库开发口令是明文，适合教学本地环境的便利性，不是生产秘密管理方案。
6. 命名卷能跨容器重建保存本地数据，但它不是生产级备份、高可用或异地容灾。

[start.sh](../../start.sh) 当前只打印消息并 `exec "$@"`。这里的 `exec` 很重要：它让 Go 进程取代 shell 成为容器主进程，可以直接收到平台发送的信号。可惜当前 Go 代码还没有处理终止信号，所以“正确传递信号”的基础有了，完整优雅退出尚未实现。

### 3.3 应用启动时必须主动验证什么

[main.go](../../main.go) 使用 `pgxpool.New` 创建连接池，但没有显式 `Ping`，也没有 `/livez`、`/readyz` 或 gRPC Health Checking Service。启动迁移会实际访问数据库，因此数据库完全不可达时通常会在迁移处失败；不过这不能替代持续 readiness，也不能覆盖 Redis 和 SMTP。

合理的启动策略是：

1. 解析并校验配置，错误要指出字段但不能打印秘密值；
2. 为数据库和 Redis 建立带截止时间的连接验证；
3. 明确哪些依赖是硬依赖，哪些可以降级；
4. 完成初始化后才把 readiness 置为成功；
5. 邮件通常可通过队列异步处理，不应因 SMTP 暂时故障阻止 API 启动，但应让 Worker 指标和告警体现积压。

---

## 4. CI/CD：自动化验证不等于自动化发布

### 4.1 CI 与 CD

- **CI（持续集成）**：每次变更自动构建、测试、静态检查，让主分支保持可集成。
- **Continuous Delivery（持续交付）**：任何通过验证的版本都可被安全发布，但生产发布可能需要审批。
- **Continuous Deployment（持续部署）**：通过流水线的变更自动进入生产。

流水线的价值不仅是“替人执行命令”，还在于创建可审计证据：哪一个提交、用什么工具版本、通过了哪些测试、生成了哪个镜像、部署到哪里。

### 4.2 Git 历史中的 CI 演进

指定提交展示了一个真实的迭代过程：

- `295accf`：建立最小 GitHub Actions 工作流；
- `cc88600`：加入 PostgreSQL service，并在测试前执行迁移；
- `d29731f`：尝试安装 golang-migrate，但 YAML 缩进和解压文件名尚有问题；
- `325c9c0`：修复 `run` 块缩进；
- `7565ae8`：修正移动的二进制名称；
- `de09c2c`：显式映射 PostgreSQL 5432 端口。

这说明 CI 本身也是代码，需要小步修改和验证。当前 [.github/workflows/ci.yml](../../.github/workflows/ci.yml) 会在 push 到 `main` 或针对 `main` 的 PR 上运行，使用 PostgreSQL 18 service，安装固定版本的 migrate，执行迁移和 `make test`。

当前 CI 的边界也必须说清：

- 当前工作分支是 `ft/RABC`；直接 push 到它不会满足 `push.branches: main`，只有向 `main` 发 PR 才触发 `pull_request` 流程。
- `make test` 是 `go test -v -cover -short ./...`，真实 Gmail 测试会被跳过。
- PostgreSQL 集成测试有环境，但没有 Redis service，因此没有覆盖完整异步链路。
- 没有单独的 `gofmt`、`go vet`、race detector、静态分析、漏洞扫描、生成代码一致性检查、镜像构建测试或端到端测试。
- migrate 安装通过网络下载压缩包后直接使用，没有展示校验 checksum/signature。
- `actions/setup-go@v4`、`actions/checkout@v4` 使用大版本标签，维护方便但不是不可变引用。高安全组织可把第三方 Action 固定到完整提交 SHA，并由自动化工具更新；GitHub 也提供强制完整 SHA 的仓库策略。
- 工作流只做 CI 测试，没有镜像推送、环境部署、审批、金丝雀、回滚或部署验证，所以不能称为完整 CD。

版本号固定并不天然安全，永不升级会积累漏洞；永远追随浮动最新版又会降低可重复性。工业实践是：关键工具和依赖有明确版本，自动提出升级 PR，流水线验证后受控合并；构建输出记录依赖清单与来源。

### 4.3 当前测试体系能证明什么

测试通过的含义必须与测试环境绑定，不能把“有测试”直接翻译成“生产可靠”。当前仓库大致有以下边界：

- `api/*_test.go` 和 `gapi/*_test.go` 大量使用 gomock，能快速验证 Handler/RPC 的分支、返回码和依赖调用，但 Mock 不会暴露真实 SQL、网络、序列化或连接池问题。
- `db/sqlc/*_test.go` 连接真实 PostgreSQL，适合验证生成 SQL、约束和事务；`db/sqlc/main_test.go` 同样通过 `LoadConfig("../..")` 依赖仓库配置文件，并未在测试结束前显式关闭连接池。
- `mail/sender_test.go` 是会访问外部 Gmail 并产生真实副作用的集成测试，`testing.Short()` 时跳过。它不应使用个人邮箱和生产凭据作为默认 CI 验证。
- Worker 没有一套由 CI 启动真实 Redis、验证入队—消费—重试—死信的完整集成测试。
- 当前覆盖率是测试命令的输出，不是质量目标；高覆盖率也不能替代并发、迁移兼容、负载、故障与恢复测试。

工业流水线通常分层：单元测试必须快且稳定；PostgreSQL/Redis 集成测试使用一次性隔离环境；外部邮件使用沙箱或契约测试；少量端到端测试验证打包后的真实服务。若某个测试不稳定，应修复隔离和确定性，而不是习惯性重跑到绿色。

### 4.4 发布与回滚策略

常见发布方式：

- **Rolling update**：逐批替换，成本低，但新旧版本会共存；
- **Blue/Green**：准备完整新环境后切流，回滚快，成本高；
- **Canary**：先给少量实例或流量，观察 SLO 和业务指标后逐步扩大；
- **Feature flag**：代码已部署但功能按用户或比例启用，需管理旗标生命周期。

回滚不等于 `git revert`。应用镜像可以回滚，数据库数据和破坏性 schema 变更往往不可逆。因此每次发布必须记录：镜像摘要、配置版本、迁移版本、发布人、开始/结束时间、关键指标，以及经过演练的回退条件。

---

## 5. 数据库迁移：为什么“启动时自动 Up”有多副本风险

### 5.1 迁移是发布协议

数据库 schema 被多个应用版本共同使用。滚动发布期间，旧版本和新版本会同时访问数据库，因此迁移不能只满足新代码，还必须考虑旧代码。

`ffcfd13` 把 migration URL 加入配置，并在 [main.go](../../main.go) 启动时调用 `migration.Up()`；同时从 `start.sh` 移除了外部 migrate 命令。优点是开发者不容易忘记迁移，单实例教学环境启动方便。

生产多副本下，每个新实例都会执行同一段迁移。迁移工具可能通过数据库锁将执行串行化，但仍有以下风险：

- 多个实例争抢迁移锁，启动时间和发布结果更难预测；
- 长 DDL 锁住业务表，所有新实例可能卡住，旧实例也受影响；
- 应用运行身份必须拥有 DDL 权限，违反最小权限；
- 某个实例迁移失败会直接 `Fatal`，平台重启后反复尝试；
- schema 尚未兼容旧程序时，滚动发布中旧实例立即报错；
- 应用启动、健康检查和迁移生命周期被耦合。

更常见的做法是将迁移作为**一次性、受控、可观察的发布步骤**：由专用 CI job、Kubernetes Job 或部署平台任务使用单独的迁移身份运行；成功后再滚动应用。是否允许自动迁移取决于组织风险，但“谁执行、只执行一次、失败如何停止发布”必须清楚。

### 5.2 Expand/Contract 模式

安全演进通常分三次甚至更多次发布：

1. **Expand**：只做向后兼容的扩展，例如新增可空列、新表、新索引；旧代码仍能运行。
2. **Migrate**：发布可同时读写新旧结构的应用，后台回填历史数据，监控一致性。
3. **Switch**：切换读取路径，确认所有实例和离线任务都不再依赖旧结构。
4. **Contract**：在后续独立发布中删除旧列、旧索引和兼容代码。

例如把 `users.full_name` 拆成姓和名，不能一条迁移直接删除旧列。应先加新列、双写、回填、切读，最后再删除。大表建索引要评估在线创建方式和锁；回填要分批、限速、可恢复，不能在一个巨大事务中堵住生产。

迁移的 `down` 文件也不代表任何时候都能安全回退。删除列后数据已丢，down 只能重建空列。真实回滚往往是“修复向前”，或依赖经过验证的备份恢复。发布前必须在接近生产规模的数据上测量迁移耗时、锁等待和磁盘增长。

当前 `runDBMigration` 还没有调用 migrate 实例的 `Close`，应用也没有显式关闭 pgxpool。资源会在进程退出时被操作系统回收，但生产代码应该明确管理生命周期，让错误可见并便于优雅退出。

---

## 6. 可观测性：日志、指标和链路追踪不是一回事

### 6.1 三种信号

- **日志（Logs）**：离散事件，适合调查“这一请求发生了什么”。
- **指标（Metrics）**：随时间聚合的数值，适合仪表盘、趋势和告警。
- **链路（Traces）**：一次请求跨组件的路径，适合定位延迟在哪一跳。

OpenTelemetry 将 traces、metrics、logs 视为相关但不同的遥测信号，参阅 [OpenTelemetry Signals](https://opentelemetry.io/docs/concepts/signals/)。三者要用 request/trace ID、服务名、版本和环境关联，而不是互相替代。

### 6.2 结构化日志

结构化日志把字段输出为机器可查询的结构，例如：

```json
{"level":"info","service":"simplebank","protocol":"grpc","method":"LoginUser","duration_ms":18,"trace_id":"..."}
```

字段应稳定、低基数，并避免秘密和个人数据。不要记录密码、Token、Authorization Header、完整数据库连接串；用户标识也应根据合规要求脱敏或哈希。日志级别应有语义：预期的用户输入错误不一定是 error；真正需要处理的服务故障才是 error。

提交 `ce7994a` 增加 [gapi/logger.go](../../gapi/logger.go) 中的 gRPC Unary Interceptor，并用 zerolog 记录协议、方法、状态和耗时；`b48ade6` 把 HTTP Logger 包装到 Gateway。这是结构化日志的良好起点。Worker 也通过 [worker/logger.go](../../worker/logger.go) 适配 zerolog。

当前实现的问题：

1. [main.go](../../main.go) 多个 `log.Fatal().Msg(...)` 没有 `.Err(err)`，实际错误被丢掉。例如配置、连接、监听和迁移失败时只看到概括文本。
2. HTTP Logger 会在非 200 时记录完整响应 body。错误体可能包含内部错误、邮箱或其他敏感信息，而且大 body 会放大内存与日志成本。
3. `ResponseRecorder.Body` 在每次 `Write` 时直接替换而不是累加；它也没有转发 `Flusher`、`Hijacker`、`Pusher` 等可选接口，若未来处理流式响应或 WebSocket 可能改变行为。
4. 将所有非 200 都记为 error 不准确：201、204、400、401、404 都可能是正常业务结果。
5. 缺少 request ID、trace ID、客户端信息、应用版本、环境和实例标识。
6. gRPC 只有 Unary Interceptor；如果未来增加 streaming RPC，需要单独的 Stream Interceptor。

### 6.3 指标应该观测什么

对 SimpleBank，至少需要四类指标：

**请求 RED：**

- Rate：HTTP/gRPC 请求率；
- Errors：按方法和状态分类的错误率；
- Duration：延迟直方图及 p50/p95/p99。

**资源 USE：**

- Utilization：CPU、内存、磁盘、网络、连接池占用；
- Saturation：队列长度、pgx pool 等待、goroutine 数、Redis 队列积压；
- Errors：OOM、连接错误、磁盘错误。

**依赖：** PostgreSQL 查询延迟、锁等待、连接数、复制延迟；Redis 延迟和错误；SMTP 成功率；Asynq 每类任务成功、重试、死亡任务、队列最老任务年龄。

**业务正确性：** 转账成功/失败/不确定结果，余额约束异常，邮箱验证转化率。业务指标不能含高基数标签，例如不要把 username、account ID、token ID 放进 Prometheus label，否则时间序列数量会爆炸。

### 6.4 Trace 如何穿过系统

HTTP Gateway 接收 W3C trace context，创建 span；gRPC handler、SQL、Redis 入队继续使用同一个 `context.Context`。异步任务不能保留原进程 context，但可以把受控的 trace correlation 信息写入任务元数据，在 Worker 建立新的消费 span 并链接生产 span。

当前项目还没有 OpenTelemetry、指标端点或分布式追踪。这些属于工业建议，不能说成已实现。

---

## 7. 超时、重试、熔断、限流与背压

### 7.1 超时和 deadline

没有超时的网络调用可能永远占用 goroutine、连接和内存。每个入口请求应有总 deadline，并把剩余时间通过 context 传给 SQL、Redis 和下游。下游超时必须小于上游总预算，例如总预算 2 秒，数据库最多 500 ms，邮件不应同步占用这条预算。

当前 Gateway 使用 `http.Serve(listener, handler)`，没有显式 `http.Server`，也就没有配置 `ReadHeaderTimeout`、`ReadTimeout`、`WriteTimeout`、`IdleTimeout`。gRPC 也没有统一 deadline 策略。Go 的 `context` 能取消数据库操作，但前提是调用链真的传递了带 deadline 的 context。

### 7.2 重试不是万能药

重试适合暂时性错误，例如连接被重置或服务短暂过载，但必须同时满足：

- 操作是幂等的，或带服务端幂等键；
- 只重试明确的暂时错误；
- 次数有限，使用指数退避和随机抖动；
- 总时间不超过 deadline；
- 只有一层负责重试，避免 SDK、服务和代理叠加成重试风暴。

银行转账尤其不能因为客户端看到超时就直接重发。应要求 `idempotency_key`，服务端持久化请求身份与最终结果；同一 key 重放时返回原结果。数据库提交后响应丢失是典型的“结果未知”，不是简单失败。

Asynq 邮件任务当前配置最大重试次数，适合处理暂时 SMTP 故障；但消费者必须幂等，否则每次重试都可能创建新验证记录或重复发信。队列重试也需要死信处理、最老任务年龄告警和人工重放手册。

### 7.3 熔断、隔舱、限流和背压

- **熔断**：下游持续失败时暂时停止调用，防止资源浪费；半开状态用少量请求探测恢复。
- **隔舱（bulkhead）**：给不同依赖或任务队列独立并发额度，邮件阻塞不能耗尽转账资源。
- **限流**：按 IP、用户、Token 或业务动作限制请求，保护登录、验证码和昂贵查询。
- **背压**：系统饱和时明确拒绝或排队，而不是无限创建 goroutine。

限流应同时考虑边缘网关和应用语义。登录接口可按 IP+账户组合；转账按已认证主体和风险规则；返回 HTTP 429 或 gRPC `ResourceExhausted`，附合理重试提示。分布式限流需要共享状态或网关支持，但 Redis 故障时究竟 fail-open 还是 fail-closed，要按业务风险决定。

熔断也不应到处添加。PostgreSQL 是核心依赖，断路后系统大多只能拒绝写入；更重要的是连接池上限、查询超时和负载保护。SMTP 是异步依赖，更适合通过队列隔离。每种机制都要有指标，否则保护动作本身会变成黑盒。

---

## 8. 优雅关闭与资源生命周期

### 8.1 为什么直接杀进程会出问题

平台停止容器时通常先发终止信号，等待宽限期后再强制杀死。收到信号后服务应：

1. 将 readiness 设为失败，停止接收新流量；
2. 停止新的 gRPC/HTTP 请求；
3. 给在途请求有限时间完成；
4. 停止领取新任务，让正在处理的任务完成或安全重新入队；
5. flush 遥测，关闭 Redis client、数据库连接池、listener 等资源；
6. 超过总关闭 deadline 后强制退出。

Go 通常通过 `signal.NotifyContext` 接收 SIGINT/SIGTERM；HTTP 使用 `Server.Shutdown(ctx)`；gRPC 使用 `GracefulStop()`，超时后退化为 `Stop()`；Asynq Processor 已经暴露 `Shutdown()`，但当前 `main.go` 没有保留它并调用。

### 8.2 回到 SimpleBank

当前 `main.go` 直接：

```text
goroutine: Worker
goroutine: HTTP Gateway
主 goroutine: gRPC Serve
```

任一 goroutine 内调用 `log.Fatal` 都会直接退出进程；没有集中错误组、信号处理或协调关闭。HTTP 用包级 `http.Serve`，没有可调用 `Shutdown` 的 server 变量；gRPC server 只在局部存在；pgxpool 没有 `Close`；任务分发器内部 Asynq client 没有暴露 `Close`；migrate 资源也没有关闭。

这说明仓库**尚未实现优雅关闭**。README 的 Lecture #74 只是课程链接，当前 Git 历史没有对应提交。设计优雅退出时还要避免常见错误：无限等待。关闭必须有上限，并确保平台的 termination grace period 大于应用内部 drain 时间。

---

## 9. TLS、mTLS 与网络边界

### 9.1 TLS 解决什么

TLS 提供传输加密、完整性和服务端身份认证。公网 HTTP 应由可信证书保护；数据库连接也应验证服务端证书，不能在生产连接串中沿用 `sslmode=disable`。证书要自动签发、续期并监控到期时间。

mTLS 在 TLS 基础上让客户端也提供证书，常用于服务间身份。它不是业务授权的替代品：即使某个服务证书合法，仍要判断它是否有权执行某项转账操作。

TLS 可以终止在负载均衡器/Ingress，也可以一直到应用；选择取决于威胁模型、合规、网络是否可信和运维能力。如果边缘终止 TLS，边缘到后端仍应位于受控网络，敏感环境可继续使用内部 TLS/mTLS。gRPC 原生适合 TLS，但当前 `grpc.NewServer` 没有 credentials，Gateway 的 `http.Serve` 也未配置 TLS，所以当前 8081/9090 都是明文服务。

仓库没有 AWS、ECR、RDS、Route53、EKS、Kubernetes 清单、Ingress 或证书配置。README Lecture #26—#36 和 #71—#72 描述的是课程内容，不是这个分支已实现的事实。**TLS 也没有在当前 Git 历史落地。**

---

## 10. CORS：浏览器策略，不是认证系统

CORS 决定浏览器是否允许某个 origin 的前端 JavaScript 读取跨源响应。它不是防火墙：curl、移动应用和后端服务不受浏览器 CORS 限制；也不能替代 Token、权限和 CSRF 防护。

生产策略应明确允许：

- 精确 origin 列表，不要把任意来源和凭据组合；
- 必要的方法和请求头，例如 `Authorization`、`Content-Type`；
- 是否允许 credentials；
- 预检缓存时间；
- `Vary: Origin` 等正确缓存行为。

若使用 Cookie 认证，还要认真设计 SameSite、Secure、HttpOnly 与 CSRF Token。当前 SimpleBank 没有 CORS middleware 或策略配置。README Lecture #76 和前端目录中的 CORS 视频只是课程目录，当前 Git 历史没有实现。

同样，README Lecture #77 提到 JWT v5 升级，但仓库没有对应提交；当前 `go.mod` 仍列出 `github.com/dgrijalva/jwt-go`，运行服务器主要使用项目的 PASETO Maker。不能把“课程计划升级”写成“项目已经升级”。

---

## 11. 备份、恢复、RPO 与 RTO

### 11.1 两个必须会说的指标

- **RPO（Recovery Point Objective）**：最多能接受丢失多长时间的数据。例如 RPO 5 分钟表示灾难后最多容忍最近 5 分钟数据丢失。
- **RTO（Recovery Time Objective）**：从事故发生到恢复服务最多允许多久。

RPO/RTO 是业务选择，不是工程师拍脑袋。银行账务通常要求极低数据丢失，并需要审计和对账，但不同系统的法律与业务要求不同，不能一概宣称“必须为零”。目标越严格，跨可用区同步、日志归档、演练和人员成本越高。

### 11.2 PostgreSQL 恢复层次

- 逻辑备份（如 `pg_dump`）：适合对象级恢复、迁移和较小数据库；恢复大库可能慢。
- 物理基础备份：复制数据库文件层面的基线。
- WAL 连续归档：结合基础备份实现 PITR，恢复到某一时间点；PostgreSQL 官方说明见 [Continuous Archiving and PITR](https://www.postgresql.org/docs/18/continuous-archiving.html)。
- 同步/异步副本：提高可用性，但**副本不是备份**。误删除和逻辑损坏也可能复制过去。

备份必须加密、限制访问、跨故障域保存、设置保留策略，并监控最后成功时间和可恢复性。最关键的一句是：**没有做过恢复演练的备份，只是一个希望。** 应定期在隔离环境恢复，验证 schema、记录数、约束、业务不变量和实际 RTO。

当前 Compose 的 `data-volume` 只让本机 PostgreSQL 数据脱离容器生命周期。主机磁盘损坏、误删卷或数据库逻辑损坏时仍会丢失；仓库没有备份、PITR、恢复脚本、复制或对账流程。

Redis 在本项目主要承载异步任务。要明确任务丢失的业务后果，再决定 AOF/RDB、主从、托管服务和死信补偿。仅仅“可以重新发邮件”也需要有数据库状态或 Outbox 支持重建任务。

---

## 12. 容量规划与性能边界

容量规划不是猜“需要几台机器”，而是把业务负载转换为资源需求和安全余量：

1. 估算峰值 QPS、并发数、请求体大小和读写比例；
2. 测量单实例在目标延迟下的稳定吞吐，而不是压到崩溃的最高数字；
3. 找到瓶颈：CPU、内存、数据库锁、连接池、磁盘 IOPS、Redis 或外部邮件；
4. 保留故障和发布余量，例如少一个可用区时仍满足 SLO；
5. 用真实分布的负载测试验证，并持续根据生产趋势修正。

Little's Law 可帮助理解并发：稳定系统中 `并发量 ≈ 到达率 × 平均处理时间`。若 1000 请求/秒、平均耗时 0.2 秒，系统中平均约有 200 个在途请求。延迟上升会推高并发，继而占满连接池，形成反馈循环。

pgxpool 大小不能简单设得越大越好。每个应用副本 50 个连接、20 个副本就是 1000 个数据库连接，可能把 PostgreSQL 压垮。要从数据库总连接预算倒推单副本池大小，给迁移、运维和故障切换留余量，并观测 Acquire 等待时间。

Asynq 的队列权重 `critical: 10`、`default: 5` 是调度权重，不等于业务容量证明。需要测量任务到达率、处理时间、并发度和最老任务年龄；邮件服务限额也可能成为瓶颈。

当前仓库没有基准测试、负载测试、自动扩缩容、容量报告或资源 request/limit 配置，因此这些仍是待建设能力。

---

## 13. SLI、SLO、告警与故障演练

### 13.1 术语

- **SLI**：实际测量指标，例如“有效请求成功比例”“p99 延迟”。
- **SLO**：SLI 的目标，例如“滚动 30 天内 99.9% 有效请求成功”。
- **SLA**：带业务或法律后果的协议，不要把内部目标都叫 SLA。
- **Error budget**：允许失败的空间。例如 99.9% 可用性目标对应 0.1% 错误预算。

Google SRE 对这些术语和选择原则有系统说明：[Service Level Objectives](https://sre.google/sre-book/service-level-objectives/)。SLO 要从用户体验和业务正确性出发，而不是从“手头最容易采集的 CPU 指标”出发。

SimpleBank 可先定义教学版目标：

- 登录和查询：成功率、p95/p99 延迟；
- 转账：正确完成比例、不确定结果比例、重复处理比例；
- 邮件：从注册提交到验证邮件可投递的端到端延迟；
- 数据：备份成功与恢复演练、账务不变量和对账差异。

具体数字必须由业务量、用户预期和成本共同决定，本章不替真实业务编造 99.99%。

### 13.2 好告警的标准

告警应当可行动：收到后值班人员知道用户受什么影响、先查哪里、如何缓解。优先根据错误预算消耗、错误率、延迟和任务积压告警；CPU 高但用户无影响可能只是容量预警，不一定凌晨叫醒人。

每条高优先级告警应带：

- 服务、环境和影响范围；
- 当前值、阈值、持续时间；
- Dashboard 和相关日志链接；
- Runbook；
- 最近发布和配置变更；
- 升级联系人。

要抑制抖动、去重和聚合。页面告警用于需要立即人工处置的事故；低优先级问题进入工单。当前仓库只有日志，没有指标规则、Dashboard、SLO 或告警配置。

### 13.3 故障演练

故障演练不是随意在生产拔网线。流程应是：提出假设、限定爆炸半径、准备停止条件和回滚、通知相关人员、观测、复盘并落实改进。可以从预发布开始：

- 停止 Redis，观察注册 API、任务投递和恢复后积压；
- 给 PostgreSQL 注入延迟，观察 deadline、连接池和 readiness；
- SMTP 持续返回临时错误，观察重试、死信和告警；
- 发布一个不兼容迁移到影子数据库，验证流水线能否阻止；
- 给进程发送 SIGTERM，确认在途转账、HTTP、gRPC 和 Worker 是否安全退出；
- 从备份恢复到隔离环境，测量实际 RPO/RTO。

每次复盘关注系统改进，不把事故简单归咎于个人。Runbook 也必须演练，否则真正事故中可能已经失效。

---

## 14. 供应链安全与依赖升级

后端依赖链包括：Go module、生成工具、GitHub Actions、基础镜像、系统包、迁移二进制和 Proto 插件。任何一环被篡改，都可能进入生产制品。

最低实践包括：

1. `go.mod/go.sum` 纳入评审，理解新增直接依赖；
2. CI 使用受控工具版本并验证下载 checksum；
3. 第三方 Action 固定版本，严格环境可固定完整 SHA；
4. 生成 SBOM，扫描 Go 依赖、镜像和许可证；
5. 构建使用最小权限 Token，PR 流程不得接触生产秘密；
6. 镜像签名、部署侧验证、制品库不可变；
7. 自动依赖升级以小 PR 形式进行，测试通过后逐步发布；
8. 为紧急漏洞升级准备旁路流程，但仍保留审计与回滚。

当前 `go.mod` 中依赖很多，且保留了 `lib/pq` 与 pgx 相关项；是否可删除必须以源码引用、生成代码和测试为依据，不能看到“似乎旧”就直接删。README 提到 JWT v5，但当前代码和 Git 历史没有完成升级。依赖升级要阅读 breaking changes，跑单元/集成/兼容测试，不能只让 `go get -u` 修改版本后提交。

---

## 15. 当前仓库生产就绪审计表

下面是截至当前 `ft/RABC` / `ce0b978` 的事实汇总：

| 能力 | 当前已有 | 仍缺少或存在风险 |
|---|---|---|
| 配置 | Viper + 环境变量覆盖 | 必须有 `app.env`；无完整启动校验 |
| 秘密 | 字段已外置 | `app.env` 被提交且进镜像；必须轮换，不能只删除 |
| 镜像 | 多阶段构建 | 端口元数据不一致；无非 root、SBOM、签名、`.dockerignore` |
| 本地编排 | PostgreSQL、Redis、API、命名卷 | API 未等待 Redis；缺 healthcheck |
| 数据库 | SQL migrations；启动自动 Up | 多副本迁移耦合、无专用迁移 job、无 expand/contract 发布流程 |
| 日志 | zerolog；gRPC unary 和 HTTP 请求日志 | 多处漏 `.Err(err)`；可能记录错误 body；无 trace/request ID |
| 指标/追踪 | 无 | 无 RED/USE、OpenTelemetry、Dashboard、告警 |
| 超时与保护 | context 在业务接口中传递 | HTTP server 无显式 timeout；无统一 deadline、限流、熔断策略 |
| 优雅退出 | `start.sh` 使用 `exec`；Processor 有 `Shutdown` 方法 | main 未监听 signal、未 drain、未 Close 资源 |
| TLS/CORS | 无 | 当前 HTTP/gRPC 明文，无 CORS 策略 |
| CI | main/PR 测试、PostgreSQL service、迁移 | 无 Redis、lint/race/安全扫描、制品与部署阶段 |
| 备份恢复 | 本地命名卷 | 无备份、PITR、恢复演练、RPO/RTO |
| 云与编排 | 无 | 无 AWS/EKS/Kubernetes/Ingress/自动部署配置 |

尤其要牢记：README 中 AWS、EKS、Kubernetes、TLS、优雅关闭、CORS、JWT v5 等课程条目，在当前 Git 历史中没有实现。README 是学习路线，不是仓库能力清单。

---

## 16. 分阶段生产化清单

不要一次引入 Kubernetes、Service Mesh、全套可观测性平台和几十个中间件。按风险递进更适合这个项目。

### P0：在任何联网部署前

- 轮换所有已提交秘密，移出镜像与 Git 当前版本，接入秘密扫描；
- 配置启动校验，错误日志保留 `err` 但屏蔽敏感值；
- 增加数据库约束、转账幂等键和审计要求；
- 为 HTTP/gRPC 设置 deadline、请求大小上限和基本限流；
- 使用 TLS，生产数据库禁止明文连接；
- 增加 signal 处理与有上限的优雅关闭；
- 明确部署使用的唯一镜像和回滚方法。

### P1：第一次受控生产试运行

- 建立 `/livez`、`/readyz` 和 gRPC health；验证 PostgreSQL/Redis 依赖语义；
- 将迁移移为一次性发布任务，采用 expand/contract；
- 加入请求率、错误率、延迟、连接池、队列积压和业务正确性指标；
- 定义最小 SLO、告警和 Runbook；
- PostgreSQL 自动备份与 PITR，完成第一次隔离恢复演练；
- 邮件使用专用生产服务和域名认证，开发/CI 使用沙箱或 Mock；
- CI 增加格式、vet、race、静态检查、Redis 集成与镜像扫描。

### P2：规模增长前

- 负载测试并确定实例、连接池和队列容量；
- 金丝雀/蓝绿发布、自动健康门禁、发布审计；
- OpenTelemetry trace，关联 HTTP/gRPC/SQL/Redis/Worker；
- 多可用区、数据库故障转移与故障演练；
- SBOM、签名、策略验证和依赖自动升级；
- 数据保留、隐私、审计与访问权限复查。

### P3：高风险金融要求

- 独立安全评审、威胁建模、渗透测试和合规控制；
- 账务总账、不可变审计、对账与异常检测；
- 更严格的双人审批、职责分离、密钥托管和灾备；
- 以业务正确性 SLI 驱动错误预算和变更冻结；
- 定期桌面推演、区域故障和恢复演练。

这份清单是学习顺序，不是合规认证。真实银行系统还受所在司法辖区、支付网络和组织制度约束，需要专业安全、法务和审计参与。

---

## 17. 六个故障场景：训练你的生产思维

### 场景 A：Redis 在注册时不可用

当前 API 能连接 PostgreSQL，但任务分发到 Redis 失败。你要问：用户事务是否回滚？客户端能否安全重试？Redis 恢复后任务如何补发？有没有 Outbox？告警看错误率还是队列深度？

理想方向是 Transactional Outbox，让用户记录和待发事件在同一个 PostgreSQL 事务提交，再由独立发布器投递。这样 Redis 短暂故障不会让业务事实消失。

### 场景 B：迁移锁表 20 分钟

新实例启动时自动 Up，旧实例查询开始超时。单纯增加 liveness 重启会更糟。应停止发布、识别阻塞 SQL、按 Runbook 决定取消迁移还是等待；事前应在生产规模副本测试锁和耗时，用 expand/contract 与在线 DDL 降低风险。

### 场景 C：转账提交后响应丢失

客户端看到 deadline exceeded，但数据库可能已经提交。没有幂等键时重试可能重复转账。服务端需要持久化请求 ID 与结果，客户端查询原请求状态，而不是把“超时”等同于“失败”。

### 场景 D：某个秘密出现在 Git 历史

不能只做 `git rm app.env`。首先轮换与撤销，检查使用日志，再清理历史与镜像，通知协作者，最后建立扫描门禁。顺序错了会留下仍可使用的旧凭据。

### 场景 E：SIGTERM 时 Worker 正在发邮件

若进程立即退出，任务可能按至少一次语义重试并重复发信。任务处理要幂等；退出时停止取新任务，等待在途任务到 deadline，未完成任务应保持可重试状态。

### 场景 F：备份每天显示成功，但恢复失败

“上传文件成功”不等于可恢复。可能缺 WAL、加密密钥、版本不兼容或备份早已损坏。监控必须包括定期恢复验证和实际恢复耗时，而不只看备份 job 的退出码。

---

## 18. 动手练习（建议独立分支完成）

这些练习按风险从低到高排列。本章不替你实际修改仓库。

1. **事实审计**：逐行标注 Dockerfile、Compose、main.go，分别写出 build、release、run、shutdown 的责任边界。
2. **配置测试**：给 `LoadConfig` 设计“仅环境变量，无 app.env”“缺少 Token key”“Duration 非法”三个测试。先写预期，不急着改代码。
3. **日志修复设计**：列出 `main.go` 每个丢失 `err` 的位置，设计既保留原因又不打印秘密的字段。
4. **健康接口设计**：写 `/livez` 与 `/readyz` 的判定表。说明 PostgreSQL、Redis、SMTP 各自故障时两个端点应返回什么，以及为什么。
5. **优雅关闭**：画出 signal → readiness false → HTTP Shutdown / gRPC GracefulStop / Worker Shutdown → pool Close 的时序图，并规定总超时。
6. **迁移演练**：设计一次给 users 增加必填字段的 expand/contract 三阶段方案，禁止第一步直接 `NOT NULL` 且无默认回填。
7. **幂等转账**：设计 `idempotency_key` 表字段、唯一约束、处理中/成功/失败状态和重复请求返回规则。
8. **CI 分层**：把测试计划分为秒级单元测试、分钟级数据库/Redis 集成测试、受控邮件沙箱测试、发布前端到端测试。
9. **可观测性**：为 Login、Transfer、SendVerifyEmail 各列 3 个 metric，检查是否含高基数标签。
10. **恢复演练**：写一个不触碰真实生产的演练方案：从备份恢复到隔离实例，验证迁移版本、表数量、外键和账户总额不变量，记录 RPO/RTO。
11. **故障注入**：在本地 Compose 停止 Redis、延迟数据库和发送 SIGTERM，记录当前行为与目标行为差距。
12. **供应链审计**：盘点 Go module、Actions、基础镜像和下载二进制，给每一类写出版本、校验、更新和回滚责任人。

---

## 19. 自测题

先不看答案，尝试用自己的话回答。

1. 为什么把秘密放进环境变量还不等于完成秘密管理？
2. Dockerfile 的 `EXPOSE 8080` 为什么不会自动把宿主机 8080 打开？当前项目的端口问题是什么？
3. `depends_on`、端口可连接和 readiness 有什么区别？
4. 为什么应用启动时自动迁移在单实例很方便，在多副本发布却有风险？
5. 日志、指标、Trace 各自最擅长回答什么问题？
6. 为什么转账超时不能盲目重试？
7. liveness 为什么通常不应把 PostgreSQL 可用性作为强条件？
8. `start.sh` 使用 `exec` 有什么意义？为什么项目仍不算优雅关闭？
9. 副本、命名卷和备份有什么区别？
10. RPO 与 RTO 分别回答什么问题？
11. 为什么 README 有 Kubernetes/TLS 课程链接，仍不能说仓库实现了 Kubernetes/TLS？
12. 为什么“所有请求都达到 100% 可用”通常不是合理 SLO？

### 参考答案

1. 环境变量只是注入途径；秘密仍需加密存储、最小权限、审计、轮换、撤销，并防止进入日志、清单和构建物。
2. `EXPOSE` 是元数据，发布由 `-p`/Compose 完成，监听由应用决定。项目镜像声明 8080，而实际 HTTP 配置和 Compose 使用 8081，信息不一致。
3. `depends_on` 默认只控制启动顺序；端口可连只证明 TCP 层；readiness 表示应用及其必要初始化已完成、适合接收业务流量。
4. 每个副本都会尝试迁移，可能争锁、锁表、要求过大权限并把发布与 schema 变化耦合；滚动期间新旧代码还必须同时兼容 schema。
5. 日志解释具体事件，指标观察聚合趋势并告警，Trace 展示一次请求跨组件路径与耗时。
6. 数据库可能已提交但响应丢失，重复请求会再次扣款。必须使用持久化幂等键和结果查询。
7. 数据库故障时重启所有应用不会修好数据库，反而造成重连风暴；可用 readiness 摘流并让 liveness 关注进程自身。
8. `exec` 让 Go 成为容器主进程并接收信号；但 main 没有监听信号、drain server、Shutdown Worker 或 Close 资源。
9. 副本提高可用性但会复制误操作；命名卷只延长本机数据生命周期；备份是独立、可恢复、带保留与演练的数据副本。
10. RPO 是最多允许丢多少时间的数据；RTO 是最多允许多久恢复服务。
11. README 描述课程路线；只有 Git 中的实际配置和代码才证明能力。当前没有 Kubernetes 清单或 TLS 实现提交。
12. 绝对可用既不现实，也可能导致极高成本和阻碍发布；SLO 应由用户需求、风险和成本决定，并用错误预算管理变化。

---

## 20. 掌握清单

当你能在不看本章的情况下做到以下事情，才算真正掌握这一阶段：

- [ ] 能区分普通配置、敏感配置和秘密，并解释完整密钥轮换顺序；
- [ ] 能解释镜像、容器、多阶段构建、`EXPOSE` 和端口映射；
- [ ] 能区分启动、存活、就绪，并为数据库、Redis、SMTP 设计依赖策略；
- [ ] 能读懂当前 CI 的触发条件、测试环境与没有覆盖的边界；
- [ ] 能用 expand/contract 设计新旧版本兼容的数据库发布；
- [ ] 能为 HTTP、gRPC、SQL、Redis 和 Worker 设计结构化日志、指标与 Trace；
- [ ] 能说明 deadline、重试、幂等、熔断、隔舱、限流和背压的适用边界；
- [ ] 能画出有时间上限的 Go 服务优雅关闭流程；
- [ ] 能解释 TLS、mTLS、CORS 各自解决什么，以及不能解决什么；
- [ ] 能根据业务定义 RPO/RTO，并组织一次真实的恢复验证；
- [ ] 能从流量、延迟和连接预算推导容量，而不是只看 CPU；
- [ ] 能定义少量用户导向的 SLI/SLO，并让告警具备可行动性；
- [ ] 能列出 Go、Action、基础镜像和工具二进制的供应链控制；
- [ ] 能明确指出 SimpleBank 当前已经做到什么、仍缺什么，不把 README 课程目录当作实现事实。

---

## 21. 本章核对过的仓库证据与官方资料

### Git 提交

- `295accf`：初始化 CI；
- `cc88600`：CI 加 PostgreSQL service 和迁移；
- `d29731f`、`325c9c0`、`7565ae8`：逐步修正 golang-migrate 安装；
- `de09c2c`：CI 映射 PostgreSQL 端口；
- `3a955a6`：首次加入多阶段 Dockerfile；
- `116fc3e`：Compose、`start.sh`、`wait-for.sh`；
- `ffcfd13`：迁移移入 Go 启动流程，HTTP 改到 8081；
- `ce7994a`：gRPC/zerolog 结构化日志；
- `b48ade6`：HTTP 日志 middleware 接入；
- `2cd81b4`：PostgreSQL 端口与卷、Redis、gRPC 端口；
- `ce0b978`：当前分支 HEAD，RBAC。

### 当前文件

- [Dockerfile](../../Dockerfile)
- [docker-compose.yaml](../../docker-compose.yaml)
- [start.sh](../../start.sh)
- [wait-for.sh](../../wait-for.sh)
- [main.go](../../main.go)
- [util/config.go](../../util/config.go)
- [gapi/logger.go](../../gapi/logger.go)
- [worker/logger.go](../../worker/logger.go)
- [.github/workflows/ci.yml](../../.github/workflows/ci.yml)
- [Makefile](../../Makefile)
- [mail/sender_test.go](../../mail/sender_test.go)

### 官方概念资料

- [The Twelve-Factor App](https://12factor.net/)
- [Docker Compose：控制启动与关闭顺序](https://docs.docker.com/compose/how-tos/startup-order/)
- [Kubernetes：Liveness、Readiness 与 Startup Probes](https://kubernetes.io/docs/concepts/workloads/pods/probes/)
- [OpenTelemetry Signals](https://opentelemetry.io/docs/concepts/signals/)
- [Google SRE：Service Level Objectives](https://sre.google/sre-book/service-level-objectives/)
- [PostgreSQL：Continuous Archiving 与 PITR](https://www.postgresql.org/docs/18/continuous-archiving.html)

最后再强调一次：本章给出的生产做法是用来暴露差距和建立思维框架，不代表当前项目已经具备这些能力。生产化没有“一键毕业”，它是一套通过测量、演练、复盘和持续改进维持的工程纪律。
