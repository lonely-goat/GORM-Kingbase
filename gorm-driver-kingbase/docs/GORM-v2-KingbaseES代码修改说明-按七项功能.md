# GORM v2 KingbaseES 代码修改说明与源码对照导读

整理日期：2026-10-28。以《GORM v2-需求分析-V1.0》的 6.2.1—6.2.7 为目录，以当前工作区生产源码为实现依据。需求描述不自动等于已经实现，现有限制单独说明。

## 0. 对照基线与阅读方法

### 0.1 “原框架”分三层对照

| 对照层 | 实际源码根目录 | 本文用途 |
|---|---|---|
| GORM 核心 v1.31.1 | `gorm.io/gorm v1.31.1` | 解释 Open、Session、Schema、Statement、Callback、Scan、AutoMigrate、事务和关联原来怎样工作 |
| 原 MySQL 方言 v1.6.0 | `gorm.io/driver/mysql v1.6.0` | 对比原生 MySQL 的连接、类型、冲突、迁移和错误处理 |
| Oracle 示例方言 v1.1.2 | `github.com/oracle-samples/gorm-oracle v1.1.2` | 对比该版本面向 Oracle/godror 的类型、RETURNING INTO、MERGE 和目录查询 |
| 本项目新方言 | 仓库的 `gorm-driver-kingbase/` 目录 | 记录通过 gokb 适配金仓两种模式的实际新增代码 |

方言实际引用 GORM v1.31.1；配套 gokb 通过指向仓库 `third_party/gokb/` 的相对路径 replace 提供。MySQL、Oracle 方言是比较基线，不是新方言的运行依赖。[依赖声明][k-mod]

对照时已核查三个比较基线的 Go 源码与模块下载归档一致。开发工作区另有 GORM 核心研究副本，与当前依赖版本存在差异；这些差异不计入金仓适配修改，也不作为本仓库运行依赖。

本文的“覆盖”指通过接口、回调注册或嵌入结构的方法覆盖运行行为；不表示改写了原依赖文件。无原版同名函数的新增逻辑，会给出它所接入的原版调用点，不虚构一个“修改前函数”。原框架链接固定到相应 tag，本仓库链接使用相对路径；位置对应本次阅读时的行号。

### 0.2 七项功能与文件对应

| 需求 | 先看原框架 | 再看新增适配 | 本项性质 |
|---|---|---|---|
| 6.2.1 连接与会话管理 | `gorm.go:132` 的 Open、`:250` 的 Session | `kingbase.go:132`、`mode.go:13` | 新建连接初始化和模式机制；复用标准会话 |
| 6.2.2 模型解析与对象映射 | `schema/schema.go:133`、`schema/field.go:106` | `kingbase.go:112`、`:300`、`mysql_types.go`、`oracle_types.go` | 复用解析；新增命名默认配置和物理类型映射 |
| 6.2.3 SQL 构建 | `statement.go:175`、`:493`、`clause/` | `quote_bind.go`、`clause.go` | 新增双模式 SQL 策略 |
| 6.2.4 数据操作与结果映射 | `callbacks/create.go:37`、`scan.go:126` | `kingbase.go:203`、`map_returning.go`、`oracle_names.go`、`errors.go` | 复用 CRUD；配置 RETURNING、补充扫描和错误适配 |
| 6.2.5 数据库结构迁移与元信息管理 | `migrator/migrator.go:121` | `kingbase.go:285`、四个 `migrator*.go` 文件 | 复用 AutoMigrate；覆盖 DDL 和目录读取 |
| 6.2.6 事务管理 | `finisher_api.go:622`、`:665`、`:722` | `kingbase.go:307`、`:317`、`:258` | 复用事务主流程；新增保存点及连接能力保护 |
| 6.2.7 关联关系管理 | `schema/relationship.go:65`、`callbacks/associations.go:13` | RETURNING、OnConflict、列映射和外键迁移适配 | 复用关联引擎；修补其依赖的数据库行为 |

整体调用关系：

```text
业务 GORM API
  → GORM 的 Session / Schema / Statement / Callback
  → kingbase.Dialector：连接、模式、类型、引用、绑定、专用子句
  → kingbase.Migrator：结构操作和数据库目录
  → database/sql → gokb → KingbaseES MySQL / Oracle 模式
```

## 1. 连接与会话管理（需求 6.2.1）

建议顺序：原 Open → 两个原方言 Initialize → 新 Config/Open/New → 新 Initialize → mode.go → 原 Session。

### 1.1 新增统一接入入口和 gokb 驱动选择

**原来：** [GORM `Open`][g-open] 在第 205 行调用 `Dialector.Initialize`，连接方式交给方言。[MySQL `Open/New`][m-open] 会解析 go-sql-driver/mysql 的 DSN，默认驱动名为 `mysql`；[Oracle `Config/Open/Initialize`][o-config] 使用 `DataSourceName` 和默认驱动 `godror`，其 Conn 类型是 `*sql.DB`。

**现在：** [新方言驱动导入和常量][k-driver] 空白导入 gokb，使其注册 `database/sql` 驱动 `kingbase`。[新 `Config`][k-config] 保留 DSN、DriverName、Conn，并增加 Mode、ModeDetectionTimeout、DefaultStringSize、WithoutReturning；Conn 使用 `gorm.ConnPool`，可接收满足接口的连接池。[`Open/New/Name`][k-open] 提供简化入口和完整配置入口，Name 统一返回 `kingbase`。[`Initialize` 的连接分支][k-conn] 有 Conn 时注入，否则调用 `sql.Open`。

**差异含义：** 模式选择成为同一个金仓方言内部的配置，不再要求业务在 MySQL、Oracle 两个原生方言间切换。gokb 是底层通信驱动，Dialector 是上层 GORM 方言，二者职责不同。

### 1.2 新增兼容模式识别、校验与保存

**原来：** [MySQL `Initialize`][m-init] 在第 128—161 行通过 `SELECT VERSION()` 判断 MySQL/MariaDB/TiDB 能力；[Oracle `Initialize`][o-init] 预先假定 Oracle 方言，不探测 KingbaseES 的 database_mode。GORM 核心只提供初始化扩展点，没有这套双模式识别逻辑。

**现在：** [模式定义与配置校验][k-mode] 支持 auto/mysql/oracle；[`normalizeMode` 和 `parseDatabaseMode`][k-parse-mode] 去空白、小写化后精确匹配，拒绝 `notmysql`、PG 等值。[初始化模式校验][k-validate-mode] 发生在建连前；[自动识别分支][k-detect] 执行 `SHOW database_mode`，显式模式直接保存，跳过查询。[`DatabaseMode`][k-get-mode] 为后续类型、SQL、迁移提供统一读取入口。

**新增处理细节：** 自动识别使用 `context.WithTimeout(context.Background(), timeout)`，非正超时采用 5 秒；查询失败或返回未知模式时返回错误，不回退到默认模式。这里的超时只约束模式查询，不覆盖完整 Open、最后 Ping 或后续业务请求。显式模式不核验实际实例，也不切换服务端模式。

### 1.3 新增初始化失败时的连接归属保护

**原来：** [GORM `Open` 的 Initialize 失败清理][g-init-cleanup] 会调用 `db.DB()` 取出可识别的 `*sql.DB` 并关闭；[MySQL 原注入分支][m-init] 直接赋值 `db.ConnPool = dialector.Conn`，不额外区分该池由谁持有。[GORM `DB()`][g-db] 通过具体类型或 `GetDBConnector` 取得底层池。

**现在：** [`borrowedConnPool`][k-borrow] 只暴露基本 `gorm.ConnPool`；[注入连接分支][k-conn] 在初始化期间临时包裹调用方连接池，使失败清理无法取出并关闭调用方的 `*sql.DB`。自建池则用 `openedByDialector` 标记，[`closeOnError`][k-close] 负责清理。[初始化成功尾部][k-restore] 恢复原池，保留 BeginTx、GetDBConn 等可选能力。

**为什么不能一直包装：** GORM 的 ConnPool 基础接口只有 Prepare/Exec/Query/QueryRow；事务、获取底层 DB 依赖额外接口。永久只暴露基础接口会使原本可用的事务或 Connection 能力丢失。

**范围：** 该保护针对方言自身 Initialize 失败；GORM 在 Initialize 返回后还会执行 PrepareStmt 包装和 Ping，不能把这段逻辑描述成覆盖所有后续插件和 Ping 失败的统一资源管理器。

