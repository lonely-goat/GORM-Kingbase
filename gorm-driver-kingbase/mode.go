package kingbase

import (
	"fmt"
	"strings"
)

// Mode 表示 KingbaseES 实例启用的数据库兼容模式。
//
// ModeAuto 是推荐值：Dialector 初始化时执行 SHOW database_mode，并将检测结果
// 冻结在当前 Dialector 实例中。显式模式主要用于离线 SQL 生成、单元测试，或调用方
// 已经在连接建立前可靠确认实例模式的场景。
type Mode string

const (
	ModeAuto   Mode = "auto"
	ModeMySQL  Mode = "mysql"
	ModeOracle Mode = "oracle"
)

func (m Mode) validConfiguredMode() bool {
	switch normalizeMode(m) {
	case ModeAuto, ModeMySQL, ModeOracle:
		return true
	default:
		return false
	}
}

func normalizeMode(mode Mode) Mode {
	return Mode(strings.ToLower(strings.TrimSpace(string(mode))))
}

// parseDatabaseMode 只接受当前项目承诺支持的 MySQL、Oracle 两种模式。
// 不把 PG/SQLServer 等模式静默降级，是为了避免方言生成“看起来能运行、实际语义错误”的 SQL。
func parseDatabaseMode(raw string) (Mode, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	// KingbaseES compatibility (KB-MODE-005/006): SHOW database_mode is a
	// single mode value. Match it exactly after normalization so malformed
	// values such as "notmysql" cannot silently select the MySQL builder.
	switch normalized {
	case string(ModeMySQL):
		return ModeMySQL, nil
	case string(ModeOracle):
		return ModeOracle, nil
	default:
		return "", fmt.Errorf("kingbase: unsupported database_mode %q; only mysql and oracle are supported", raw)
	}
}
