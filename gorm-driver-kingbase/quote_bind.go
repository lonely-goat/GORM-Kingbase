package kingbase

import (
	"regexp"
	"strconv"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var oraclePlaceholder = regexp.MustCompile(`:(\d+)`)

// BindVarTo 按模式生成 gokb 已验证支持的占位符。
// MySQL 模式使用 ?；Oracle 模式使用从 1 开始的 :N。
func (d *Dialector) BindVarTo(writer clause.Writer, stmt *gorm.Statement, _ interface{}) {
	if d.DatabaseMode() == ModeOracle {
		_ = writer.WriteByte(':')
		_, _ = writer.WriteString(strconv.Itoa(len(stmt.Vars)))
		return
	}
	_ = writer.WriteByte('?')
}

func (d *Dialector) QuoteTo(writer clause.Writer, value string) {
	if d.DatabaseMode() == ModeOracle {
		quoteIdentifier(writer, value, '"')
		return
	}
	quoteIdentifier(writer, value, '`')
}

// quoteIdentifier 保留 GORM 官方 MySQL/PostgreSQL Dialector 的三个重要语义：
// 1. schema.table 的每一段分别引用；2. 已引用输入不重复包裹；3. 内部引号双写转义。
// 这比 strings.Split 后简单拼接更安全，因为调用方可能传入已引用标识符。
func quoteIdentifier(writer clause.Writer, value string, delimiter byte) {
	var (
		underQuoted      bool
		selfQuoted       bool
		continuousQuotes int8
		shiftDelimiter   int8
	)

	for _, current := range []byte(value) {
		switch current {
		case delimiter:
			continuousQuotes++
			if continuousQuotes == 2 {
				_, _ = writer.WriteString(string([]byte{delimiter, delimiter}))
				continuousQuotes = 0
			}
		case '.':
			if continuousQuotes > 0 || !selfQuoted {
				shiftDelimiter = 0
				underQuoted = false
				continuousQuotes = 0
				_ = writer.WriteByte(delimiter)
			}
			_ = writer.WriteByte(current)
			continue
		default:
			if shiftDelimiter-continuousQuotes <= 0 && !underQuoted {
				_ = writer.WriteByte(delimiter)
				underQuoted = true
				if selfQuoted = continuousQuotes > 0; selfQuoted {
					continuousQuotes--
				}
			}

			for ; continuousQuotes > 0; continuousQuotes-- {
				_, _ = writer.WriteString(string([]byte{delimiter, delimiter}))
			}
			_ = writer.WriteByte(current)
		}
		shiftDelimiter++
	}

	if continuousQuotes > 0 && !selfQuoted {
		_, _ = writer.WriteString(string([]byte{delimiter, delimiter}))
	}
	_ = writer.WriteByte(delimiter)
}

func (d *Dialector) Explain(sqlText string, vars ...interface{}) string {
	if d.DatabaseMode() == ModeOracle {
		return logger.ExplainSQL(sqlText, oraclePlaceholder, "'", vars...)
	}
	return logger.ExplainSQL(sqlText, nil, "'", vars...)
}