### 1.4 新增 DSN 主键回填冲突检查

**原来：** [GORM Create 的 LastInsertId 路径][g-last-id] 依赖驱动返回自增值。gokb 自带的 `get_last_insert_id` 选项可能为 INSERT 追加 `RETURNING *`；配套 SDK 的 `conn.go:1113` 为该追加实现，见 [gokb 行为说明][kb-append-returning]。

**现在：** [Initialize 的冲突检查][k-check-dsn] 在启用方言 RETURNING、解析模式为 MySQL 且配置 DSN 可识别时拒绝冲突选项。[`hasEnabledLastInsertID`][k-dsn-parser] 识别空白、`?`、`&`、`;` 分隔，兼容关键字 DSN 与 URL 查询串，以及 yes/true/1/on、大小写和外层引号。

**目的与边界：** 避免方言和驱动各追加一次 RETURNING。检查的是 `Config.DSN`，不是对外部注入池内部 DSN 的反向读取，也不是完整 DSN 解析器。

### 1.5 新增初始化与模式读取同步

**原来：** GORM 提供可共享 DB、Schema 等机制，但没有本项目新增的 resolvedMode 状态。

**现在：** [Dialector 状态字段][k-state] 中 `initializeMu` 串行化 Initialize；`modeMu` 保护配置模式及已解析模式的读写；[`DatabaseMode/setConfiguredMode/setResolvedMode`][k-get-mode] 集中访问。普通会话复用已初始化方言，不逐条 SQL 探测模式。

**范围：** 此处不表示任意共享同一个 Dialector 重新初始化到不同数据库都没有行为风险，也不表示所有 GORM 核心迁移竞态已解决。

### 1.6 会话、上下文、预编译与固定连接复用原机制

**原来和现在共用：** [GORM `Session`][g-session] 复制会话配置；[`WithContext`][g-context] 通过 Session 传上下文；[`prepare_stmt.go`][g-prepared] 管理预编译缓存；[`Connection`][g-connection] 取得 `*sql.Conn`，在回调结束后归还。

**适配接入点：** 新方言提供符合 ConnPool 的 gokb 连接，并在 [Initialize 成功时恢复原池][k-restore]；没有新增 Kingbase 专用 Session、上下文传播器或预编译缓存。

**文档表述：** “复用 GORM 会话、上下文及预编译机制，新增连接初始化与模式策略，并保留连接池扩展能力。”普通 Session 不等于固定物理连接；需要固定连接时用 Connection 或事务。只设置 DryRun 不保证 Open 不访问数据库：自动模式查询、自动 Ping、默认事务需分别考虑。

**现有验证入口：** [模式/连接测试][t-mode]、[超时与连接归属测试][t-ownership]、[DSN 变体测试][t-dsn]、[真实库事务会话测试][t-tx]、[需求连接会话示例][e-connect]。

## 2. 模型解析与对象映射（需求 6.2.2）

建议顺序：Schema.Parse → ParseField → 命名策略 → Migrator.DataTypeOf → 新方言类型分派 → 双模式类型文件。

### 2.1 模型解析、标签、自定义类型入口沿用 GORM

**原来：** [`schema.Parse`][g-schema] 处理模型和缓存；[`ParseField`][g-field] 读取 column、primaryKey、size、precision、scale、default 等标签，识别 GormDataType、Scanner/Valuer、Serializer 等扩展。[核心 Migrator.DataTypeOf][g-mig-type] 优先调用用户的 `GormDBDataType`，否则调用方言 DataTypeOf；[`FullDataTypeOf`][g-full-type] 拼接 NOT NULL、DEFAULT。

**现在：** [新 `Migrator`][k-migrator-entry] 嵌入通用 Migrator；[新 `DataTypeOf`][k-data-type] 根据实际模式分派到 mysqlDataTypeOf 或 oracleDataTypeOf。没有重写 Schema.Parse、ParseField，也没有建立金仓专用标签解析器。显式未知数据库类型并非本地必然报错，可能解析成功后由服务端拒绝。

### 2.2 新增标识符长度的默认配置

**原来：** [GORM Open 默认值][g-default-name] 为 64；[`NamingStrategy.formatName`][g-format-name] 负责生成索引/约束等名称并在超长时加哈希。

**现在：** [新 `Apply`][k-apply] 将未显式设置的默认 IdentifierMaxLength 设为 63；兼容 NamingStrategy 的值类型和指针类型，保留调用方明确的正值或自定义命名器。GORM 在 Initialize 前调用 Apply。

**准确范围：** 这里设置的是 GORM 命名策略参数，没有重写其名称生成算法，也没有对任意手写表列名实施统一的 63 字节校验。原算法包含按 rune 判断长度和按字节切片的行为，不能扩大为“所有中文标识符已按字节安全截断”。

### 2.3 MySQL 模式：整数去 unsigned，保留常用映射

**原来：** [MySQL `getSchemaIntAndUnitType`][m-int] 根据字段大小选整数族，schema.Uint 追加 `unsigned`，自增再追加 `AUTO_INCREMENT`。

**现在：** [新 `mysqlIntegerType`][k-mysql-int] 按大小选 tinyint/smallint/mediumint/integer/bigint，int/uint 使用同一有符号物理类型，自增追加 AUTO_INCREMENT。代码说明针对金仓已发现的 `bigint unsigned AUTO_INCREMENT` 不兼容。

**示例：** 常见 `ID uint` 自增列从原方言的 `bigint unsigned AUTO_INCREMENT` 变为 `bigint AUTO_INCREMENT`。超过对应有符号上限的 uint 不在默认兼容承诺内。

**其他类型：** [新 MySQL 类型入口][k-mysql-types] 复用与原方言相近的 boolean、decimal/float/double、varbinary/mediumblob/longblob 映射；[`mysqlStringType`][k-mysql-string] 未设 size 时普通字符串使用 longtext，主键、默认值或其检测到的 INDEX/UNIQUE 标签场景用保守长度 191。与[原字符串映射][m-string] 的重要配置差异是：新方言不把 Config.DefaultStringSize 当作 MySQL 的全局字符串长度，它用于 Oracle。

### 2.4 MySQL 时间精度：保留默认毫秒，不改共享 Field

**原来：** [MySQL `getSchemaTimeType`][m-time] 在没有 precision 标签时向 `field.Precision` 写入默认值 3，并可能在类型字符串追加 NULL；[原 MySQL Apply][m-apply] 还设置 NowFunc 的时间舍入。

**现在：** [新 MySQL 时间分支][k-mysql-time] 直接返回 datetime(3)，显式正精度返回 datetime(n)，显式 precision:0 返回 datetime；不通过修改共享 Field 设置默认精度。空值属性交由通用迁移定义及 AlterColumn 处理。新 Apply 负责命名默认值，没有照搬原 MySQL NowFunc 配置。

### 2.5 Oracle 模式：整数族 identity 替代 NUMBER identity

**原来：** [Oracle `getIntegerType`][o-int] 将位宽换算为 NUMBER(n)，自增时追加 GENERATED BY DEFAULT AS IDENTITY；典型 64 位 ID 会得到 NUMBER(20) identity。

**现在：** [新 `oracleIntegerType`][k-oracle-int] 使用 tinyint/smallint/integer/bigint，自动主键追加 `GENERATED BY DEFAULT AS IDENTITY`，对应金仓 identity 只接受适用整数族的兼容边界。

**示例：** `ID uint` 的常见列定义变为 `bigint GENERATED BY DEFAULT AS IDENTITY`。不能把“Oracle 兼容模式”理解为可直接使用原生 Oracle 方言的全部类型。

### 2.6 Oracle 模式：布尔、数值、字符串和时间映射

**原来：** [Oracle DataTypeOf 及辅助函数][o-types] 的 bool 用 NUMBER(1)；无精度浮点用 FLOAT，有 precision 且 scale=0 时也可能用 FLOAT(p)；字符串用 VARCHAR2/CLOB；时间用 TIMESTAMP WITH TIME ZONE。

**现在：** [新 Oracle 类型入口][k-oracle-types] 中 bool 用 boolean，避免 gokb 返回数值后的布尔转换歧义；浮点有精度时统一 NUMBER(p,s)，否则 DOUBLE PRECISION；字符串未声明长度时采用 DefaultStringSize，1—4000 用 VARCHAR2(n)，其他情况 CLOB；时间可保留 precision:0 或正精度，使用 TIMESTAMP(n) WITH TIME ZONE；bytes 用 BLOB。

