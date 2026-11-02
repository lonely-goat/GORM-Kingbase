package kingbase

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

// Migrator 复用 GORM 的迁移编排，只替换 KingbaseES 有差异的元信息查询和 DDL。
// 这样 AutoMigrate 的模型重排、约束/索引创建等通用逻辑仍由 GORM 核心维护。
type Migrator struct {
	migrator.Migrator
	dialector *Dialector
}

// queryRaw 保证 DryRun 下的元信息查询仍会真实执行；这是 GORM AutoMigrate
// “查询现状 + 打印将执行 DDL”语义所必需的。
func (m Migrator) queryRaw(sqlText string, values ...interface{}) *gorm.DB {
	queryDB := m.DB
	if m.DB.DryRun {
		queryDB = m.DB.Session(&gorm.Session{})
		queryDB.DryRun = false
	}
	return queryDB.Raw(sqlText, values...)
}

// CurrentDatabase returns the connected database name. Object discovery uses
// CurrentSchema independently because a KingbaseES database contains schemas.
func (m Migrator) CurrentDatabase() (name string) {
	_ = m.queryRaw("SELECT CURRENT_DATABASE()").Scan(&name).Error
	return name
}

// CurrentSchema 同时支持模型表名 "schema.table" 和 db.Table("schema.table")。
// 未显式指定 schema 时把 CURRENT_SCHEMA() 作为 SQL 表达式嵌入元信息查询，
// 从而不依赖用户名、dbname 或固定 public 假设。
func (m Migrator) CurrentSchema(stmt *gorm.Statement, table string) (interface{}, interface{}) {
	if schemaName, tableName, ok := splitQualifiedName(table); ok {
		return m.dialector.catalogName(schemaName), m.dialector.catalogName(tableName)
	}
	if stmt.TableExpr != nil {
		if schemaName, _, ok := splitQualifiedName(stmt.TableExpr.SQL); ok {
			return m.dialector.catalogName(schemaName), m.dialector.catalogName(table)
		}
	}
	return clause.Expr{SQL: "CURRENT_SCHEMA()"}, m.dialector.catalogName(table)
}

