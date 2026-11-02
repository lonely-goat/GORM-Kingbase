module kingbase.com/gorm-driver-kingbase

go 1.22

require (
	gorm.io/gorm v1.31.1
	kingbase.com/gokb v0.0.0
)

require (
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	golang.org/x/text v0.20.0 // indirect
)

// 配套 gokb 源码随交付仓库保留，下载后无需依赖开发机外部目录。
// 业务项目引用本模块时，仍需自行配置 gokb 的 replace。
replace kingbase.com/gokb => ../third_party/gokb
