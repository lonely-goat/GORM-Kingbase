# 配套 SDK

`gokb/` 是适配器当前验证配套的完整 Go 驱动源码，模块名为 `kingbase.com/gokb`。适配器通过 `../third_party/gokb` 引用它，移动或解压整个仓库不需要手动配置 SDK 路径。

[gokb-source.json](gokb-source.json) 记录复制时每个原始文件的 SHA-256。后续更新 SDK 时，应重新建立来源记录并执行构建、单元测试和双模式实库回归。

本目录未改写供应商源码或 `go.mod`。测试命令从 `gorm-driver-kingbase/` 执行，使用适配器固定的第三方依赖版本。

版权及公开分发许可状态见 [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md)。