func splitQualifiedName(name string) (string, string, bool) {
	// KingbaseES compatibility (KB-META-002): locate the schema separator only
	// outside quoted identifiers. A simple strings.Split would misread names
	// such as "tenant.name"."events" and break schema-scoped metadata queries.
	name = strings.TrimSpace(name)
	var quote byte
	separator := -1
	for index := 0; index < len(name); index++ {
		current := name[index]
		if quote != 0 {
			if current == quote {
				// SQL escapes an identifier quote by doubling it. Skip the
				// second byte so an embedded quote does not terminate the part.
				if index+1 < len(name) && name[index+1] == quote {
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		switch current {
		case '`', '"':
			quote = current
		case '.':
			if separator >= 0 {
				return "", "", false
			}
			separator = index
		}
	}
	if quote != 0 || separator <= 0 || separator >= len(name)-1 {
		return "", "", false
	}
	schemaName, schemaOK := unquoteIdentifier(name[:separator])
	tableName, tableOK := unquoteIdentifier(name[separator+1:])
	if !schemaOK || !tableOK || schemaName == "" || tableName == "" {
		return "", "", false
	}
	return schemaName, tableName, true
}

func unquoteIdentifier(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if value[0] != '`' && value[0] != '"' {
		if strings.ContainsAny(value, "`\"") {
			return "", false
		}
		return value, true
	}
	quote := value[0]
	if len(value) < 2 || value[len(value)-1] != quote {
		return "", false
	}
	inner := value[1 : len(value)-1]
	return strings.ReplaceAll(inner, string([]byte{quote, quote}), string(quote)), true
}

func (m Migrator) GetTables() (tables []string, err error) {
	schemaName, _ := m.CurrentSchema(m.DB.Statement, "")
	err = m.queryRaw(
		"SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_type = ? ORDER BY table_name",
		schemaName, "BASE TABLE",
	).Scan(&tables).Error
	return tables, err
}

func (m Migrator) HasTable(value interface{}) bool {
	var count int64
	_ = m.RunWithValue(value, func(stmt *gorm.Statement) error {
		schemaName, tableName := m.CurrentSchema(stmt, stmt.Table)
		return m.queryRaw(
			"SELECT count(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ? AND table_type = ?",
			schemaName, tableName, "BASE TABLE",
		).Scan(&count).Error
	})
	return count > 0
}

func (m Migrator) HasColumn(value interface{}, fieldName string) bool {
	var count int64
	_ = m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema != nil {
			if field := stmt.Schema.LookUpField(fieldName); field != nil {
				fieldName = field.DBName
			}
		}
		schemaName, tableName := m.CurrentSchema(stmt, stmt.Table)
		return m.queryRaw(
			"SELECT count(*) FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = ?",
			schemaName, tableName, m.dialector.catalogName(fieldName),
		).Scan(&count).Error
	})
	return count > 0
}

func (m Migrator) HasConstraint(value interface{}, name string) bool {
	var count int64
	_ = m.RunWithValue(value, func(stmt *gorm.Statement) error {
		constraint, table := m.GuessConstraintInterfaceAndTable(stmt, name)
		if constraint != nil {
			name = constraint.GetName()
		}
		schemaName, tableName := m.CurrentSchema(stmt, table)
		return m.queryRaw(
			"SELECT count(*) FROM information_schema.table_constraints WHERE constraint_schema = ? AND table_name = ? AND constraint_name = ?",
			schemaName, tableName, m.dialector.catalogName(name),
		).Scan(&count).Error
	})
	return count > 0
}

// MigrateColumnUnique distinguishes a gorm:"unique" constraint from a
// gorm:"uniqueIndex" on the same column. KingbaseES reports both as UNIQUE in
// information_schema, so ColumnType.Unique alone cannot decide whether the
// GORM-named constraint should be created or dropped.
func (m Migrator) MigrateColumnUnique(value interface{}, field *schema.Field, _ gorm.ColumnType) error {
	if field.PrimaryKey {
		return nil
	}
	queryTx, execTx := m.GetQueryAndExecTx()
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		name := m.DB.NamingStrategy.UniqueName(stmt.Table, field.DBName)
		hasConstraint := queryTx.Migrator().HasConstraint(value, name)
		switch {
		case field.Unique && !hasConstraint:
			return execTx.Migrator().CreateConstraint(value, name)
		case !field.Unique && hasConstraint:
			return execTx.Migrator().DropConstraint(value, name)
		default:
			return nil
		}
	})
}

const kingbaseHasIndexSQL = `
SELECT count(*)
FROM pg_catalog.pg_indexes
WHERE schemaname = ? AND tablename = ? AND indexname = ?`

func (m Migrator) HasIndex(value interface{}, name string) bool {
	var count int64
	_ = m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema != nil {
			if index := stmt.Schema.LookIndex(name); index != nil {
				name = index.Name
			}
		}
		schemaName, tableName := m.CurrentSchema(stmt, stmt.Table)
		name = m.dialector.catalogName(name)
		return m.queryRaw(kingbaseHasIndexSQL, schemaName, tableName, name).Scan(&count).Error
	})
	return count > 0
}