**显式类型补充：** [MySQL `mysqlCustomType`][k-mysql-custom] 和 [Oracle `oracleCustomType`][k-oracle-custom] 保留显式物理类型，按模式补自增属性，并避免重复追加相应关键字；实际类型能否建表仍由数据库验证。实现了 GormDBDataType 的自定义类型仍走核心优先入口。

### 2.7 Oracle 对象名称对应：逻辑名与目录名分开

**原来：** GORM 缓存模型 DBName，按该名字生成 SQL、匹配列。[Oracle 示例方言的大小写处理][o-case] 针对未引用名称返回大写的场景，建立大写结果名映射。

**现在：** [新 `catalogName`][k-catalog-name] 针对当前金仓 Oracle 实例的行为，仅把全大写名称转换为目录小写，混合大小写保留。它用于目录查询及结果标签适配；SQL 引用仍由 QuoteTo 负责。没有把共享 Schema 中 DBName 统一改小写。结果映射见 4.3，元信息映射见 5.4。

**现有验证入口：** [类型配置测试][t-types]、[命名默认测试][t-naming]、[需求模型映射示例][e-model]、[Oracle 大写模型回归][t-uppercase]。

## 3. SQL 构建（需求 6.2.3）

建议顺序：Statement.AddVar/Build → BindVarTo/QuoteTo → ClauseBuilders → 分页 → 两种 OnConflict。

### 3.1 新增按模式选择占位符和日志解释规则

**原来：** [GORM `Statement.AddVar`][g-addvar] 保存参数，再调用方言 BindVarTo；[MySQL BindVarTo][m-bind] 输出 `?`，[Oracle BindVarTo][o-bind] 输出 `:N`。[Statement.Build][g-build] 优先调用已注册 Clause Builder。

**现在：** [新 `BindVarTo`][k-bind] 在同一方法中按模式输出 `?` 或 `:` 加 `len(stmt.Vars)`；[新 `Explain`][k-explain] 分别使用普通问号匹配或 `:(\d+)` 匹配来生成日志显示文本。参数仍单独保存在 Statement.Vars，执行时没有把 Explain 文本当作业务 SQL。

### 3.2 新增双模式标识符引用公共实现

**原来：** [MySQL QuoteTo][m-quote] 用反引号，处理名称分段、已引用输入和内部引号；[Oracle QuoteTo][o-quote] 使用自己的双引号逻辑，并有 SkipQuoteIdentifiers 开关。

**现在：** [新 `QuoteTo/quoteIdentifier`][k-quote] 用同一套状态处理逻辑，按模式选择反引号或双引号。schema/table 各段引用、已引用名称和内部引用符转义都在此处处理；新 Config 没有照搬 Oracle 的 SkipQuoteIdentifiers。

**范围：** 对 clause.Table/Column 等结构化标识符生效；不能把任意 Where、Order、Raw 字符串都解释为会自动安全引用的名称。

### 3.3 Oracle 分页：覆盖 LIMIT，并修正默认排序

**原来：** [GORM `clause.Limit.Build`][g-limit] 输出 LIMIT/OFFSET 并绑定分页参数；[Oracle 示例分页构造][o-limit] 用 OFFSET/FETCH，缺省排序时直接使用未限定表的主键，或 ORDER BY 1。

**现在：** [新 `ClauseBuilders`][k-clauses] 仅为 Oracle 注册 LIMIT 覆盖；[`oracleLimitClauseBuilder`][k-oracle-limit] 保留已有 ORDER BY。无显式排序时，完整模型且非 Distinct、非分组、无显式 Select 的查询补“当前表.主键”；其他形态补 ORDER BY 1，避免 JOIN 的同名 id 歧义，以及投影/分组结果不包含主键的问题。OFFSET/FETCH 中的数值来自框架 int 参数，直接构造成十进制文本。

**范围：** MySQL 继续复用核心 LIMIT/OFFSET。Oracle 的补充排序并不保证所有业务查询分页稳定，业务仍应明确唯一排序。

### 3.4 MySQL 冲突忽略：保留真正 DoNothing

**原来：** [MySQL `ClauseBuilders`][m-conflict] 通常把抽象 OnConflict 改写成 ON DUPLICATE KEY UPDATE；没有更新赋值时，用主键或第一列自赋值。

**现在：** [新 `mysqlOnConflictClauseBuilder`][k-mysql-conflict] 的第 107—110 行对 DoNothing 直接调用原 clause.Build，保留 `ON CONFLICT DO NOTHING`。金仓 MySQL 模式的实现使用这一语法，而不是把所有场景硬套成原生 MySQL 更新。

**原因：** 在 RETURNING 扫描中，冲突忽略行与发生更新的行返回行为不同。把忽略变为空更新会让旧行返回，可能把旧 ID 填到同批次新关联对象。该修改既属于 SQL 构建，也支撑结果回填和关联保存。

### 3.5 MySQL 显式冲突目标和条件不被丢弃

**原来：** 原 MySQL ON DUPLICATE KEY UPDATE 对唯一键冲突生效，不能表达 GORM 的特定 Columns、OnConstraint 或冲突过滤条件语义。

**现在：** [新冲突构造中的保留分支][k-mysql-target] 遇到 OnConstraint、Columns、TargetWhere、Where 时保留原 ON CONFLICT。只有没有显式目标及条件的更新场景转为 ON DUPLICATE KEY UPDATE，`excluded.col` 转成 `VALUES(col)`。

**新增补充：** 对无更新赋值的该更新分支，优先用 Schema 主键/首列自赋值；没有 Schema 的 map 创建，从 VALUES 取第一列；没有任何可用列时返回构建错误。[新空赋值处理][k-empty-assignment] 原方言没有这一套无 Schema 回退。

### 3.6 Oracle 冲突更新：原 ON CONFLICT 配合唯一目标推断

**原来：** [Oracle 示例 `OnConflictClauseBuilder`][o-conflict] 在满足相应条件时构造 MERGE，缺少目标时先尝试主键；这是该原生 Oracle 方言的策略。

**现在：** [新 `oracleOnConflictClauseBuilder`][k-oracle-conflict] 保留 GORM ON CONFLICT，不引入 MERGE 重写。DoNothing、已指定约束名或列目标时直接 Build。无显式目标的更新场景，从主键、适用普通唯一索引、单列 unique 约束收集候选，去重后必须恰好一个，才填入 Columns；零个或多个返回错误。

**排除条件：** 带 Where 的部分唯一索引、表达式索引、前缀长度索引等不被当作可推断候选。只依据模型声明，不在此查询数据库补目标。新增逻辑主要补偿 GORM 关联更新对无主键模型可能留下的空目标。

### 3.7 空列 VALUES、DEFAULT 和 RETURNING 配置

**原来：** [原 MySQL VALUES 构造][m-conflict] 对空列输出 VALUES()；[原 Oracle DefaultValueOf][o-default] 返回空表达式，其 RETURNING Builder 还会追加 INTO 和 sql.Out。

**现在：** [新 `mysqlValuesClauseBuilder`][k-mysql-values] 保留 MySQL 空列 VALUES()；[新 `DefaultValueOf`][k-default] 统一返回 DEFAULT；[Initialize 的 callbackConfig][k-callbacks] 配置 Create/Update/Delete 的子句顺序，正常追加 RETURNING。新方言没有注册 Oracle RETURNING INTO Builder，使用 GORM 普通 Returning 子句；其执行和回填见下一项。

**另一个差异：** 与原 MySQL Update/Delete 子句中的 ORDER BY、LIMIT 不同，新方言配置 Update 为 UPDATE/SET/FROM/WHERE，Delete 为 DELETE/FROM/WHERE，再按配置追加 RETURNING。不能据此宣称原 MySQL 的更新/删除排序限量行为全部保留。

**现有验证入口：** [参数化测试][t-bind]、[引用测试][t-quote]、[分页测试][t-page]、[冲突测试][t-upsert]、[指定约束实库回归][t-named-conflict]、[需求 SQL 示例][e-sql]。

## 4. 数据操作与结果映射（需求 6.2.4）

建议顺序：核心 Create → RETURNING 与 LastInsertId 两条分支 → Initialize 配置 → Oracle 映射 → map 批次回填 → Translate。

