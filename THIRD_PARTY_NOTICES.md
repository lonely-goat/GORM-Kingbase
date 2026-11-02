# 第三方来源与分发说明

## 适配器

`gorm-driver-kingbase/` 及根目录交付辅助脚本使用[适配器 MIT 许可](gorm-driver-kingbase/LICENSE)。这份许可不替代第三方组件自己的许可。

## 配套 gokb

`third_party/gokb/` 来自本项目原有配套 SDK 源码，模块名为 `kingbase.com/gokb`。复制时逐文件保持原始内容及版权声明；未替换为其他 gokb 实现，未赋予虚构的发行版本。文件清单与 SHA-256 见 [gokb-source.json](third_party/gokb-source.json)。

原源码包含中电科金仓（北京）科技股份有限公司的版权声明，取得的这份目录没有附带独立的 LICENSE。仓库所有者已于 2026-11-02 确认该配套 gokb 源码可以公开分发；确认状态记录在 [gokb-source.json](third_party/gokb-source.json) 中。原版权声明保持原样，本记录不替代或另行编写供应商许可文本。

gokb 原始 `go.mod` 仅声明模块名。其使用的 `civil`、`decimal` 等依赖由适配器 `go.mod` 固定；测试入口从适配器模块运行，不要求将 SDK 作为独立项目执行 `go test`。

## GORM 及常规模块

运行时 GORM 由适配器 `go.mod` 固定为 `gorm.io/gorm v1.31.1`，其源码和 MIT LICENSE 随 Go 模块取得。

`gorm-driver-kingbase/testdata/upstream/gorm-v1.31.1.tar.gz` 是固定版本的 GORM 原始源码归档，包含上游 LICENSE，供回归测试解压使用。来源地址及归档 SHA-256 见 [gorm-baseline.json](gorm-driver-kingbase/scripts/gorm-baseline.json)。测试补丁仅应用于临时副本。

其余模块按适配器 `go.mod`、`go.sum` 解析，相关许可保留在各模块源码中。本源码包没有包含这些模块的离线副本。
