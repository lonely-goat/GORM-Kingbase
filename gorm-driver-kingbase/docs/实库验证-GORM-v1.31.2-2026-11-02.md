# GORM v1.31.2 正式升级与实库验证（2026-11-02）

当前分支为 `kingbase-driver-gorm-v1.31.2`。运行时依赖、固定官方测试源码归档及补丁均已同步到 **GORM v1.31.2**；配套 gokb 原始源码保持不变。旧版 `kingbase-driver` 分支及[历史 v1.31.1 实测记录](实库验证-2026-11-02.md)保留原版本信息。

## 升级内容

- 更新 `go.mod`、`go.sum` 和 `scripts/gorm-baseline.json`，将固定官方源码归档替换为 v1.31.2。
- 新归档 SHA-256 为 `a7d1609fb4508dedf764f81bc366541aada8991f0b37b3c78e9bca0bca6aac7a`，运行器会核对摘要与依赖版本。
- 重新生成三份官方测试补丁，针对新基线精确应用，禁用模糊匹配。没有改写官方生产核心源码。
- 启用上游新增的 `TestCreateInBatchesWithReturning`，两种模式均实际通过。
- 新增 [gorm_upgrade_test.go](../integration/gorm_upgrade_test.go) 四组双模式测试：泛型 Row/Rows 模型推导、WithResult 错误与复用、括号内命名切片/数组/空集合、相同默认值下的 integer → bigint 迁移与数据库 DEFAULT 保留。
- 复核并保留 map RETURNING 包装及列元信息补全，更新注释解释它们与新核心的关系；适配器生产行为没有为本次升级另作修改。
- 更新源码说明、维护导读、文件清单和交付 manifest，旧版报告仍标注 v1.31.1。

## 实测结果

环境为 Linux amd64、Go 1.26.2、KingbaseES V009R001C010、UTF8。

| 验证入口 | MySQL 模式 | Oracle 模式 |
|---|---|---|
| GORM → Dialector → gokb → 实例 | 通过 | 通过 |
| 自有集成测试，含竞态检测 | 39 通过、0 失败、0 跳过 | 37 通过、0 失败、2 跳过 |
| 固定 GORM 核心测试 | 100 通过、0 失败 | 100 通过、0 失败 |
| 固定 GORM 数据库回归 | 380 通过、0 失败、32 跳过 | 380 通过、0 失败、32 跳过 |
| GORM 核心与数据库合计 | 480 通过、0 失败、32 跳过 | 480 通过、0 失败、32 跳过 |

构建、无数据库单元测试、vet 及无数据库竞态检测也通过。统计依据 Go JSON 测试终态；跳过不计为通过，两种模式重复执行的核心测试也不能累加成额外的数据库场景。官方回归本轮没有启用 `-race`，自有集成与无数据库竞态入口启用了。

## 实例设置与复现条件

- 在两个实例内分别新建专用集成库和官方回归库，没有复用业务数据库。
- MySQL 实例为 `enable_ci=on`；Oracle 为 `enable_ci=off`。
- 自有集成测试保留 Oracle 默认 `ora_input_emptystr_isnull=on`。
- 官方 Oracle 回归按既有约定仅在测试连接上添加 `ora_input_emptystr_isnull=off`，匹配上游测试对空字符串的预期。
- 测试进程使用 `TZ=UTC`，DSN 使用 `TimeZone=UTC`；MySQL DSN 使用 `get_last_insert_id=no`。
- 测后核对两个实例默认时区仍为 Asia/Shanghai，Oracle 默认空字符串设置仍为 on，没有改变实例默认配置。
- 密码由本机进程环境提供，不包含在源码和日志中。

下载完整新版分支后，按[根目录 README](../../README.md)配置两条专用测试库 DSN，再执行：

```sh
bash scripts/test.sh
bash scripts/test.sh race
KINGBASE_TEST_ALLOW_DDL=yes bash scripts/test.sh integration -race
KINGBASE_TEST_ALLOW_DDL=yes bash scripts/test.sh official-mysql
KINGBASE_TEST_ALLOW_DDL=yes bash scripts/test.sh official-oracle
```

官方 Oracle 回归的 DSN 需要明确添加上述空字符串参数，详情见[测试说明](testing.md)。测试所需 Go 工具链、普通模块依赖、Python、patch 和数据库实例需由测试环境提供；仓库自带适配器、配套 gokb 和固定 GORM 回归源码归档。

## 跳过范围

自有集成在 Oracle 下跳过两项 MySQL 专用测试：`TestMySQLCustomizedTypesNoUnneededAlter`、`TestMySQLDefaultDateTimePrecision`。两种模式均实际连接，没有整组缺少 DSN 的跳过。

官方回归应用本仓库的方言接入和断言补丁，不属于未修改的上游原样测试。32 项跳过包括其他数据库专用场景及上游按方言名称选择的用例。新增的 `TestMigrateDontOverrideAlterColumnByDefaultValue` 使用 SQLite 专用类型，仍按上游条件跳过；同一迁移意图由新增 `TestGORM1312MigrateTypeWithMatchingDefault` 在两种金仓模式实测覆盖。

<details>
<summary>官方回归跳过名单（两种模式相同，32 项）</summary>

- `TestAssociationEmptyPrimaryKey`
- `TestAutoMigrateDecimal`
- `TestAutoMigrateInt8PGAndGaussDB`
- `TestConnPoolWrapper`
- `TestCreateFromMapWithoutPK`
- `TestGaussDB`
- `TestGaussDBAlterColumnDataType`
- `TestGaussDBMany2ManyWithDefaultValueUUID`
- `TestGaussDBOnConstraint`
- `TestGaussDBReturningIDWhichHasStringType`
- `TestGaussDBTableWithIdentifierLength`
- `TestInvalidCachedPlanSimpleProtocol`
- `TestInvalidCachedPlanSimpleProtocolGaussDB`
- `TestMany2ManyWithDefaultValueUUID`
- `TestManyToManyWithCustomizedForeignKeys`
- `TestManyToManyWithCustomizedForeignKeys2`
- `TestMigrateArrayTypeModel`
- `TestMigrateDontOverrideAlterColumnByDefaultValue`
- `TestMigrateExistingBoolColumnPGAndGaussDB`
- `TestMigrateMySQLWithCustomizedTypes`
- `TestMigrateSerialColumn`
- `TestMigrateWithIndexComment`
- `TestMigrateWithQuotedIndex`
- `TestPostgres`
- `TestPostgresOnConstraint`
- `TestPostgresReturningIDWhichHasStringType`
- `TestPostgresTableWithIdentifierLength`
- `TestPrimarykeyID`
- `TestPrimarykeyIDGaussDB`
- `TestTableType`
- `TestTiDBMigrateColumns`
- `TestWithSingleConnection`

</details>

本记录是固定版本和配置下的结果快照，其他 Go、SDK、数据库版本和实例参数组合需要分别验证。完整本机日志位于开发工作区 `test-results/gorm-v1.31.2-20261102202535/`，不随公开源码上传。后续代码更新后，应重新记录实际结果。
