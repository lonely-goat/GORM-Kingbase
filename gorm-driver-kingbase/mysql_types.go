package kingbase

import (
	"fmt"
	"math"
	"strings"

	"gorm.io/gorm/schema"
)

func (d *Dialector) mysqlDataTypeOf(field *schema.Field) string {
	switch field.DataType {
	case schema.Bool:
		return "boolean"
	case schema.Int, schema.Uint:
		return mysqlIntegerType(field)
	case schema.Float:
		if field.Precision > 0 {
			return fmt.Sprintf("decimal(%d,%d)", field.Precision, field.Scale)
		}
		if field.Size <= 32 {
			return "float"
		}
		return "double"
	case schema.String:
		return d.mysqlStringType(field)
	case schema.Time:
		if field.Precision > 0 {
			return fmt.Sprintf("datetime(%d)", field.Precision)
		}
		if field.TagSettings["PRECISION"] != "" {
			return "datetime"
		}
		// Match GORM's MySQL default so UpdatedAt does not round to a whole
		// second when it is saved and read back.
		return "datetime(3)"
	case schema.Bytes:
		switch {
		case field.Size > 0 && field.Size < 65536:
			return fmt.Sprintf("varbinary(%d)", field.Size)
		case field.Size >= 65536 && field.Size <= 1<<24:
			return "mediumblob"
		default:
			return "longblob"
		}
	default:
		return mysqlCustomType(field)
	}
}

func mysqlIntegerType(field *schema.Field) string {
	var dataType string
	switch {
	case field.Size <= 8:
		dataType = "tinyint"
	case field.Size <= 16:
		dataType = "smallint"
	case field.Size <= 24:
		dataType = "mediumint"
	case field.Size <= 32:
		dataType = "integer"
	default:
		dataType = "bigint"
	}

	// KingbaseES MySQL 模式已实测拒绝 bigint unsigned AUTO_INCREMENT。
	// 因此 uint 与 int 使用相同的有符号物理类型，优先保证 gorm.Model.ID 等常用
	// 自动主键可迁移、插入和回填。超过对应有符号上限的 uint 值不在兼容范围内。
	if field.AutoIncrement {
		dataType += " AUTO_INCREMENT"
	}
	return dataType
}

func (d *Dialector) mysqlStringType(field *schema.Field) string {
	size := field.Size
	if size == 0 {
		hasIndex := field.TagSettings["INDEX"] != "" || field.TagSettings["UNIQUE"] != ""
		if field.PrimaryKey || field.HasDefaultValue || hasIndex {
			// MySQL 索引、主键和默认值不适合落到 text；沿用 GORM MySQL 的保守长度。
			size = 191
		}
	}

	switch {
	case size >= 65536 && size <= int(math.Pow(2, 24)):
		return "mediumtext"
	case size > int(math.Pow(2, 24)) || size <= 0:
		return "longtext"
	default:
		return fmt.Sprintf("varchar(%d)", size)
	}
}

func mysqlCustomType(field *schema.Field) string {
	dataType := string(field.DataType)
	if dataType == "" {
		dataType = string(field.GORMDataType)
	}
	if field.AutoIncrement && !strings.Contains(strings.ToLower(dataType), "auto_increment") {
		dataType += " AUTO_INCREMENT"
	}
	return dataType
}
