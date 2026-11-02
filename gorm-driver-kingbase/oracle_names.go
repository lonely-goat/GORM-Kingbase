package kingbase

import (
	"strings"

	"gorm.io/gorm"
)

// catalogName mirrors the tested Oracle-mode identifier rule: quoted names
// containing only uppercase letters fold to lowercase, mixed case is preserved.
// Keep this distinct from SQL quoting; metadata values are bound as strings.
func (d *Dialector) catalogName(name string) string {
	if d.DatabaseMode() == ModeOracle && name == strings.ToUpper(name) {
		return strings.ToLower(name)
	}
	return name
}

// mapOracleColumns repairs the boundary between physical result labels and
// explicit uppercase GORM column tags. Only the statement mapping is copied;
// cached schemas and caller-supplied MapColumns maps must remain immutable.
func (d *Dialector) mapOracleColumns(db *gorm.DB) {
	stmt := db.Statement
	if db.Error != nil || stmt.Schema == nil {
		return
	}
	var mapping map[string]string
	add := func(logical string) {
		physical := d.catalogName(logical)
		if physical == logical || logical == "" {
			return
		}
		if _, explicit := stmt.ColumnMapping[physical]; explicit {
			return
		}
		if mapping == nil {
			mapping = make(map[string]string, len(stmt.ColumnMapping)+len(stmt.Schema.Fields))
			for from, to := range stmt.ColumnMapping {
				mapping[from] = to
			}
		}
		mapping[physical] = logical
	}
	for _, field := range stmt.Schema.Fields {
		add(field.DBName)
	}
	// GORM names joined result columns Relation__COLUMN. An all-uppercase
	// relation and column fold together; mixed-case alias components do not.
	for _, join := range stmt.Joins {
		current := stmt.Schema
		prefix := ""
		parts := strings.Split(join.Name, ".")
		for index, part := range parts {
			relation, ok := current.Relationships.Relations[part]
			if !ok {
				break
			}
			if prefix != "" {
				prefix += "__"
			}
			prefix += relation.Name
			alias := prefix
			if index == len(parts)-1 && join.Alias != "" {
				alias = join.Alias
			}
			for _, field := range relation.FieldSchema.Fields {
				if field.DBName != "" {
					add(alias + "__" + field.DBName)
				}
			}
			current = relation.FieldSchema
		}
	}
	if mapping != nil {
		stmt.ColumnMapping = mapping
	}
}