### 4.1 CRUD、零值更新、软删除复用原回调

**原来：** [核心 `RegisterDefaultCallbacks`][g-callbacks] 注册创建、查询、更新、删除、关联、hooks 和默认事务；[Update/ConvertToAssignments][g-update] 处理结构体/map、零值选择；[Delete][g-delete] 与 [soft_delete.go][g-soft-delete] 处理删除及软删除。

**现在：** [新 Initialize][k-callbacks] 继续注册默认回调；标准结构体操作仍进入原流程。新增 create 包装内部也调用 `callbacks.Create(config)`，不是重新实现全部 CRUD。map 更新 0/false、Unscoped、ErrRecordNotFound 和缺少条件的保护来自原框架。

### 4.2 统一选择 RETURNING 结果集回填生成值

**原来：** [核心 Create 的能力判断与返回列选择][g-create] 由 CreateClauses 是否包含 RETURNING 决定。实际有返回列时，[第 80 行起][g-returning] 用 QueryContext + Scan；否则走 ExecContext，并可能调用 [LastInsertId][g-last-id]。原 MySQL 的 RETURNING 支持还受 [版本判断][m-init] 控制。原 Oracle 示例用 [专用 Create][o-create]、[ReturningClauseBuilder][o-returning] 等处理单条 INTO、批量 PL/SQL/输出参数。

**现在：** [新回调配置][k-callbacks] 对两个模式正常都开启 RETURNING，让核心针对数据库默认字段自动加返回列，用 QueryContext + Scan 回填。新方言没有采用 LastInsertId 主键推算，也没有采用 RETURNING INTO/PLSQL 输出参数机制。WithoutReturning 仅为排障开关，不能算正常回填方案。

**重要区分：** “支持 Update/Delete RETURNING”表示配置了可构建、扫描的路径；普通更新/删除不会因此一律自动返回所有列，需业务显式声明适用的 `clause.Returning`。

### 4.3 新增 Oracle 语句级列标签映射

**原来：** [核心 Scan][g-scan] 先应用 Statement.ColumnMapping，再匹配字段。原 Oracle 的 [MismatchedCaseHandler][o-case] 处理未引用名称返回大写的差异，主要注册在 Query。

**现在：** [新 `mapOracleColumns`][k-map-columns] 根据 catalogName 建立“物理列标签 → 模型逻辑列名”的语句级映射；复制已有 mapping 后补充，已有显式映射优先，不改共享 Schema，也不原地污染调用方 MapColumns 的 map。[初始化注册位置][k-map-register] 覆盖 Create/Query/Update/Delete/Row 的执行前阶段。

**关联 JOIN 补充：** 遍历 Statement.Joins，处理嵌套关系、显式 Alias，以及 GORM 的 `Relation__COLUMN` 标签。这样不仅 Find，Model(...).Scan 和 DML RETURNING 也能获得相应映射。原始无模型 SQL 仍需应用明确列别名或 MapColumns。

### 4.4 新增 map 切片创建回调包装

**原来：** [GORM Scan 的 `*[]map` 分支][g-map-scan] 为每个返回行构造一个新 map 并 append 到目的切片；它不会自动把 RETURNING 字段合并回已有输入 map。值形式 `[]map` 也没有同一个专用扫描分支。

**现在：** [新 `createWithMapReturning`][k-map-create] 识别 `[]map[string]interface{}` 和其指针；把输入切片浅复制到独立 backing array，临时把 Dest 设置为 `&batch` 后调用核心 Create。复制切片可防止 RETURNING 的 append 覆盖后续批次共用的输入容量；map 本身不复制，保留原引用。执行后还原 Dest，在后续关联及 hooks 前处理返回结果。[回调替换入口][k-map-register-create]

### 4.5 新增常规 map 批次 RETURNING 结果归并

**原来：** 对 n 个输入 map，追加扫描可能形成“n 个输入 + n 个返回 map”的 2n 结构。

**现在：** [新 `mergeMapReturning`][k-map-merge] 在无错误、非 DryRun、RowsAffected>0、无 ON CONFLICT 且长度恰为 2×RowsAffected 时，把后半部分逐条合并到前半部分，再截回 n 条。既保留输入字段，也回填主键等返回字段；[初始化还注册额外归并回调][k-map-register-merge]，安排在后置关联之前。

**边界：** 显式排除 OnConflict，不能把行数变化的冲突批次按位置强行归并。这里也不保证任意写法都自动选出主键返回列；仍需正确模型/表及 Returning 声明。

### 4.6 新增基于 gokb.SQLSTATE 的统一错误转换

**原来：** [GORM AddError][g-error] 在 TranslateError 开启时调用方言 Translate。[原 MySQL Translate][m-error] 识别 MySQLError.Number 的 1062、1451、1452，无法直接识别 gokb.Error。

**现在：** [新 errors.go][k-errors] 用 `errors.As` 找到可能被包装的 `*gokb.Error`，按 SQLSTATE 映射：

| SQLSTATE | 新返回错误 |
|---|---|
| 23505 | gorm.ErrDuplicatedKey |
| 23503 | gorm.ErrForeignKeyViolated |
| 23514 | gorm.ErrCheckConstraintViolated |
| 42703 | gorm.ErrInvalidField |

两种模式共享此表；未知代码或非 gokb 错误原样返回。已识别错误直接返回 GORM sentinel，不附带原始 gokb.Error 错误链；需原始诊断时不能假定还能 errors.As 回原错误。扫描错误、上下文错误与 SQLSTATE 错误也应区分。

**现有验证入口：** [CRUD/主键批次][t-crud]、[DoNothing 主键对应][t-donothing]、[map 创建变体][t-map]、[大写结果/RETURNING][t-uppercase]、[错误矩阵][t-errors]、[需求数据结果示例][e-data]。

## 5. 数据库结构迁移与元信息管理（需求 6.2.5）

建议顺序：核心 AutoMigrate → 新 Migrator 嵌入 → CurrentSchema → ColumnTypes → DDL → 索引/约束 → 已知错误边界。

### 5.1 保留 AutoMigrate 编排，新增 Migrator 实现

**原来：** [核心 AutoMigrate][g-automigrate] 排序模型、判断表、读取列、比较类型、创建字段/约束/索引；它通过 `tx.Migrator()` 调用方言实现。[原 MySQL Migrator][m-migrator-entry] 与 [原 Oracle Migrator][o-migrator-entry] 分别提供原生数据库实现。

**现在：** [新 `Dialector.Migrator`][k-migrator-entry] 返回嵌入通用 migrator.Migrator 的 [新 Migrator 结构][k-migrator]，将 CreateIndexAfterCreateTable 设为 true；本项目未另写 AutoMigrate。模型重排、FullDataTypeOf、MigrateColumn 比较等仍来自核心，覆盖部分见下文。

### 5.2 新增 database/schema 分离及限定名称解析

**原来：** [原 MySQL CurrentSchema][m-current-schema] 简单按点拆表名，未指定时采用 CurrentDatabase；[原 Oracle HasTable][o-has-table] 查 USER_TABLES，按原生 Oracle 用户对象语义处理。

**现在：** [新 CurrentDatabase][k-current-db] 用 SELECT CURRENT_DATABASE()；[新 CurrentSchema][k-current-schema] 优先解析显式 `schema.table` 或 Statement.TableExpr，否则以 `clause.Expr{SQL:"CURRENT_SCHEMA()"}` 作为目录过滤表达式。[splitQualifiedName/unquoteIdentifier][k-split] 按引号状态找到分隔点，处理引用符双写，不把被引用名称内部的点误判为 schema 分隔；再用 catalogName 归一化适用的 Oracle 名称。

**效果：** 不再将数据库名、用户名或固定 public 当作当前 schema，显式目标优先于连接当前 schema。

### 5.3 新增目标范围内的目录查询及 DryRun 查询辅助

**原来：** [核心 GetQueryAndExecTx][g-query-exec] 在迁移 DryRun 中区分“真实读取当前结构”和“打印 DDL”；原 MySQL/Oracle 的目录视图及过滤条件与金仓并不完全一致。

**现在：** [新 queryRaw][k-query] 对 DryRun 复制会话并关闭其 DryRun，确保元信息真实读取。[GetTables][k-tables] 查当前/指定 schema 的 BASE TABLE；[HasTable/HasColumn/HasConstraint][k-has] 分别查询 information_schema，先解析 GORM 字段或约束名；[HasIndex][k-has-index] 查 pg_catalog.pg_indexes；[TableType][k-table-type] 返回表 schema/name/type，不虚构表注释。

