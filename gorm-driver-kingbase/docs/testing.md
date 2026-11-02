# 测试说明

当前 GORM v1.31.2 的验证结果见[新版双模式验证记录](实库验证-GORM-v1.31.2-2026-11-02.md)，旧版结果保留在[历史 v1.31.1 记录](实库验证-2026-11-02.md)，两者的测试计数不能混用。

完整仓库的统一入口位于[根目录说明](../../README.md)。在仓库根目录运行 `bash scripts/test.sh` 执行构建、无数据库测试和 vet；`bash scripts/test.sh race` 执行无数据库竞态测试。两个入口会清除自身进程中的 DSN，避免意外连接实库。下面的直接 Go 命令均在 `gorm-driver-kingbase/` 中执行，并会使用调用进程原有环境变量。

## 无数据库验证

在模块根目录执行：

```sh
go test ./...
go test -race ./...
go vet ./...
```

根目录两个测试文件通过模拟驱动、连接池及 DryRun 验证模式、参数、类型、DDL、元信息和错误转换。`integration/` 是真实库测试，未配置 DSN 时会显示 SKIP；SKIP 不代表数据库场景通过。

## 双模式集成测试

集成测试会创建、修改和删除固定前缀的表、索引、约束及专用 schema。使用可丢弃的专用测试数据库，避免连接业务库。

```sh
export KINGBASE_MYSQL_DSN='host=... get_last_insert_id=no'
export KINGBASE_ORACLE_DSN='host=...'
KINGBASE_TEST_REQUIRE_DSN=yes go test ./integration -count=1 -p=1 -v
```

或从仓库根目录运行 `KINGBASE_TEST_ALLOW_DDL=yes bash scripts/test.sh integration`。统一实库入口额外要求 DDL 开关，并强制两条 DSN 都存在；缺少任一配置直接报错。连接凭据由测试者在本机配置，仓库不包含有效连接配置。

`KINGBASE_TEST_REQUIRE_DSN=yes` 要求两条 DSN 均存在，防止缺少环境时把实库测试跳过。Oracle 空字符串、连接时区等配置应按本轮验收目标明确设置，不将一组配置下的结果推广到其他配置。

新增的 `integration/gorm_upgrade_test.go` 覆盖 v1.31.2 的泛型模型推导、WithResult 错误与复用、命名数组/切片参数，以及相同默认值下的类型迁移。上游新增的批量 RETURNING 测试也已纳入 Kingbase 回归。

七项需求示例位于 `docs/examples/`，各目录均有一个独立 main.go。先阅读对应程序的参数及 fixture 逻辑，再按其命令行运行；连接会话示例只读，其他示例可能创建对象或执行回滚验证。

## 与依赖版本一致的 GORM 回归

```sh
KINGBASE_TEST_ALLOW_DDL=yes ./scripts/run-official-gorm-tests.sh mysql -count=1
KINGBASE_TEST_ALLOW_DDL=yes ./scripts/run-official-gorm-tests.sh oracle -count=1
```

运行器依次完成：

1. 检查 DDL 授权开关和相应 DSN，通过 autodetect 核对实际模式。
2. 从随仓库保留的 v1.31.2 源码归档提取临时副本，检查 SHA-256 及 go.mod 版本。
3. 精确应用针对 v1.31.2 重新生成的方言接入与断言补丁（禁用模糊匹配），保留生产核心基线。
4. 在临时副本执行核心及数据库测试，按 Go JSON 事件统计 PASS、FAIL、SKIP。零测试匹配会失败。

历史测试中使用的进程 `TZ=UTC`、DSN `TimeZone=UTC`，以及 Oracle 的 `ora_input_emptystr_isnull=off` 会影响相应用例语义。复现这些结果时应采用对应配置，并在报告中记录。`KINGBASE_TEST_SUMMARY` 可指定 JSON 统计输出文件。

`official-gorm-tests.patch` 负责接入本方言及适用语法；`official-gorm-assertions.patch` 修补已审查的断言漏洞并启用适用场景。它们修改临时测试副本，不能称为未打补丁的上游原样测试。`KINGBASE_DIAGNOSTIC_SQL=yes` 会额外应用 SQL 诊断 patch，仅用于排查，不作为正式通过计数。

源码归档与补丁均需保留：它们使新机器可以使用相同基线，不必克隆开发机旁边的 GORM 研究仓库。

## 故障注入

配置两条专用 DSN 后，可运行：

```sh
TZ=UTC KINGBASE_TEST_ALLOW_DDL=yes python3 scripts/verify-expectations.py ../test-results/expectation-controls
TZ=UTC KINGBASE_TEST_ALLOW_DDL=yes python3 scripts/verify-assertion-regressions.py ../test-results/assertion-controls
```

两个脚本通过 Go overlay 注入错误，要求正常基线通过、指定故障由对应断言拒绝；编译错误或连接故障不算有效拒绝。前者针对适配行为及基础断言，后者针对补强的官方及自有测试断言。当前脚本只使用固定版本基线，不依赖历史研究项目。

本地历史日志和截图属于验收证据，保存在开发工作区的 test-results；不随源码仓库上传。测试总数随基线和用例变化，以本次命令输出为准，不复用旧研究快照的 511 项等固定计数。
