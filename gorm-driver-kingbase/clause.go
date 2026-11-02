package kingbase

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (d *Dialector) ClauseBuilders() map[string]clause.ClauseBuilder {
	if d.DatabaseMode() == ModeOracle {
		return map[string]clause.ClauseBuilder{
			"LIMIT":       oracleLimitClauseBuilder,
			"ON CONFLICT": oracleOnConflictClauseBuilder,
		}
	}
	return map[string]clause.ClauseBuilder{
		"ON CONFLICT": mysqlOnConflictClauseBuilder,
		"VALUES":      mysqlValuesClauseBuilder,
	}
}

// Oracle mode requires a conflict target for DO UPDATE. GORM's association
// callback leaves it empty when a related model has no primary key. Infer a
// target only when the model declares exactly one usable unique key; choosing
// between several keys would silently change which conflict gets updated.
func oracleOnConflictClauseBuilder(current clause.Clause, builder clause.Builder) {
	onConflict, ok := current.Expression.(clause.OnConflict)
	if !ok || onConflict.DoNothing || onConflict.OnConstraint != "" || len(onConflict.Columns) > 0 {
		current.Build(builder)
		return
	}
	stmt, ok := builder.(*gorm.Statement)
	if !ok || stmt.Schema == nil {
		builder.AddError(errors.New("kingbase: Oracle ON CONFLICT DO UPDATE requires a conflict target"))
		return
	}

	var candidates [][]clause.Column
	seen := make(map[string]bool)
	addCandidate := func(names []string) {
		if len(names) == 0 {
			return
		}
		key := strings.Join(names, "\x00")
		if seen[key] {
			return
		}
		seen[key] = true
		columns := make([]clause.Column, len(names))
		for i, name := range names {
			columns[i] = clause.Column{Name: name}
		}
		candidates = append(candidates, columns)
	}
	addCandidate(stmt.Schema.PrimaryFieldDBNames)
	for _, index := range stmt.Schema.ParseIndexes() {
		if index.Class != "UNIQUE" || index.Where != "" || len(index.Fields) == 0 {
			continue
		}
		names := make([]string, 0, len(index.Fields))
		for _, option := range index.Fields {
			if option.Field == nil || option.Field.DBName == "" || option.Expression != "" || option.Length != 0 {
				names = nil
				break
			}
			names = append(names, option.Field.DBName)
		}
		addCandidate(names)
	}
	for _, constraint := range stmt.Schema.ParseUniqueConstraints() {
		if constraint.Field != nil {
			addCandidate([]string{constraint.Field.DBName})
		}
	}
	if len(candidates) != 1 {
		stmt.AddError(fmt.Errorf("kingbase: Oracle ON CONFLICT DO UPDATE requires an explicit conflict target; found %d eligible unique keys", len(candidates)))
		return
	}
	onConflict.Columns = candidates[0]
	current.Expression = onConflict
	current.Build(builder)
}

func mysqlValuesClauseBuilder(current clause.Clause, builder clause.Builder) {
	if values, ok := current.Expression.(clause.Values); ok && len(values.Columns) == 0 {
		_, _ = builder.WriteString("VALUES()")
		return
	}
	current.Build(builder)
}