// CreateTable 使用通用建表逻辑（其中会调用本方言 DataTypeOf），随后使用
// COMMENT ON COLUMN 写入注释。注释不拼接用户文本，Explain 会先完成安全字面量转义。
func (m Migrator) CreateTable(values ...interface{}) error {
	for _, value := range m.ReorderModels(values, false) {
		if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
			if stmt.Schema == nil || m.DB.DisableForeignKeyConstraintWhenMigrating || m.DB.IgnoreRelationshipsWhenMigrating {
				return nil
			}
			for _, rel := range stmt.Schema.Relationships.Relations {
				if rel.Field.IgnoreMigration {
					continue
				}
				if constraint := rel.ParseConstraint(); constraint != nil && constraint.Schema == stmt.Schema {
					if err := m.ensureReferenceUniqueConstraint(stmt, constraint); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if err := m.Migrator.CreateTable(value); err != nil {
			return err
		}
		if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
			if stmt.Schema == nil {
				return nil
			}
			for _, dbName := range stmt.Schema.DBNames {
				field := stmt.Schema.FieldsByDBName[dbName]
				if field.Comment != "" && !field.IgnoreMigration {
					if err := m.commentColumn(stmt, field); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// KingbaseES requires a UNIQUE constraint for a referenced non-primary key.
// A standalone GORM uniqueIndex is not sufficient on either target mode. Only
// attach an existing, plain unique index whose columns exactly match the FK
// reference; never invent uniqueness for a non-unique model field.
func (m Migrator) ensureReferenceUniqueConstraint(stmt *gorm.Statement, constraint *schema.Constraint) error {
	if constraint == nil || len(constraint.References) == 0 {
		return nil
	}
	for _, index := range constraint.ReferenceSchema.ParseIndexes() {
		if index.Class != "UNIQUE" || index.Where != "" || len(index.Fields) != len(constraint.References) {
			continue
		}
		matches := true
		for position, option := range index.Fields {
			if option.Expression != "" || option.Length != 0 || option.Field.DBName != constraint.References[position].DBName {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		schemaName, tableName := m.CurrentSchema(stmt, constraint.ReferenceSchema.Table)
		var count int64
		if err := m.queryRaw(
			"SELECT count(*) FROM information_schema.table_constraints WHERE constraint_schema = ? AND table_name = ? AND constraint_name = ? AND constraint_type = 'UNIQUE'",
			schemaName, tableName, m.dialector.catalogName(index.Name),
		).Scan(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			if err := m.DB.Exec(
				"ALTER TABLE ? ADD CONSTRAINT ? UNIQUE USING INDEX ?",
				clause.Table{Name: constraint.ReferenceSchema.Table}, clause.Column{Name: index.Name}, clause.Column{Name: index.Name},
			).Error; err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func (m Migrator) CreateConstraint(value interface{}, name string) error {
	if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		constraint, _ := m.GuessConstraintInterfaceAndTable(stmt, name)
		if foreignKey, ok := constraint.(*schema.Constraint); ok {
			return m.ensureReferenceUniqueConstraint(stmt, foreignKey)
		}
		return nil
	}); err != nil {
		return err
	}
	return m.Migrator.CreateConstraint(value, name)
}

func (m Migrator) AddColumn(value interface{}, fieldName string) error {
	if err := m.Migrator.AddColumn(value, fieldName); err != nil {
		return err
	}
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema == nil {
			return nil
		}
		field := stmt.Schema.LookUpField(fieldName)
		if field == nil || field.Comment == "" {
			return nil
		}
		return m.commentColumn(stmt, field)
	})
}

func (m Migrator) commentColumn(stmt *gorm.Statement, field *schema.Field) error {
	placeholder := "?"
	if m.dialector.DatabaseMode() == ModeOracle {
		placeholder = ":1"
	}
	literal := clause.Expr{SQL: "NULL"}
	if field.Comment != "" {
		literal.SQL = m.dialector.Explain(placeholder, field.Comment)
	}
	return m.DB.Exec(
		"COMMENT ON COLUMN ?.? IS ?",
		m.CurrentTable(stmt), clause.Column{Name: field.DBName}, literal,
	).Error
}

// AlterColumn 使用各兼容模式原生接受的 MODIFY 形式。GORM 通用实现使用
// ALTER COLUMN TYPE；本实现按目标实例验证结果使用模式对应的 MODIFY。
func (m Migrator) AlterColumn(value interface{}, fieldName string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema == nil {
			return errors.New("kingbase: failed to get schema while altering column")
		}
		field := stmt.Schema.LookUpField(fieldName)
		if field == nil {
			return fmt.Errorf("kingbase: failed to look up field %q", fieldName)
		}
		// KingbaseES accepts the type/nullability change through MODIFY, but
		// MODIFY without an explicit NULL does not relax an existing NOT NULL.
		// Oracle mode also rejects DEFAULT appended to MODIFY, so handle the
		// default with its own ALTER COLUMN statement in both modes.
		fullType := m.DataTypeOf(field) + " NULL"
		if field.NotNull {
			fullType = m.DataTypeOf(field) + " NOT NULL"
		}
		var err error
		if m.dialector.DatabaseMode() == ModeOracle {
			err = m.DB.Exec(
				"ALTER TABLE ? MODIFY ? ?",
				m.CurrentTable(stmt), clause.Column{Name: field.DBName}, clause.Expr{SQL: fullType},
			).Error
		} else {
			err = m.DB.Exec(
				"ALTER TABLE ? MODIFY COLUMN ? ?",
				m.CurrentTable(stmt), clause.Column{Name: field.DBName}, clause.Expr{SQL: fullType},
			).Error
		}
		if err != nil {
			return err
		}
		if !field.AutoIncrement {
			defaultSQL := field.DefaultValue
			if field.DefaultValueInterface != nil {
				bindStmt := &gorm.Statement{Vars: []interface{}{field.DefaultValueInterface}}
				m.dialector.BindVarTo(bindStmt, bindStmt, field.DefaultValueInterface)
				defaultSQL = m.dialector.Explain(bindStmt.SQL.String(), field.DefaultValueInterface)
			}
			if field.HasDefaultValue && defaultSQL != "" && defaultSQL != "(-)" && !strings.EqualFold(defaultSQL, "null") {
				err = m.DB.Exec(
					"ALTER TABLE ? ALTER COLUMN ? SET DEFAULT ?",
					m.CurrentTable(stmt), clause.Column{Name: field.DBName}, clause.Expr{SQL: defaultSQL},
				).Error
			} else {
				err = m.DB.Exec(
					"ALTER TABLE ? ALTER COLUMN ? DROP DEFAULT",
					m.CurrentTable(stmt), clause.Column{Name: field.DBName},
				).Error
			}
			if err != nil {
				return err
			}
		}
		return m.commentColumn(stmt, field)
	})
}

func (m Migrator) DropTable(values ...interface{}) error {
	values = m.ReorderModels(values, false)
	for index := len(values) - 1; index >= 0; index-- {
		value := values[index]
		if m.dialector.DatabaseMode() == ModeOracle && !m.HasTable(value) {
			continue
		}
		if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
			if m.dialector.DatabaseMode() == ModeOracle {
				return m.DB.Exec("DROP TABLE ? CASCADE CONSTRAINTS", m.CurrentTable(stmt)).Error
			}
			return m.DB.Exec("DROP TABLE IF EXISTS ? CASCADE", m.CurrentTable(stmt)).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// indexIdentifier keeps an explicit table schema on standalone index DDL.
// Quote each identifier part before handing it to GORM, so dots and delimiter
// characters within a schema/index name remain literal identifier characters.
// No SET search_path is used: a pooled connection's session is not our target.
func (m Migrator) indexIdentifier(stmt *gorm.Statement, name string) clause.Column {
	schemaName, _ := m.CurrentSchema(stmt, stmt.Table)
	if namespace, ok := schemaName.(string); ok {
		quote := "`"
		if m.dialector.DatabaseMode() == ModeOracle {
			quote = `"`
		}
		part := func(value string) string {
			return quote + strings.ReplaceAll(value, quote, quote+quote) + quote
		}
		return clause.Column{Name: part(namespace) + "." + part(name)}
	}
	return clause.Column{Name: name}
}

// CreateIndex follows the tested modes' grammar: USING precedes the key
// list and WHERE follows the options. Oracle comments use a separate statement;
// MySQL inline comments are escaped as SQL literals, never interpolated raw.
func (m Migrator) CreateIndex(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema == nil {
			return errors.New("kingbase: failed to get schema while creating index")
		}
		idx := stmt.Schema.LookIndex(name)
		if idx == nil {
			return fmt.Errorf("kingbase: failed to find index %q", name)
		}
		opts := m.BuildIndexOptions(idx.Fields, stmt)
		values := []interface{}{clause.Column{Name: idx.Name}, m.CurrentTable(stmt), opts}
		createSQL := "CREATE "
		if idx.Class != "" {
			createSQL += idx.Class + " "
		}
		createSQL += "INDEX ? ON ?"
		if idx.Type != "" {
			createSQL += " USING " + idx.Type
		}
		createSQL += "?"
		placeholder := "?"
		if m.dialector.DatabaseMode() == ModeOracle {
			placeholder = ":1"
		}
		commentLiteral := clause.Expr{SQL: m.dialector.Explain(placeholder, idx.Comment)}
		if idx.Comment != "" && m.dialector.DatabaseMode() == ModeMySQL {
			createSQL += " COMMENT ?"
			values = append(values, commentLiteral)
		}
		if idx.Option != "" {
			createSQL += " " + idx.Option
		}
		if idx.Where != "" {
			createSQL += " WHERE " + idx.Where
		}
		if idx.Comment == "" || m.dialector.DatabaseMode() == ModeMySQL {
			return m.DB.Exec(createSQL, values...).Error
		}

		// Request rollback if either CREATE INDEX or COMMENT ON INDEX fails.
		// Transactional DDL behavior still depends on the target database.
		return m.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(createSQL, values...).Error; err != nil {
				return err
			}
			return tx.Exec("COMMENT ON INDEX ? IS ?", m.indexIdentifier(stmt, idx.Name), commentLiteral).Error
		})
	})
}

func (m Migrator) DropIndex(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema != nil {
			if index := stmt.Schema.LookIndex(name); index != nil {
				name = index.Name
			}
		}
		schemaName, tableName := m.CurrentSchema(stmt, stmt.Table)
		var uniqueConstraintCount int64
		if err := m.queryRaw(
			"SELECT count(*) FROM information_schema.table_constraints WHERE constraint_schema = ? AND table_name = ? AND constraint_name = ? AND constraint_type = 'UNIQUE'",
			schemaName, tableName, m.dialector.catalogName(name),
		).Scan(&uniqueConstraintCount).Error; err != nil {
			return err
		}
		if uniqueConstraintCount > 0 {
			return m.DB.Exec("ALTER TABLE ? DROP CONSTRAINT ?", m.CurrentTable(stmt), clause.Column{Name: name}).Error
		}
		// Both KingbaseES modes accept a standalone index name. The MySQL
		// mode rejects GORM's MySQL-style "DROP INDEX ... ON table" form.
		return m.DB.Exec("DROP INDEX ?", m.indexIdentifier(stmt, name)).Error
	})
}

func (m Migrator) RenameIndex(value interface{}, oldName, newName string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		// KingbaseES MySQL mode also renames the index directly. It rejects
		// MySQL's "ALTER TABLE ... RENAME INDEX ... TO ..." form.
		return m.DB.Exec(
			"ALTER INDEX ? RENAME TO ?",
			m.indexIdentifier(stmt, oldName), clause.Column{Name: newName},
		).Error
	})
}