**边界：** 迁移 DryRun 仍需可用数据库及目录权限；不等同于离线生成 SQL。

### 5.4 重写列元信息读取并分离双模式 SQL

**原来：** [核心 ColumnTypes][g-columns] 主要依赖 rows.ColumnTypes；[原 MySQL ColumnTypes][m-columns] 合并 sql.ColumnType 与 MySQL 专属 column_type/column_key/extra/column_comment；[原 Oracle ColumnTypes][o-columns] 查询 ROWNUM=1 后使用驱动列元信息。

**现在：** [新 ColumnTypes][k-columns] 直接读取金仓目录，构造 migrator.ColumnType。来源按模式分开：

| 新源码 | 实际处理 |
|---|---|
| [migrator_mysql.go:6][k-mysql-metadata] | information_schema.columns 联合 pg_attribute/pg_description；extra 缺失时以 attidentity 识别自增；适用字符长度为 -1 时用 atttypmod 补足 |
| [migrator_oracle.go:5][k-oracle-metadata] | 用 is_identity 识别 identity，读取 Oracle 模式的列属性；通过 pg_description 取注释 |
| [migrator_columns.go:40][k-column-fields] | 逐列扫描类型、长度、精度、标度、时间精度、默认值、空值、自增、注释，构造完整类型名称 |
| [migrator_columns.go:91][k-column-keys] | 另查 PRIMARY KEY/UNIQUE；只有单列唯一约束标为单列 Unique，避免把复合唯一错误地解释为每列唯一 |
| [migrator_columns.go:123][k-column-logical] | 约束匹配完成后，仅对模型已声明字段将物理目录列名映回模型 DBName，供 AutoMigrate 精确匹配；原始表名查询保留物理名 |

**新增缺表处理：** 列查询为空时再次检查表目录；确实不存在时返回错误，避免“缺表”和合法空集合混淆。[缺表分支][k-missing-table]

### 5.5 新增元信息补齐，避开核心空指针回退

**原来：** [v1.31.1 ColumnType.Length/DecimalSize/ScanType][g-column-access] 在相应缓存字段未设置时直接访问 SQLColumnType。新实现直接构造目录元信息，没有必然存在的 `*sql.ColumnType`。

**现在：** [completeColumnTypeMetadata][k-complete-columns] 对不适用/缺失长度、精度、标度补 Valid 的 0，对 ScanType 用 [scanTypeForDatabaseType][k-scan-type] 提供反射类型；[buildColumnTypeName][k-column-name] 重建带长度或精度的类型表示。不修改核心访问器。

**准确解释：** 这些 0 是“当前元信息不适用/未给出相应尺寸”的兼容表示，不是数据库实际创建了零长度。ScanType 对数值类型的推断也不等于任意 NUMBER 业务值都应以 float64 无损接收。

### 5.6 新增默认值归一化和类型别名

**原来：** [核心 MigrateColumn][g-migrate-column] 比较模型类型、默认值等，调用方言 GetTypeAliases；原 MySQL 对目录默认值主要做外层单引号处理。

**现在：** [ColumnTypes 自增判断][k-column-identity] 同时看 auto_increment、YES、nextval；identity 的内部默认表达式不参与业务 default 比较。[normalizeDefaultValue/topLevelTypeCastIndex][k-normalize-default] 仅裁去引号和括号外的顶层 `::type`，还原单引号转义，保留 `'scheme::value'` 内部冒号及函数内部转换。[GetTypeAliases/kingbaseTypeAliases][k-alias] 增加 integer/int4、varchar2/character varying、number/numeric、blob/bytea 等对应，减少相同物理类型的误迁移。

### 5.7 创建表、新增列与字段注释

**原来：** [核心 CreateTable][g-create-table] 构造列、主键、约束并按配置创建索引；[原 MySQL FullDataTypeOf][m-full-type] 在类型定义后追加 COMMENT。

**现在：** [新 CreateTable][k-create-table] 按依赖顺序处理外键引用的唯一约束，再调用通用 CreateTable，成功后逐列写注释；[新 AddColumn][k-add-column] 先复用通用新增列，再补注释；[commentColumn][k-comment] 用 `COMMENT ON COLUMN` 单独维护注释。注释文本通过 Explain 进行 SQL 字面量转义，字段名通过结构化标识符引用；空注释生成 IS NULL。

### 5.8 修改列：模式 MODIFY 与独立 DEFAULT 操作

**原来：** [核心 AlterColumn][g-alter-column] 输出 ALTER COLUMN TYPE；[原 MySQL AlterColumn][m-alter-column] 用 MODIFY COLUMN 加 FullDataTypeOf。

**现在：** [新 AlterColumn][k-alter-column] 先验证字段；MySQL 用 MODIFY COLUMN，Oracle 用 MODIFY。显式拼 NULL 或 NOT NULL，确保能解除已有非空限制；非自增字段另用 ALTER COLUMN SET DEFAULT / DROP DEFAULT，再更新注释。默认值不强塞进 Oracle MODIFY，避免其语法边界；自增列跳过常规默认值改写。

### 5.9 删除表：按模式输出级联语法

**原来：** [原 MySQL DropTable][m-drop-table] 在固定连接上切换 FOREIGN_KEY_CHECKS 后删除；核心采用通用删除 SQL。

**现在：** [新 DropTable][k-drop-table] 逆依赖顺序删除；Oracle 先 HasTable，再执行 DROP TABLE … CASCADE CONSTRAINTS；MySQL 执行 DROP TABLE IF EXISTS … CASCADE。未照搬原 MySQL 的会话级外键检查开关。Oracle 的预检查也受 5.14 所述 Has* 错误边界影响。

### 5.10 重写索引创建和注释

**原来：** [核心 CreateIndex][g-create-index] 构造通用索引语句，其 USING 布局等不完全对应目标模式。

**现在：** [新 CreateIndex][k-create-index] 复用 BuildIndexOptions 生成有序字段项，再按金仓语法把 USING 放在键列表前、WHERE 放在选项后。MySQL 使用转义后的行内 COMMENT；Oracle 通过事务顺序执行 CREATE INDEX 与 COMMENT ON INDEX，第二步失败时请求回滚该步骤。[indexIdentifier][k-index-name] 为独立索引操作保留显式 schema，并逐段转义点和引号；没有依赖修改连接的 search_path 来定位索引。

**边界：** 索引方法、Where、Option 等模型 SQL 结构须由可信代码提供并满足目标实例语法。事务包装是实现动作，不能外推为所有 DDL 在所有环境下都保证可回滚。

### 5.11 重写索引删除和重命名

**原来：** [核心 DropIndex][g-drop-index] 使用 DROP INDEX … ON table；[原 MySQL RenameIndex][m-rename-index] 正常分支用 ALTER TABLE … RENAME INDEX … TO；原 Oracle 对独立索引名操作。

**现在：** [新 DropIndex][k-drop-index] 先查同名 UNIQUE 约束，有则 ALTER TABLE DROP CONSTRAINT，否则按带 schema 的独立索引名 DROP INDEX；[新 RenameIndex][k-rename-index] 两种模式均用 ALTER INDEX … RENAME TO，并保留旧索引目标 schema。

**原因：** 金仓 MySQL 模式不等于原生 MySQL 索引 DDL；某些唯一索引已成为约束的支撑对象，不能按普通索引直接删。

### 5.12 新增/调整唯一约束与非主键外键引用处理

**原来：** [核心 MigrateColumnUnique][g-unique] 主要根据 ColumnType.Unique 决定变化；原 MySQL 有自己的唯一索引处理；[核心 CreateConstraint][g-create-constraint] 直接根据关系定义创建约束。

**现在：** [新 MigrateColumnUnique][k-unique] 按 GORM 命名的具体 UNIQUE 约束判断创建/删除，区分 unique 与 uniqueIndex。[ensureReferenceUniqueConstraint][k-reference-unique] 对非主键外键引用，寻找字段和顺序完全相同的、非部分/表达式/前缀普通唯一索引，按需用 `ADD CONSTRAINT … UNIQUE USING INDEX …` 将已有索引附着为约束。[新 CreateConstraint][k-create-constraint] 和 CreateTable 都接入此检查。

**范围：** 不为模型中非唯一字段凭空增加唯一性，不承诺任意自定义引用都能建立物理外键。

