// Command migration-metadata demonstrates requirements 6.2.5.
// All database objects belong to private schemas and are rolled back.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"kingbase.com/gokb"
	kingbase "kingbase.com/gorm-driver-kingbase"
)

type Initial struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"size:32;not null"`
}

func (Initial) TableName() string { return "schema_probe" }

type Evolved struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"size:64;not null;comment:名称说明"`
	Code string `gorm:"size:32;unique;index:idx_doc625_code"`
}

func (Evolved) TableName() string { return "schema_probe" }

type Neighbor struct {
	ID    uint   `gorm:"primaryKey"`
	Name  string `gorm:"size:128"`
	Extra int    `gorm:"index:idx_doc625_code"`
}

type InvalidIndex struct {
	Code string `gorm:"index:idx_doc625_bad,type:kb_doc_missing_method625"`
}

func main() {
	name := flag.String("case", "migration-ok", "migration-ok, migration-error, metadata-ok or metadata-error")
	mode := flag.String("mode", "", "expected actual mode: mysql or oracle")
	flag.Parse()
	cases := map[string]func(*gorm.DB) error{
		"migration-ok": migrationOK, "migration-error": migrationError,
		"metadata-ok": metadataOK, "metadata-error": metadataError,
	}
	run, known := cases[*name]
	if !known || (*mode != "mysql" && *mode != "oracle") {
		fmt.Fprintln(os.Stderr, "ERROR: specify a supported -case and -mode mysql|oracle")
		os.Exit(2)
	}
	fmt.Printf("case=%s expected_mode=%s\n", *name, *mode)
	db, closePool, err := openDatabase(*mode)
	if err == nil {
		err = run(db)
		closePool()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func migrationOK(db *gorm.DB) error {
	return withSchemas(db, func(tx *gorm.DB, a, _ string) error {
		table := a + ".schema_probe"
		if err := tx.Table(table).AutoMigrate(&Initial{}); err != nil {
			return err
		}
		seed := Initial{Name: "迁移前数据"}
		if err := tx.Table(table).Create(&seed).Error; err != nil {
			return err
		}
		// screenshot:migration-ok:start
		if err := tx.Table(table).AutoMigrate(&Evolved{}); err != nil {
			return err
		}
		if err := tx.Table(table).Migrator().
			CreateConstraint(&Evolved{}, "Code"); err != nil {
			return err
		}
		before, err := verifyEvolved(tx, table, seed.ID)
		if err != nil {
			return err
		}
		var ddl []string
		repeat := tx.Table(table).Session(&gorm.Session{
			Logger: ddlLog{Interface: logger.Discard, statements: &ddl},
		})
		if err := repeat.AutoMigrate(&Evolved{}); err != nil {
			return err
		}
		if err := verifyRepeat(tx, table, seed.ID, before, ddl); err != nil {
			return err
		}
		// screenshot:migration-ok:end
		fmt.Println("initial_create=ok upgrade=ok name_length=64 code_column=true")
		fmt.Println("index_columns=[code] unique_constraint=true data_preserved=true")
		fmt.Println("repeat_ddl_count=0 catalog_unchanged=true")
		return nil
	})
}

func migrationError(db *gorm.DB) error {
	return withSchemas(db, func(tx *gorm.DB, a, _ string) error {
		table := a + ".schema_probe"
		if err := tx.Table(table).AutoMigrate(&Evolved{}); err != nil {
			return err
		}
		// screenshot:migration-error:start
		err := tx.Table(table).Migrator().CreateIndex(
			&InvalidIndex{}, "idx_doc625_bad",
		)
		var databaseError *gokb.Error
		if !errors.As(err, &databaseError) || databaseError.Code != "42704" {
			return fmt.Errorf("expected undefined access method, got %v", err)
		}
		fmt.Printf("create_index_error=true sqlstate=%s\n", databaseError.Code)
		return err
		// screenshot:migration-error:end
	})
}

func metadataOK(db *gorm.DB) error {
	return withSchemas(db, func(tx *gorm.DB, a, b string) error {
		table := a + ".schema_probe"
		if err := tx.Table(table).AutoMigrate(&Evolved{}); err != nil {
			return err
		}
		if err := tx.Table(b + ".schema_probe").AutoMigrate(&Neighbor{}); err != nil {
			return err
		}
		for _, marker := range []string{a + ".only_a", b + ".only_b"} {
			if err := tx.Exec("CREATE TABLE ? (id integer)",
				clause.Table{Name: marker}).Error; err != nil {
				return err
			}
		}
		var path string
		if err := tx.Raw("SELECT set_config('search_path', ?, true)",
			b+", public").Row().Scan(&path); err != nil {
			return err
		}
		var current string
		if err := tx.Raw("SELECT CURRENT_SCHEMA()").Row().Scan(&current); err != nil || current != b {
			return fmt.Errorf("unexpected current schema %q: %v", current, err)
		}
		// screenshot:metadata-ok:start
		m := tx.Table(table).Migrator()
		columns, err := m.ColumnTypes(&Evolved{})
		if err != nil {
			return err
		}
		indexes, err := m.GetIndexes(&Evolved{})
		if err != nil {
			return err
		}
		tables, err := m.GetTables()
		if err != nil {
			return err
		}
		if err := verifyMetadata(tx, a, b, columns, indexes, tables); err != nil {
			return err
		}
		// screenshot:metadata-ok:end
		fmt.Println("explicit_target=A current_schema=B same_table_and_index_names=true")
		fmt.Println("A: tables=[only_a schema_probe] B: tables=[only_b schema_probe]")
		fmt.Println("A: columns=[id name code] name_length=64 index_columns=[code]")
		fmt.Println("B: columns=[id name extra] name_length=128 index_columns=[extra]")
		fmt.Println("A: primary_key=true identity=true unique=true comment=名称说明")
		fmt.Println("schema_isolation_check=PASS")
		return nil
	})
}

func metadataError(db *gorm.DB) error {
	return withSchemas(db, func(tx *gorm.DB, a, _ string) error {
		table := a + ".schema_probe"
		if err := tx.Table(table).AutoMigrate(&Evolved{}); err != nil {
			return err
		}
		// screenshot:metadata-error:start
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		m := tx.WithContext(ctx).Table(table).Migrator()
		_, err := m.ColumnTypes(&Evolved{})
		if !errors.Is(err, context.Canceled) {
			return fmt.Errorf("expected context.Canceled, got %v", err)
		}
		if m.HasTable(&Evolved{}) {
			return errors.New("unexpected HasTable result for canceled context")
		}
		fmt.Println("column_types_error_is_context_canceled=true")
		fmt.Println("has_table_with_canceled_context=false table_existed_before=true")
		return err
		// screenshot:metadata-error:end
	})
}

type nativeColumn struct {
	Name, Type, Identity, Comment string
	NotNull                       bool
}

type nativeIndex struct{ Name, Definition string }

type catalogSnapshot struct {
	Columns []nativeColumn
	Indexes []nativeIndex
}

func snapshot(tx *gorm.DB, table string) (catalogSnapshot, error) {
	parts := strings.Split(table, ".") // Names are generated internally, unquoted.
	var out catalogSnapshot
	err := tx.Raw(`SELECT a.attname AS name,
pg_catalog.format_type(a.atttypid, a.atttypmod) AS type,
a.attnotnull AS not_null, a.attidentity AS identity,
COALESCE(pg_catalog.col_description(c.oid, a.attnum), '') AS comment
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=? AND c.relname=? AND a.attnum>0 AND NOT a.attisdropped
ORDER BY a.attnum`, parts[0], parts[1]).Scan(&out.Columns).Error
	if err != nil {
		return out, err
	}
	err = tx.Raw(`SELECT indexname AS name, indexdef AS definition
FROM pg_catalog.pg_indexes WHERE schemaname=? AND tablename=?
ORDER BY indexname`, parts[0], parts[1]).Scan(&out.Indexes).Error
	return out, err
}

func verifyEvolved(tx *gorm.DB, table string, id uint) (catalogSnapshot, error) {
	state, err := snapshot(tx, table)
	if err != nil {
		return state, err
	}
	if len(state.Columns) != 3 || state.Columns[0].Name != "id" ||
		state.Columns[1].Name != "name" || state.Columns[2].Name != "code" ||
		!strings.Contains(state.Columns[1].Type, "(64") ||
		!state.Columns[1].NotNull || state.Columns[1].Comment != "名称说明" {
		return state, fmt.Errorf("unexpected native columns: %+v", state.Columns)
	}
	m := tx.Table(table).Migrator()
	indexes, err := m.GetIndexes(&Evolved{})
	if err != nil {
		return state, err
	}
	if !hasIndexColumns(indexes, []string{"code"}) ||
		!m.HasConstraint(&Evolved{}, "uni_schema_probe_code") {
		return state, errors.New("missing expected index or unique constraint")
	}
	var stored Evolved
	if err := tx.Table(table).First(&stored, id).Error; err != nil {
		return state, err
	}
	if id == 0 || stored.ID != id || stored.Name != "迁移前数据" {
		return state, fmt.Errorf("migration changed stored data: %+v", stored)
	}
	return state, nil
}

func verifyRepeat(tx *gorm.DB, table string, id uint, before catalogSnapshot, ddl []string) error {
	after, err := verifyEvolved(tx, table, id)
	if err != nil {
		return err
	}
	if len(ddl) != 0 || !reflect.DeepEqual(before, after) {
		return fmt.Errorf("repeat migration changed catalog or issued DDL: %v", ddl)
	}
	return nil
}

func verifyMetadata(tx *gorm.DB, a, b string, columns []gorm.ColumnType,
	indexes []gorm.Index, tables []string) error {
	if !reflect.DeepEqual(tables, []string{"only_a", "schema_probe"}) || len(columns) != 3 ||
		!hasIndexColumns(indexes, []string{"code"}) {
		return errors.New("A table/column/index discovery mismatch")
	}
	wantNames := []string{"id", "name", "code"}
	for i, column := range columns {
		if column.Name() != wantNames[i] || column.ScanType() == nil {
			return fmt.Errorf("unexpected column[%d]: %s", i, column.Name())
		}
		switch column.Name() {
		case "id":
			pk, pkKnown := column.PrimaryKey()
			auto, autoKnown := column.AutoIncrement()
			if !pkKnown || !pk || !autoKnown || !auto {
				return errors.New("A primary key or identity metadata mismatch")
			}
		case "name":
			n, known := column.Length()
			nullable, nullableKnown := column.Nullable()
			comment, commentKnown := column.Comment()
			if !known || n != 64 || !nullableKnown || nullable ||
				!commentKnown || comment != "名称说明" {
				return errors.New("A length/nullability/comment metadata mismatch")
			}
		case "code":
			unique, known := column.Unique()
			if !known || !unique {
				return errors.New("A unique constraint metadata mismatch")
			}
		}
	}
	ma := tx.Table(a + ".schema_probe").Migrator()
	mb := tx.Table(b + ".schema_probe").Migrator()
	if !ma.HasTable(&Evolved{}) || ma.HasColumn(&Evolved{}, "extra") ||
		!ma.HasColumn(&Evolved{}, "Code") || !ma.HasIndex(&Evolved{}, "idx_doc625_code") ||
		!ma.HasConstraint(&Evolved{}, "uni_schema_probe_code") ||
		!mb.HasColumn(&Neighbor{}, "extra") || mb.HasColumn(&Neighbor{}, "code") {
		return errors.New("schema-scoped presence checks mismatch")
	}
	other, err := mb.ColumnTypes(&Neighbor{})
	if err != nil || len(other) != 3 {
		return fmt.Errorf("B column discovery failed: %v", err)
	}
	for i, name := range []string{"id", "name", "extra"} {
		if other[i].Name() != name {
			return fmt.Errorf("unexpected B column[%d]: %s", i, other[i].Name())
		}
	}
	otherTables, err := mb.GetTables()
	if err != nil || !reflect.DeepEqual(otherTables, []string{"only_b", "schema_probe"}) {
		return fmt.Errorf("B table discovery mismatch: tables=%v error=%v", otherTables, err)
	}
	if n, known := other[1].Length(); other[1].Name() != "name" || !known || n != 128 {
		return errors.New("B column length mismatch")
	}
	otherIndexes, err := mb.GetIndexes(&Neighbor{})
	if err != nil || !hasIndexColumns(otherIndexes, []string{"extra"}) {
		return fmt.Errorf("B index discovery mismatch: %v", err)
	}
	for _, scope := range []string{a, b} {
		kind, err := tx.Table(scope + ".schema_probe").Migrator().TableType("schema_probe")
		if err != nil || kind.Schema() != scope || kind.Name() != "schema_probe" || kind.Type() != "BASE TABLE" {
			return fmt.Errorf("table type discovery mismatch in %s: %v", scope, err)
		}
	}
	state, err := snapshot(tx, a+".schema_probe")
	if err != nil || len(state.Columns) != 3 || !strings.Contains(state.Columns[1].Type, "(64") {
		return fmt.Errorf("A native catalog cross-check failed: %v", err)
	}
	return nil
}

func hasIndexColumns(indexes []gorm.Index, want []string) bool {
	for _, index := range indexes {
		if index.Name() == "idx_doc625_code" {
			return reflect.DeepEqual(index.Columns(), want)
		}
	}
	return false
}

type ddlLog struct {
	logger.Interface
	statements *[]string
}

func (l ddlLog) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sqlText, _ := fc()
	upper := strings.ToUpper(strings.TrimSpace(sqlText))
	for _, prefix := range []string{"CREATE ", "ALTER ", "DROP ", "COMMENT ", "RENAME ", "TRUNCATE "} {
		if strings.HasPrefix(upper, prefix) {
			*l.statements = append(*l.statements, sqlText)
			return
		}
	}
}

func openDatabase(mode string) (*gorm.DB, func(), error) {
	dsn := os.Getenv("KINGBASE_DSN")
	if dsn == "" {
		return nil, nil, errors.New("KINGBASE_DSN is required; configure the target test instance first")
	}
	db, err := gorm.Open(kingbase.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, nil, err
	}
	pool, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	closePool := func() { _ = pool.Close() }
	actual := string(db.Dialector.(*kingbase.Dialector).DatabaseMode())
	if actual != mode {
		closePool()
		return nil, nil, fmt.Errorf("actual mode=%s, expected %s", actual, mode)
	}
	fmt.Printf("detected_mode=%s\n", actual)
	return db, closePool, nil
}

func withSchemas(db *gorm.DB, body func(*gorm.DB, string, string) error) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return tx.Error
	}
	suffix := time.Now().UnixNano()
	a, b := fmt.Sprintf("gorm_doc625_a_%d", suffix), fmt.Sprintf("gorm_doc625_b_%d", suffix)
	defer func() {
		rollbackErr := tx.Rollback().Error
		fresh, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var count int64
		cleanupErr := db.WithContext(fresh).Raw(
			"SELECT count(*) FROM pg_catalog.pg_namespace WHERE nspname IN (?, ?)", a, b,
		).Row().Scan(&count)
		if cleanupErr == nil && count != 0 {
			cleanupErr = fmt.Errorf("fixture namespaces remain: count=%d", count)
		}
		if rollbackErr == nil && cleanupErr == nil {
			fmt.Println("fixture_cleanup_count=0")
		}
		err = errors.Join(err, rollbackErr, cleanupErr)
	}()
	for _, namespace := range []string{a, b} {
		if err := tx.Exec("CREATE SCHEMA ?", clause.Column{Name: namespace}).Error; err != nil {
			return err
		}
	}
	return body(tx, a, b)
}
