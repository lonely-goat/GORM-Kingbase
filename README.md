# GORM V2 金仓双模式适配器

本项目通过 `kingbase.com/gokb` 连接 KingbaseES，支持 MySQL、Oracle 两种兼容模式。交付目录包含适配器、配套 gokb 源码、测试、示例和固定版本的 GORM 回归测试基线。

## 下载后运行

解压完整源码包，或克隆后进入仓库根目录。保留以下相对目录关系：

```text
GORM-Kingbase/
├── README.md
├── scripts/test.sh
├── gorm-driver-kingbase/       # 适配器、示例及测试
└── third_party/
    ├── gokb/                 # 配套驱动，保留原源码及 go.mod
    └── gokb-source.json      # 驱动逐文件 SHA-256
```

准备 Go 工具链；当前验证版本为 Linux amd64、Go 1.26.2，模块最低语言版本声明为 Go 1.22。下面的统一入口使用 Bash；Windows 可在 WSL 中运行，或进入适配器目录直接执行 Go 命令。GORM 等常规依赖由 Go 按 `go.mod`、`go.sum` 下载，需要可用的 Go 模块代理或已预装依赖缓存。本包不包含 Go 工具链和金仓数据库安装程序。

```sh
bash scripts/test.sh
```

默认依次构建、运行无需数据库的测试并执行 `go vet`；入口会清除本进程中的数据库连接变量，集成包显示 SKIP 属于预期，不代表实库验证通过。

也可手动执行：

```sh
cd gorm-driver-kingbase
GOWORK=off go build -buildvcs=false -mod=readonly ./...
GOWORK=off go test -buildvcs=false -mod=readonly -count=1 .
GOWORK=off go vet -buildvcs=false -mod=readonly ./...
```

竞态检测需要启用 CGO 及可用的 C 编译器：

```sh
bash scripts/test.sh race
```

## 两种模式的实库测试

准备两套可丢弃的专用测试数据库，分别启用 MySQL、Oracle 兼容模式。测试会创建、修改及删除测试表、索引、约束和 schema。连接信息在本机通过环境变量提供，不写进源码。

```sh
export KINGBASE_MYSQL_DSN='host=127.0.0.1 port=54321 user=TEST_USER password=TEST_PASSWORD dbname=TEST_DB sslmode=disable TimeZone=UTC get_last_insert_id=no'
export KINGBASE_ORACLE_DSN='host=127.0.0.1 port=54322 user=TEST_USER password=TEST_PASSWORD dbname=TEST_DB sslmode=disable TimeZone=UTC'
export KINGBASE_TEST_ALLOW_DDL=yes
bash scripts/test.sh integration
```

示例中的地址、端口、用户、密码及库名都需要替换。实库入口要求两条 DSN 和 DDL 开关均已配置，缺少配置直接失败；测试还会核对实例实际模式。Oracle 空字符串行为等实例设置见[测试说明](gorm-driver-kingbase/docs/testing.md)。

执行固定版本的 GORM 官方回归测试还需要 Python 3.9+、`patch`，以及相应依赖的下载能力：

```sh
bash scripts/test.sh official-mysql
bash scripts/test.sh official-oracle
```

回归脚本使用随包保留的 GORM v1.31.1 源码归档，在临时目录应用方言接入及断言补丁后测试；不依赖开发机上的 GORM 研究仓库。实际执行的 PASS、FAIL、SKIP 以本次输出为准。

## 业务接入和交付范围

使用示例和能力边界见[适配器 README](gorm-driver-kingbase/README.md)，源码对照见[七项功能说明](gorm-driver-kingbase/docs/GORM-v2-KingbaseES代码修改说明-按七项功能.md)。本项目依赖 `gorm.io/gorm v1.31.1`，不要求修改 GORM 核心。

当前交付方式是下载整个源码树运行测试。适配器保留 `kingbase.com/gorm-driver-kingbase` 模块名；将其作为另一个业务模块的依赖时，需要在业务模块中配置适配器和 gokb 的本地替换，详见[开发说明](gorm-driver-kingbase/docs/development.md)。这份源码包尚未配置为可直接公开 `go get` 的模块发布物。

适配器许可见 [LICENSE](gorm-driver-kingbase/LICENSE)，第三方来源和 gokb 分发说明见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。配套 gokb 源码保留原版权声明；仓库所有者已于 2026-11-02 确认可以公开分发。