### 5.13 新增索引详情查询和有序聚合

**原来：** [原 MySQL GetIndexes][m-indexes] 读取 information_schema.STATISTICS 并按索引分组；原 Oracle 的 HasIndex 使用 USER_INDEXES。

**现在：** [新 GetIndexes/indexMetadataSQL][k-indexes] 读取 pg_index/pg_class/pg_namespace，按 indkey 的 ordinality 保留键项顺序；普通键返回列名，表达式键调用 pg_get_indexdef；聚合成 migrator.Index，返回唯一和主键标记。

**范围：** 接口未完整输出索引谓词、所有选项和完整 DDL，不能把 Columns() 当作完整索引定义。

### 5.14 当前仍存在的错误传播与迁移边界

**直接代码证据：** [HasTable 等][k-has]、[HasIndex][k-has-index] 用 `_ = m.RunWithValue(...)` 丢弃返回错误；[CurrentDatabase][k-current-db] 忽略 Scan.Error。查询失败时可能返回 false 或空字符串。核心 [AutoMigrate][g-automigrate] 用 `!HasTable` 决定是否建表，当前不存在一个已实现的统一错误阻断层。

带错误返回值的 ColumnTypes/GetIndexes/GetTables/TableType 能向调用方返回查询/扫描等错误，但 ColumnTypes、GetIndexes 的部分 `defer rows.Close()` 没有检查关闭错误，不宜写成“所有读取和关闭失败都完整传递”。

附件另记录：已有表新增 unique 列后可能需显式 CreateConstraint；同模型 AutoMigrate 应串行执行；复杂类型转换、删列和回退不属于无条件自动保障。这些应在修改文档的限制中保留，不能用已有幂等示例代替全部迁移验证。

**现有验证入口：** [元信息测试][t-metadata]、[DDL 模式测试][t-ddl]、[schema 隔离][t-schema]、[迁移幂等][t-migration]、[元信息契约回归][t-meta-contract]、[需求迁移示例][e-migration]。

## 6. 事务管理（需求 6.2.6）

建议顺序：核心 Transaction → Begin/Commit/Rollback → 核心 SavePoint → 新保存点实现 → 注入连接池恢复。

### 6.1 手动、回调、嵌套事务沿用核心

**原来：** [Transaction][g-transaction] 对普通连接 Begin，回调无错误时提交，错误/异常时请求回滚；嵌套时在默认配置下建保存点。[Begin][g-begin] 通过 TxBeginner/ConnPoolBeginner 调用 BeginTx，并传 Context；[Commit/Rollback][g-commit] 调用事务接口。[默认事务回调][g-default-tx] 给数据操作提供标准事务流程。

**现在：** 没有新增 Kingbase 事务引擎。新方言依靠 gokb/database/sql 实现底层事务，保留以上核心逻辑；[Initialize 成功时恢复原池][k-restore] 使注入池的事务接口不被包装器永久遮蔽。

### 6.2 保存点：从原始拼接改为结构化标识符

**原来：** [MySQL SavePoint/RollbackTo][m-savepoint] 与 [Oracle SavePoint/RollbackTo][o-savepoint] 将名称直接拼到 SQL 字符串。GORM [SavePointerDialectorInterface][g-savepoint-interface] 定义扩展接口，核心 SavePoint/RollbackTo 调用它并处理预编译事务包装。

**现在：** [新 SavePoint][k-savepoint]、[新 RollbackTo][k-rollback-to] 先拒绝空或全空白名称，再分别执行 `SAVEPOINT ?`、`ROLLBACK TO SAVEPOINT ?`，参数为 `clause.Column{Name:name}`。

**为什么这里的 ? 不是值绑定：** GORM 对 clause.Column 使用 QuoteTo 写成结构化标识符，因此 MySQL 输出反引号、Oracle 输出双引号，名称不直接拼接为任意 SQL。不会把保存点名称作为普通字符串值发给数据库。

### 6.3 嵌套、上下文和错误语义的文档边界

**共用核心位置：** [Transaction 嵌套分支][g-transaction]、[Begin 的 Context 和可选接口判断][g-begin]、[核心 SavePoint/RollbackTo][g-core-savepoint]。

**适配作用：** 核心自动生成的 sp 名称也走新的引用逻辑；业务错误继续由原回调事务流程返回。保存点不是独立提交；DisableNestedTransaction 会改变行为；主键已回填也不能证明事务已提交。上下文取消、提交阶段中断的最终状态需按实际返回和独立查询核对，不能宣称方言新增了自动恢复或写入重试。

**现有验证入口：** [保存点名称测试][t-savepoint]、[真实库事务测试][t-tx]、[需求事务示例][e-tx]。

## 7. 关联关系管理（需求 6.2.7）

建议顺序：关系解析 → 关联保存顺序 → OnConflict 生成 → 主键回填 → Preload/Joins → Association API → 非主键引用迁移。

### 7.1 四类关系解析与 Association API 复用原实现

**原来：** [schema.parseRelation][g-relations]、buildMany2ManyRelation 等解析 Belongs To/Has One/Has Many/Many To Many；[Association API][g-association] 实现 Find、Append、Replace、Delete、Clear、Count。[Preload][g-preload] 查询后按身份映射装配关联；[BuildQuerySQL][g-query] 构建 Joins 和关联别名。

**现在：** 没有新增 kingbase_association 引擎或改写上述算法。新代码通过前六项提供正确的建表、字段类型、SQL、主键和结果列映射，使这些原 API 在双模式工作。修改文档应把这些原位置写为“复用代码入口”，不是“已修改原框架文件”。

### 7.2 关联主外键传递依赖 RETURNING

**原来：** [SaveBeforeAssociations][g-save-before] 先保存 Belongs To，再把主键传给源对象；[SaveAfterAssociations][g-save-after] 保存 Has One/Has Many 和多对多，依赖父对象和关联对象主键构造外键、中间表。

**现在：** [正常 RETURNING 配置][k-callbacks] 和 [核心 RETURNING Scan][g-returning] 确保适用的生成值在后置关联回调前可用；Oracle 的 [列映射][k-map-columns] 同时覆盖关联对象创建返回列。新增工作是补足“主键产生并正确回填”这一依赖，没有重写主外键复制算法。

### 7.3 关联保存中的冲突语义修补

**原来：** [onConflictOption][g-rel-conflict] 在默认不更新关联字段时使用 DoNothing；有默认更新列或 FullSaveAssociations 时使用更新，并从主键构造目标。无主键模型可能留下空目标。[saveAssociations][g-save-assoc] 将该子句带到普通 Create。

**现在：** [MySQL 冲突构造][k-mysql-conflict] 保留 DoNothing，避免旧行 RETURNING 与新对象回填错位；[Oracle 冲突构造][k-oracle-conflict] 对空目标仅在唯一候选键可判定时推断，不随意选择冲突键。这两段代码集中在 clause.go，但其业务影响包括关联保存和多对多中间表。

### 7.4 Oracle 关联 JOIN 结果列适配

**原来：** 核心 [BuildQuerySQL][g-query] 生成类似 `Relation__column` 的标签，[Scan][g-scan] 解析嵌套关系字段。

**现在：** [mapOracleColumns 中的 Joins 遍历][k-map-joins] 把适用的物理别名映回逻辑嵌套标签，覆盖嵌套关联和最后一段显式 Alias；同时不改缓存 Schema。此处是原有结果装配机制的输入修补。

### 7.5 非主键引用的物理约束适配

**原来：** [关系 ParseConstraint/Constraint.Build][g-rel-constraint] 生成引用字段定义；核心 Migrator 据此创建外键，不自动按金仓规则把独立唯一索引附着成约束。

**现在：** [ensureReferenceUniqueConstraint][k-reference-unique]、[CreateConstraint][k-create-constraint]、[CreateTable][k-create-table] 按 5.12 的范围补足约束。未声明唯一性、字段/顺序不匹配、部分或表达式索引不在该自动附着范围。

### 7.6 解除关系与错误继续遵循 GORM

**原来和现在共用：** [Association Delete/Clear/Count][g-assoc-delete] 等负责置空外键或删除中间表记录；不存在的关系名由 Association 返回 ErrUnsupportedRelation。默认解除关系不等于删除目标实体，Count=0 也不能覆盖 Association.Error。