func (m Migrator) GetTypeAliases(databaseTypeName string) []string {
	return kingbaseTypeAliases[strings.ToLower(databaseTypeName)]
}

var kingbaseTypeAliases = map[string][]string{
	"bool":                        {"boolean"},
	"boolean":                     {"bool"},
	"int":                         {"integer"},
	"int2":                        {"smallint"},
	"int4":                        {"integer"},
	"int8":                        {"bigint"},
	"smallint":                    {"int2"},
	"integer":                     {"int", "int4"},
	"bigint":                      {"int8"},
	"decimal":                     {"numeric", "number"},
	"numeric":                     {"decimal", "number"},
	"number":                      {"decimal", "numeric"},
	"character varying":           {"varchar", "varchar2"},
	"varchar":                     {"character varying", "varchar2"},
	"varchar2":                    {"character varying", "varchar"},
	"text":                        {"longtext", "mediumtext", "clob"},
	"clob":                        {"text", "longtext"},
	"timestamp without time zone": {"timestamp", "datetime"},
	"timestamp with time zone":    {"timestamptz", "timestamp"},
	"datetime":                    {"timestamp without time zone", "timestamp"},
	"bytea":                       {"blob", "longblob", "mediumblob", "varbinary"},
	"blob":                        {"bytea", "longblob"},
	"longblob":                    {"bytea", "blob"},
	"double precision":            {"double", "float"},
	"double":                      {"double precision"},
}