// mysqlOnConflictClauseBuilder 把 GORM 的抽象 OnConflict 转成 MySQL 模式语法。
// KingbaseES 的 MySQL 模式也支持 ON CONFLICT DO NOTHING。必须保留其真正的
// 忽略语义：用 ON DUPLICATE KEY UPDATE 的空更新替代会让 RETURNING 返回冲突行，
// 导致 GORM 把旧行 ID 错填到同批次的新关联对象。
// 更新分支的 excluded.col 会变成 VALUES(col)，与官方 MySQL Dialector 保持一致。
func mysqlOnConflictClauseBuilder(current clause.Clause, builder clause.Builder) {
	onConflict, ok := current.Expression.(clause.OnConflict)
	if !ok {
		current.Build(builder)
		return
	}
	if onConflict.DoNothing {
		current.Build(builder)
		return
	}
	// ON DUPLICATE KEY UPDATE reacts to every unique-key conflict. When the
	// caller names a conflict target or adds a conflict WHERE condition, keep
	// GORM's ON CONFLICT form so another unique key cannot update the wrong row.
	// Both forms are accepted by the tested KingbaseES MySQL instance.
	if onConflict.OnConstraint != "" || len(onConflict.Columns) > 0 ||
		len(onConflict.TargetWhere.Exprs) > 0 || len(onConflict.Where.Exprs) > 0 {
		current.Build(builder)
		return
	}

	assignments := onConflict.DoUpdates
	if len(assignments) == 0 {
		if stmt, ok := builder.(*gorm.Statement); ok && stmt.Schema != nil {
			var column clause.Column
			if stmt.Schema.PrioritizedPrimaryField != nil {
				column = clause.Column{Name: stmt.Schema.PrioritizedPrimaryField.DBName}
			} else if len(stmt.Schema.DBNames) > 0 {
				column = clause.Column{Name: stmt.Schema.DBNames[0]}
			}
			if column.Name != "" {
				assignments = []clause.Assignment{{Column: column, Value: column}}
			}
		}
		if len(assignments) == 0 {
			// KingbaseES compatibility (KB-SQL-013): map-based creates may not
			// have a parsed Schema. Use the first VALUES column for MySQL's
			// required no-op assignment instead of emitting an invalid empty SET.
			if stmt, ok := builder.(*gorm.Statement); ok {
				if valuesClause, exists := stmt.Clauses["VALUES"]; exists {
					if values, ok := valuesClause.Expression.(clause.Values); ok && len(values.Columns) > 0 {
						column := values.Columns[0]
						assignments = []clause.Assignment{{Column: column, Value: column}}
					}
				}
				if len(assignments) == 0 {
					stmt.AddError(errors.New("kingbase: MySQL ON DUPLICATE KEY requires at least one insert column"))
					return
				}
			}
		}
	}

	_, _ = builder.WriteString("ON DUPLICATE KEY UPDATE ")
	for index, assignment := range assignments {
		if index > 0 {
			_ = builder.WriteByte(',')
		}
		builder.WriteQuoted(assignment.Column)
		_ = builder.WriteByte('=')
		if column, isColumn := assignment.Value.(clause.Column); isColumn && column.Table == "excluded" {
			column.Table = ""
			_, _ = builder.WriteString("VALUES(")
			builder.WriteQuoted(column)
			_ = builder.WriteByte(')')
		} else {
			builder.AddVar(builder, assignment.Value)
		}
	}
}

// oracleLimitClauseBuilder 使用 PoC 已验证可工作的 OFFSET/FETCH 语法。
// 没有 ORDER BY 时，完整模型结果优先补主键；投影、分组、去重或无主键的结果
// 使用 ORDER BY 1，避免引用不在结果／分组中的模型列。业务分页应显式给出唯一排序。
func oracleLimitClauseBuilder(current clause.Clause, builder clause.Builder) {
	limit, ok := current.Expression.(clause.Limit)
	if !ok {
		current.Build(builder)
		return
	}
	stmt, _ := builder.(*gorm.Statement)
	hasLimit := limit.Limit != nil && *limit.Limit >= 0
	hasOffset := limit.Offset > 0

	if (hasLimit || hasOffset) && stmt != nil {
		if _, hasOrder := stmt.Clauses["ORDER BY"]; !hasOrder {
			_, _ = builder.WriteString("ORDER BY ")
			_, grouped := stmt.Clauses["GROUP BY"]
			if !stmt.Distinct && !grouped && len(stmt.Selects) == 0 && stmt.Schema != nil && stmt.Schema.PrioritizedPrimaryField != nil {
				// KingbaseES compatibility (KB-SQL-009/010, KB-ADV-008): a
				// JOIN can expose several columns named id. Qualifying the fallback
				// order with the current table avoids an ambiguous Oracle query.
				builder.WriteQuoted(clause.Column{
					Table: clause.CurrentTable,
					Name:  stmt.Schema.PrioritizedPrimaryField.DBName,
				})
				_ = builder.WriteByte(' ')
			} else {
				// A projected/distinct/grouped result may not contain the model
				// primary key. Its first output expression is a legal fallback.
				_, _ = builder.WriteString("1 ")
			}
		}
	}

	if hasOffset {
		_, _ = builder.WriteString("OFFSET ")
		_, _ = builder.WriteString(strconv.Itoa(limit.Offset))
		_, _ = builder.WriteString(" ROWS ")
	}
	if hasLimit {
		_, _ = builder.WriteString("FETCH NEXT ")
		_, _ = builder.WriteString(strconv.Itoa(*limit.Limit))
		if *limit.Limit == 1 {
			_, _ = builder.WriteString(" ROW ONLY")
		} else {
			_, _ = builder.WriteString(" ROWS ONLY")
		}
	}
}
