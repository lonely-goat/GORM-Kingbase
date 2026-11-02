package kingbase

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/migrator"
)

func (m Migrator) ColumnTypes(value interface{}) ([]gorm.ColumnType, error) {
	columnTypes := make([]gorm.ColumnType, 0)
	err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		schemaName, tableName := m.CurrentSchema(stmt, stmt.Table)
		metadataSQL := mysqlColumnMetadataSQL
		if m.dialector.DatabaseMode() == ModeOracle {
			metadataSQL = oracleColumnMetadataSQL
		}
		rows, err := m.queryRaw(metadataSQL, schemaName, tableName).Rows()
		if err != nil {
			return err
		}
		defer rows.Close()

		byName := make(map[string]*migrator.ColumnType)
		for rows.Next() {
			column := &migrator.ColumnType{
				PrimaryKeyValue: sql.NullBool{Bool: false, Valid: true},
				UniqueValue:     sql.NullBool{Bool: false, Valid: true},
			}
			var (
				dataType, nullable, autoMarker string
				length, precision, scale       sql.NullInt64
				datetimePrecision              sql.NullInt64
				defaultValue, comment          sql.NullString
			)
			if err := rows.Scan(
				&column.NameValue, &dataType, &length, &precision, &scale,
				&datetimePrecision, &defaultValue, &nullable, &autoMarker, &comment,
			); err != nil {
				return err
			}

			column.DataTypeValue = sql.NullString{String: strings.ToLower(dataType), Valid: dataType != ""}
			column.ColumnTypeValue = sql.NullString{String: buildColumnTypeName(dataType, length, precision, scale, datetimePrecision), Valid: dataType != ""}
			column.LengthValue = length
			column.DecimalSizeValue = precision
			column.ScaleValue = scale
			if datetimePrecision.Valid {
				column.DecimalSizeValue = datetimePrecision
			}
			column.NullableValue = sql.NullBool{Bool: strings.EqualFold(nullable, "YES"), Valid: nullable != ""}
			column.CommentValue = comment

			isIdentity := strings.Contains(strings.ToLower(autoMarker), "auto_increment") ||
				strings.EqualFold(autoMarker, "YES") || strings.Contains(strings.ToLower(defaultValue.String), "nextval(")
			column.AutoIncrementValue = sql.NullBool{Bool: isIdentity, Valid: true}
			if isIdentity {
				// identity 的内部默认表达式不应与模型 default 标签比较，否则每次都会误迁移。
				column.DefaultValueValue = sql.NullString{}
			} else {
				column.DefaultValueValue = normalizeDefaultValue(defaultValue)
			}
			completeColumnTypeMetadata(column, dataType)

			byName[column.Name()] = column
			columnTypes = append(columnTypes, column)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(columnTypes) == 0 {
			// A catalog query for a missing table returns zero rows without an
			// error. GORM callers must be able to distinguish that case from a
			// real table with no columns.
			var tableCount int64
			if err := m.queryRaw(
				"SELECT count(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?",
				schemaName, tableName,
			).Scan(&tableCount).Error; err != nil {
				return err
			}
			if tableCount == 0 {
				return fmt.Errorf("kingbase: table %q does not exist", tableName)
			}
		}

		constraintRows, err := m.queryRaw(columnConstraintMetadataSQL, schemaName, tableName).Rows()
		if err != nil {
			return err
		}
		defer constraintRows.Close()
		for constraintRows.Next() {
			var columnName, constraintType string
			var columnCount int64
			if err := constraintRows.Scan(&columnName, &constraintType, &columnCount); err != nil {
				return err
			}
			column := byName[columnName]
			if column == nil {
				continue
			}
			switch constraintType {
			case "PRIMARY KEY":
				column.PrimaryKeyValue = sql.NullBool{Bool: true, Valid: true}
			case "UNIQUE":
				// 复合唯一约束不能等价为单列 unique 标签，只标记真正的单列约束。
				if columnCount == 1 {
					column.UniqueValue = sql.NullBool{Bool: true, Valid: true}
				}
			}
		}
		if err := constraintRows.Err(); err != nil {
			return err
		}
		// AutoMigrate matches ColumnType.Name against the model's exact
		// DBName. Return logical names only for model-declared columns; raw
		// table metadata keeps its physical names. Constraint lookup above
		// deliberately used physical names throughout.
		if stmt.Schema != nil {
			for _, field := range stmt.Schema.Fields {
				if column := byName[m.dialector.catalogName(field.DBName)]; column != nil {
					column.NameValue.String = field.DBName
				}
			}
		}
		return nil
	})
	return columnTypes, err
}

