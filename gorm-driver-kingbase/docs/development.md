# 开发与交付说明

初次接手或需要定位源码时，先读[维护导读](维护导读.md)。本文件中的命令以当前完整 Git 工作目录为入口；新生成的交付目录只是源码副本，不会自动替代原开发目录。

## 模块和依赖

交付仓库根目录包含 `gorm-driver-kingbase/` 和 `third_party/gokb/`。适配器与 gokb 各自保留 `go.mod`，原 GORM 核心研究仓库及 MySQL、Oracle 对照项目不随交付包上传。

默认目录关系是：

```text
GORM-Kingbase/
├── README.md
├── scripts/test.sh
├── gorm-driver-kingbase/
│   └── go.mod
└── third_party/
    ├── gokb-source.json
    └── gokb/
        └── go.mod
```

适配器声明 `kingbase.com/gokb v0.0.0` 并设置 `replace kingbase.com/gokb => ../third_party/gokb`。`v0.0.0` 是本地替换使用的占位版本，不是 SDK 正式发行版本。SDK 来源和摘要见 [gokb-source.json](../../third_party/gokb-source.json)，分发说明见 [THIRD_PARTY_NOTICES.md](../../THIRD_PARTY_NOTICES.md)。

在仓库根目录执行 `bash scripts/test.sh`，或在适配器目录执行 `GOWORK=off go build -buildvcs=false -mod=readonly ./...`。统一入口禁用 VCS 信息写入，解压源码时不依赖 Git 元数据。GORM 及其余模块仍按固定版本从 Go 模块代理下载；完全离线环境需要另外准备工具链和依赖缓存。

## 在业务模块中引用

Go 不会传递依赖模块自己的 `replace`。业务项目与完整交付目录并列放置时，在业务项目的 `go.mod` 中设置：

```go
require kingbase.com/gorm-driver-kingbase v0.0.0

replace kingbase.com/gorm-driver-kingbase => ../GORM-Kingbase/gorm-driver-kingbase
replace kingbase.com/gokb => ../GORM-Kingbase/third_party/gokb
```

路径按业务项目实际位置调整，再运行 `go mod tidy`。适配器和 SDK 的 Go 导入路径保持原模块名。本方式支持取得源码后的本地接入，不代表已经配置公开 `go get` 发布；公开安装仍需确定最终 module path 和可获取的 gokb 版本。

## 生成干净交付包

在仓库根目录运行：

```sh
python3 gorm-driver-kingbase/scripts/build-review-bundle.py deliverables/GORM-Kingbase-delivery --race
```

目标目录、同名 `.tar.gz`、`.zip` 和 `.validation.log` 必须不存在。脚本只复制明确列出的交付文件：根目录说明和测试入口、适配器、配套 gokb 及来源记录；排除研究仓库、归档、历史测试日志、环境配置、IDE 文件和缓存。

打包前核对 gokb 文件摘要及 Markdown 本地链接；复制后重新检查依赖路径，再在禁用模块网络下载的环境中构建、执行无数据库测试和 `go vet`。`--race` 额外执行竞态测试，需要 CGO 和 C 编译器。其他固定模块需预先下载到 Go 缓存。

交付目录中的 `manifest.json` 记录源码摘要、基线版本和本次验证范围；独立的 `.validation.log` 保存在包外。`.tar.gz` 和 `.zip` 包含同一份源码树。

可选 `--test-databases` 会在两条 DSN 和 `KINGBASE_TEST_ALLOW_DDL=yes` 均配置时执行副本的双模式集成测试。默认打包不连接数据库，不将历史实库结果记成本次结果。固定版本 GORM 官方实库回归使用根目录 `scripts/test.sh official-mysql`、`official-oracle` 单独执行。

## GitHub 交付边界

后续上传时，以生成的干净交付目录作为交付分支的仓库根目录；两个源码子目录均保留。原 GORM 分支可以留作研究对照。默认分支应指向交付代码，甲方打开仓库即可看到根目录运行说明。

仓库所有者已于 2026-11-02 确认配套 gokb 可以公开分发，发布时保留原版权声明及来源记录。工作区 `archive`、`test-results`、旧 GORM 研究副本及历史交付材料不属于这份源码树。

## gokb-returning

配套 gokb 的 `conn.go` 中 `addReturning` 在相关选项生效且 SQL 是 INSERT 时追加 `RETURNING *`。方言正常已生成 RETURNING，因此初始化检查可读取的 MySQL DSN 中的冲突选项。此说明对应随包源码，不表示其他 gokb 版本的实现均相同。