**适配范围：** 普通四类关系、集合 Preload、已验证的单值关系 Joins 与完整复杂关联矩阵应分别描述。可空外键、Has One 是否附加唯一约束、自定义引用、复合键、跨 schema、Select/Omit/FullSaveAssociations 组合需要对应测试证据。

**现有验证入口：** [关联/Preload 实库测试][t-assoc]、[Oracle 大写关联回归][t-upper-assoc]、[自定义无物理外键多对多测试][t-nonunique-assoc]、[需求关联示例][e-assoc]。

## 8. 全部生产文件清单与修改文档写法

| 文件 | 主要内容 | 主归属 |
|---|---|---|
| `kingbase.go` | Config/Dialector/Open/New/Name；命名默认；Initialize；DSN 检查；Migrator；默认值和类型分派；保存点 | 1、2、3、4、5、6 |
| `mode.go` | Mode、配置归一化、模式值严格解析 | 1 |
| `quote_bind.go` | 占位符、双模式引用、日志 SQL 解释 | 3 |
| `mysql_types.go` | MySQL 字段物理类型、自增、自定义类型 | 2 |
| `oracle_types.go` | Oracle 字段物理类型、整数 identity、自定义类型 | 2 |
| `clause.go` | 双模式 Builder 注册；VALUES、OnConflict、Oracle LIMIT | 3，影响 4、7 |
| `oracle_names.go` | 目录名称归一化、语句列标签与 JOIN 映射 | 2、4、5、7 |
| `map_returning.go` | map 切片创建包装及常规 RETURNING 归并 | 4 |
| `errors.go` | gokb.SQLSTATE → GORM 错误 | 4 |
| `migrator.go` | 迁移实例、schema、表、列、索引、约束、别名和元信息入口 | 5，影响 7 |
| `migrator_columns.go` | 列与约束元信息、类型补齐、默认值归一化 | 5 |
| `migrator_mysql.go` | MySQL 模式列元信息 SQL | 5 |
| `migrator_oracle.go` | Oracle 模式列元信息 SQL | 5 |

阶段性 PoC、历史设计及测试材料已在开发工作区归档，不是本仓库的运行入口。SQLServer 不在本次双模式范围。`scripts/` 中的 GORM 测试准备脚本及 patch 属于测试接入材料，不能算生产核心改动。历史快照可能对应不同版本，本文函数说明以本仓库源码为准。

正式修改说明可以把每个差异点固定写为：

> **需求归属：** 6.2.x。**原处理：** 原函数位置、执行机制。**适配原因：** 在金仓对应模式下的具体差异。**新增/覆盖：** 新函数位置、接入扩展点、处理分支。**行为变化：** SQL 或结果的前后对照。**验证与边界：** 对应用例、实际证据、未覆盖范围。

可直接采用的总体描述：

> 本次以独立 KingbaseES Dialector 和 Migrator 扩展 GORM v2，底层统一调用 gokb。保留 GORM 的会话、模型解析、CRUD 编排、事务和关联处理机制，新增兼容模式识别、双模式字段类型与 SQL 构建、RETURNING 回填及结果列映射、SQLSTATE 错误转换和迁移目录适配。原生 MySQL、Oracle 方言仅作为行为比较基线，生产模块不依赖对原框架源码的修改。

## 9. 本轮核对范围

本轮完成需求正文提取、13 个方言生产 Go 文件的源码阅读、相关核心及原方言调用路径对照、实际模块依赖核实和缓存源码/下载归档一致性核查。测试链接用于定位已有验证入口；本轮未运行测试、未连接数据库，不新增“双模式实库通过”的结论。本文新增为文档，未调整生产实现。