// GetIndexes 使用 KingbaseES 底层目录获取索引及有序列列表。information_schema
// 没有标准索引视图，而 pg_catalog 在两个目标模式中均由 KingbaseES 提供。
func (m Migrator) GetIndexes(value interface{}) ([]gorm.Index, error) {
	indexes := make([]gorm.Index, 0)
	err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		schemaName, tableName := m.CurrentSchema(stmt, stmt.Table)
		rows, err := m.queryRaw(indexMetadataSQL, schemaName, tableName).Rows()
		if err != nil {
			return err
		}
		defer rows.Close()

		type accumulated struct {
			value *migrator.Index
		}
		byName := map[string]*accumulated{}
		order := make([]string, 0)
		for rows.Next() {
			var table, name, column string
			var unique, primary bool
			if err := rows.Scan(&table, &name, &unique, &primary, &column); err != nil {
				return err
			}
			entry := byName[name]
			if entry == nil {
				entry = &accumulated{value: &migrator.Index{
					TableName:       table,
					NameValue:       name,
					UniqueValue:     sql.NullBool{Bool: unique, Valid: true},
					PrimaryKeyValue: sql.NullBool{Bool: primary, Valid: true},
				}}
				byName[name] = entry
				order = append(order, name)
			}
			entry.value.ColumnList = append(entry.value.ColumnList, column)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, name := range order {
			indexes = append(indexes, byName[name].value)
		}
		return nil
	})
	return indexes, err
}