// completeColumnTypeMetadata completes catalog-derived column metadata.
// It originally avoided GORM v1.31.1's nil *sql.ColumnType fallback. GORM
// v1.31.2 guards that pointer, but a nil-safe zero value does not supply the
// length/precision/scan-type contract expected by this adapter. Keep this
// normalization for types whose catalog size is NULL, such as bigint.
//
// A valid zero means "this type has no applicable length/precision". It is safer
// than inventing a database size and preserves AutoMigrate's comparison semantics.
func completeColumnTypeMetadata(column *migrator.ColumnType, dataType string) {
	if !column.LengthValue.Valid {
		column.LengthValue = sql.NullInt64{Int64: 0, Valid: true}
	}
	if !column.DecimalSizeValue.Valid {
		column.DecimalSizeValue = sql.NullInt64{Int64: 0, Valid: true}
	}
	if !column.ScaleValue.Valid {
		column.ScaleValue = sql.NullInt64{Int64: 0, Valid: true}
	}
	if column.ScanTypeValue == nil {
		column.ScanTypeValue = scanTypeForDatabaseType(dataType)
	}
}

func scanTypeForDatabaseType(dataType string) reflect.Type {
	normalized := strings.ToLower(dataType)
	switch {
	case strings.Contains(normalized, "bool"):
		return reflect.TypeOf(false)
	case strings.Contains(normalized, "int"):
		return reflect.TypeOf(int64(0))
	case strings.Contains(normalized, "decimal"), strings.Contains(normalized, "numeric"),
		strings.Contains(normalized, "number"), strings.Contains(normalized, "float"),
		strings.Contains(normalized, "double"), strings.Contains(normalized, "real"):
		return reflect.TypeOf(float64(0))
	case strings.Contains(normalized, "time"), normalized == "date":
		return reflect.TypeOf(time.Time{})
	case strings.Contains(normalized, "blob"), strings.Contains(normalized, "binary"), normalized == "bytea":
		return reflect.TypeOf([]byte(nil))
	default:
		return reflect.TypeOf("")
	}
}

func buildColumnTypeName(dataType string, length, precision, scale, datetimePrecision sql.NullInt64) string {
	lower := strings.ToLower(dataType)
	switch {
	case length.Valid && length.Int64 > 0 && (strings.Contains(lower, "char") || strings.Contains(lower, "binary")):
		return fmt.Sprintf("%s(%d)", lower, length.Int64)
	case precision.Valid && (lower == "decimal" || lower == "numeric" || lower == "number"):
		if scale.Valid {
			return fmt.Sprintf("%s(%d,%d)", lower, precision.Int64, scale.Int64)
		}
		return fmt.Sprintf("%s(%d)", lower, precision.Int64)
	case datetimePrecision.Valid && (strings.Contains(lower, "timestamp") || strings.Contains(lower, "datetime")):
		return fmt.Sprintf("%s(%d)", lower, datetimePrecision.Int64)
	default:
		return lower
	}
}

func normalizeDefaultValue(value sql.NullString) sql.NullString {
	if !value.Valid {
		return value
	}
	normalized := strings.TrimSpace(value.String)
	// KingbaseES compatibility (KB-META-011): the catalog often stores a
	// literal as 'x'::character varying. Only remove a top-level server cast;
	// a value such as 'scheme::value' must retain the :: inside its quotes.
	if cast := topLevelTypeCastIndex(normalized); cast >= 0 {
		normalized = normalized[:cast]
	}
	if len(normalized) >= 2 && normalized[0] == '\'' && normalized[len(normalized)-1] == '\'' {
		normalized = strings.ReplaceAll(normalized[1:len(normalized)-1], "''", "'")
	}
	return sql.NullString{String: normalized, Valid: true}
}

func topLevelTypeCastIndex(value string) int {
	var quote byte
	depth := 0
	for index := 0; index+1 < len(value); index++ {
		current := value[index]
		if quote != 0 {
			if current == quote {
				if index+1 < len(value) && value[index+1] == quote {
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		switch current {
		case '\'', '"':
			quote = current
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 && value[index+1] == ':' {
				return index
			}
		}
	}
	return -1
}

const columnConstraintMetadataSQL = `
SELECT kcu.column_name, tc.constraint_type,
       count(*) OVER (PARTITION BY tc.constraint_schema, tc.table_name, tc.constraint_name)
FROM information_schema.table_constraints AS tc
JOIN information_schema.key_column_usage AS kcu
  ON kcu.constraint_catalog = tc.constraint_catalog
 AND kcu.constraint_schema = tc.constraint_schema
 AND kcu.constraint_name = tc.constraint_name
 AND kcu.table_name = tc.table_name
WHERE tc.constraint_schema = ? AND tc.table_name = ?
  AND tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE')
ORDER BY tc.constraint_name, kcu.ordinal_position`