<!-- 以下引用对应 2026-10-28 当前源码行号。 -->
[k-mod]: ../go.mod#L1
[k-driver]: ../kingbase.go#L21
[k-config]: ../kingbase.go#L32
[k-state]: ../kingbase.go#L55
[k-borrow]: ../kingbase.go#L72
[k-open]: ../kingbase.go#L77
[k-get-mode]: ../kingbase.go#L90
[k-apply]: ../kingbase.go#L112
[k-validate-mode]: ../kingbase.go#L142
[k-conn]: ../kingbase.go#L151
[k-close]: ../kingbase.go#L165
[k-detect]: ../kingbase.go#L173
[k-check-dsn]: ../kingbase.go#L196
[k-callbacks]: ../kingbase.go#L203
[k-map-register]: ../kingbase.go#L214
[k-map-register-create]: ../kingbase.go#L240
[k-map-register-merge]: ../kingbase.go#L248
[k-restore]: ../kingbase.go#L258
[k-dsn-parser]: ../kingbase.go#L266
[k-migrator-entry]: ../kingbase.go#L285
[k-default]: ../kingbase.go#L296
[k-data-type]: ../kingbase.go#L300
[k-savepoint]: ../kingbase.go#L307
[k-rollback-to]: ../kingbase.go#L317
[k-mode]: ../mode.go#L13
[k-parse-mode]: ../mode.go#L30
[k-mysql-types]: ../mysql_types.go#L11
[k-mysql-int]: ../mysql_types.go#L51
[k-mysql-string]: ../mysql_types.go#L75
[k-mysql-time]: ../mysql_types.go#L27
[k-mysql-custom]: ../mysql_types.go#L95
[k-oracle-types]: ../oracle_types.go#L10
[k-oracle-int]: ../oracle_types.go#L44
[k-oracle-custom]: ../oracle_types.go#L65
[k-catalog-name]: ../oracle_names.go#L12
[k-map-columns]: ../oracle_names.go#L22
[k-map-joins]: ../oracle_names.go#L47
[k-bind]: ../quote_bind.go#L16
[k-quote]: ../quote_bind.go#L25
[k-explain]: ../quote_bind.go#L84
[k-clauses]: ../clause.go#L13
[k-oracle-conflict]: ../clause.go#L30
[k-mysql-values]: ../clause.go#L88
[k-mysql-conflict]: ../clause.go#L101
[k-mysql-target]: ../clause.go#L111
[k-empty-assignment]: ../clause.go#L121
[k-oracle-limit]: ../clause.go#L174
[k-map-create]: ../map_returning.go#L12
[k-map-merge]: ../map_returning.go#L49
[k-errors]: ../errors.go#L10
[k-migrator]: ../migrator.go#L17
[k-query]: ../migrator.go#L24
[k-current-db]: ../migrator.go#L35
[k-current-schema]: ../migrator.go#L43
[k-split]: ../migrator.go#L55
[k-tables]: ../migrator.go#L116
[k-has]: ../migrator.go#L125
[k-unique]: ../migrator.go#L174
[k-has-index]: ../migrator.go#L198
[k-create-table]: ../migrator.go#L215
[k-reference-unique]: ../migrator.go#L262
[k-create-constraint]: ../migrator.go#L301
[k-add-column]: ../migrator.go#L314
[k-comment]: ../migrator.go#L330
[k-alter-column]: ../migrator.go#L347
[k-drop-table]: ../migrator.go#L405
[k-index-name]: ../migrator.go#L428
[k-create-index]: ../migrator.go#L446
[k-drop-index]: ../migrator.go#L496
[k-rename-index]: ../migrator.go#L520
[k-alias]: ../migrator.go#L531
[k-indexes]: ../migrator.go#L565
[k-table-type]: ../migrator.go#L624
[k-columns]: ../migrator_columns.go#L14
[k-column-fields]: ../migrator_columns.go#L40
[k-column-identity]: ../migrator_columns.go#L58
[k-missing-table]: ../migrator_columns.go#L75
[k-column-keys]: ../migrator_columns.go#L91
[k-column-logical]: ../migrator_columns.go#L123
[k-complete-columns]: ../migrator_columns.go#L143
[k-scan-type]: ../migrator_columns.go#L158
[k-column-name]: ../migrator_columns.go#L178
[k-normalize-default]: ../migrator_columns.go#L195
[k-mysql-metadata]: ../migrator_mysql.go#L6
[k-oracle-metadata]: ../migrator_oracle.go#L5
[g-open]: https://github.com/go-gorm/gorm/blob/v1.31.1/gorm.go#L132
[g-init-cleanup]: https://github.com/go-gorm/gorm/blob/v1.31.1/gorm.go#L204
[g-default-name]: https://github.com/go-gorm/gorm/blob/v1.31.1/gorm.go#L172
[g-session]: https://github.com/go-gorm/gorm/blob/v1.31.1/gorm.go#L250
[g-context]: https://github.com/go-gorm/gorm/blob/v1.31.1/gorm.go#L352
[g-error]: https://github.com/go-gorm/gorm/blob/v1.31.1/gorm.go#L394
[g-db]: https://github.com/go-gorm/gorm/blob/v1.31.1/gorm.go#L412
[g-prepared]: https://github.com/go-gorm/gorm/blob/v1.31.1/prepare_stmt.go#L1
[g-savepoint-interface]: https://github.com/go-gorm/gorm/blob/v1.31.1/interfaces.go#L41
[g-schema]: https://github.com/go-gorm/gorm/blob/v1.31.1/schema/schema.go#L133
[g-field]: https://github.com/go-gorm/gorm/blob/v1.31.1/schema/field.go#L106
[g-format-name]: https://github.com/go-gorm/gorm/blob/v1.31.1/schema/naming.go#L98
[g-addvar]: https://github.com/go-gorm/gorm/blob/v1.31.1/statement.go#L175
[g-build]: https://github.com/go-gorm/gorm/blob/v1.31.1/statement.go#L493
[g-limit]: https://github.com/go-gorm/gorm/blob/v1.31.1/clause/limit.go#L15
[g-callbacks]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/callbacks.go#L22
[g-create]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/create.go#L37
[g-returning]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/create.go#L80
[g-last-id]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/create.go#L142
[g-update]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/update.go#L56
[g-delete]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/delete.go#L113
[g-soft-delete]: https://github.com/go-gorm/gorm/blob/v1.31.1/soft_delete.go#L1
[g-scan]: https://github.com/go-gorm/gorm/blob/v1.31.1/scan.go#L126
[g-map-scan]: https://github.com/go-gorm/gorm/blob/v1.31.1/scan.go#L166
[g-connection]: https://github.com/go-gorm/gorm/blob/v1.31.1/finisher_api.go#L598
[g-transaction]: https://github.com/go-gorm/gorm/blob/v1.31.1/finisher_api.go#L622
[g-begin]: https://github.com/go-gorm/gorm/blob/v1.31.1/finisher_api.go#L665
[g-commit]: https://github.com/go-gorm/gorm/blob/v1.31.1/finisher_api.go#L701
[g-core-savepoint]: https://github.com/go-gorm/gorm/blob/v1.31.1/finisher_api.go#L722
[g-default-tx]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/transaction.go#L7
[g-mig-type]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L78
[g-full-type]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L90
[g-query-exec]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L110
[g-automigrate]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L121
[g-create-table]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L215
[g-alter-column]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L411
[g-migrate-column]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L469
[g-unique]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L594
[g-columns]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L615
[g-create-constraint]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L757
[g-create-index]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L830
[g-drop-index]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/migrator.go#L865
[g-column-access]: https://github.com/go-gorm/gorm/blob/v1.31.1/migrator/column_type.go#L62
[g-relations]: https://github.com/go-gorm/gorm/blob/v1.31.1/schema/relationship.go#L65
[g-rel-constraint]: https://github.com/go-gorm/gorm/blob/v1.31.1/schema/relationship.go#L638
[g-association]: https://github.com/go-gorm/gorm/blob/v1.31.1/association.go#L21
[g-assoc-delete]: https://github.com/go-gorm/gorm/blob/v1.31.1/association.go#L199
[g-preload]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/preload.go#L185
[g-query]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/query.go#L36
[g-save-before]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/associations.go#L13
[g-save-after]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/associations.go#L110
[g-rel-conflict]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/associations.go#L360
[g-save-assoc]: https://github.com/go-gorm/gorm/blob/v1.31.1/callbacks/associations.go#L378
[m-open]: https://github.com/go-gorm/mysql/blob/v1.6.0/mysql.go#L69
[m-apply]: https://github.com/go-gorm/mysql/blob/v1.6.0/mysql.go#L96
[m-init]: https://github.com/go-gorm/mysql/blob/v1.6.0/mysql.go#L110
[m-conflict]: https://github.com/go-gorm/mysql/blob/v1.6.0/mysql.go#L204
[m-migrator-entry]: https://github.com/go-gorm/mysql/blob/v1.6.0/mysql.go#L276
[m-bind]: https://github.com/go-gorm/mysql/blob/v1.6.0/mysql.go#L288
[m-quote]: https://github.com/go-gorm/mysql/blob/v1.6.0/mysql.go#L292
[m-string]: https://github.com/go-gorm/mysql/blob/v1.6.0/mysql.go#L391
[m-time]: https://github.com/go-gorm/mysql/blob/v1.6.0/mysql.go#L416
[m-int]: https://github.com/go-gorm/mysql/blob/v1.6.0/mysql.go#L469
[m-savepoint]: https://github.com/go-gorm/mysql/blob/v1.6.0/mysql.go#L508
[m-error]: https://github.com/go-gorm/mysql/blob/v1.6.0/error_translator.go#L16
[m-full-type]: https://github.com/go-gorm/mysql/blob/v1.6.0/migrator.go#L40
[m-alter-column]: https://github.com/go-gorm/mysql/blob/v1.6.0/migrator.go#L147
[m-rename-index]: https://github.com/go-gorm/mysql/blob/v1.6.0/migrator.go#L250
[m-drop-table]: https://github.com/go-gorm/mysql/blob/v1.6.0/migrator.go#L292
[m-columns]: https://github.com/go-gorm/mysql/blob/v1.6.0/migrator.go#L308
[m-indexes]: https://github.com/go-gorm/mysql/blob/v1.6.0/migrator.go#L424
[m-current-schema]: https://github.com/go-gorm/mysql/blob/v1.6.0/migrator.go#L483
[o-config]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/oracle.go#L62
[o-init]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/oracle.go#L92
[o-migrator-entry]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/oracle.go#L141
[o-types]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/oracle.go#L154
[o-int]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/oracle.go#L206
[o-default]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/oracle.go#L244
[o-bind]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/oracle.go#L251
[o-quote]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/oracle.go#L257
[o-savepoint]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/oracle.go#L288
[o-case]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/query.go#L90
[o-returning]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/clause_builder.go#L118
[o-limit]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/clause_builder.go#L241
[o-conflict]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/clause_builder.go#L282
[o-create]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/create.go#L79
[o-has-table]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/migrator.go#L252
[o-columns]: https://github.com/oracle-samples/gorm-oracle/blob/v1.1.2/oracle/migrator.go#L396
[kb-append-returning]: development.md#gokb-returning
[t-mode]: ../compatibility_test.go#L120
[t-ownership]: ../compatibility_test.go#L199
[t-dsn]: ../compatibility_test.go#L236
[t-bind]: ../compatibility_test.go#L259
[t-quote]: ../compatibility_test.go#L290
[t-page]: ../compatibility_test.go#L311
[t-upsert]: ../compatibility_test.go#L331
[t-savepoint]: ../compatibility_test.go#L377
[t-types]: ../compatibility_test.go#L411
[t-metadata]: ../compatibility_test.go#L471
[t-ddl]: ../compatibility_test.go#L603
[t-schema]: ../compatibility_test.go#L696
[t-errors]: ../compatibility_test.go#L777
[t-naming]: ../compatibility_test.go#L796
[t-crud]: ../integration/acceptance_test.go#L105
[t-tx]: ../integration/acceptance_test.go#L218
[t-donothing]: ../integration/acceptance_test.go#L379
[t-assoc]: ../integration/acceptance_test.go#L442
[t-migration]: ../integration/acceptance_test.go#L572
[t-uppercase]: ../integration/submission_regression_test.go#L24
[t-upper-assoc]: ../integration/submission_regression_test.go#L124
[t-map]: ../integration/submission_regression_test.go#L247
[t-meta-contract]: ../integration/metadata_contract_test.go#L16
[t-named-conflict]: ../integration/named_conflict_test.go#L20
[t-nonunique-assoc]: ../integration/nonunique_association_test.go#L32
[e-connect]: ../docs/examples/connection-session/main.go#L49
[e-model]: ../docs/examples/model-mapping/main.go#L84
[e-sql]: ../docs/examples/sql-building/main.go#L28
[e-data]: ../docs/examples/data-results/main.go#L32
[e-migration]: ../docs/examples/migration-metadata/main.go#L73
[e-tx]: ../docs/examples/transaction-management/main.go#L55
[e-assoc]: ../docs/examples/association-management/main.go#L89