const indexMetadataSQL = `
SELECT tbl.relname, idx.relname, ind.indisunique, ind.indisprimary,
       CASE WHEN keys.attnum = 0
            THEN pg_catalog.pg_get_indexdef(ind.indexrelid, keys.position::integer, true)
            ELSE attr.attname END
FROM pg_catalog.pg_index AS ind
JOIN pg_catalog.pg_class AS tbl ON tbl.oid = ind.indrelid
JOIN pg_catalog.pg_namespace AS ns ON ns.oid = tbl.relnamespace
JOIN pg_catalog.pg_class AS idx ON idx.oid = ind.indexrelid
JOIN LATERAL unnest(ind.indkey) WITH ORDINALITY AS keys(attnum, position) ON true
LEFT JOIN pg_catalog.pg_attribute AS attr ON attr.attrelid = tbl.oid AND attr.attnum = keys.attnum
WHERE ns.nspname = ? AND tbl.relname = ?
ORDER BY idx.relname, keys.position`

func (m Migrator) TableType(value interface{}) (gorm.TableType, error) {
	var result migrator.TableType
	err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		schemaName, tableName := m.CurrentSchema(stmt, stmt.Table)
		return m.queryRaw(
			"SELECT table_schema, table_name, table_type FROM information_schema.tables WHERE table_schema = ? AND table_name = ?",
			schemaName, tableName,
		).Row().Scan(&result.SchemaValue, &result.NameValue, &result.TypeValue)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
