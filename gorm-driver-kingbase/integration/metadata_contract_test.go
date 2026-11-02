package integration_test

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestCurrentDatabaseContract(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		var database, schema string
		if err := db.Raw("SELECT CURRENT_DATABASE(), CURRENT_SCHEMA()").Row().Scan(&database, &schema); err != nil {
			t.Fatal(err)
		}
		if got := db.Migrator().CurrentDatabase(); database == "" || got != database {
			t.Fatalf("CurrentDatabase=%q actual database=%q schema=%q", got, database, schema)
		}
	})
}

type precisionV1 struct {
	ID     uint      `gorm:"primaryKey"`
	Name   string    `gorm:"size:64"`
	Amount float64   `gorm:"precision:12;scale:4"`
	At     time.Time `gorm:"precision:4"`
}

func (precisionV1) TableName() string { return "gorm_kb_accept_precision_migration" }

type precisionV2 struct {
	ID     uint      `gorm:"primaryKey"`
	Name   string    `gorm:"size:128"`
	Amount float64   `gorm:"precision:14;scale:6"`
	At     time.Time `gorm:"precision:2"`
}

func (precisionV2) TableName() string { return (precisionV1{}).TableName() }

func TestPrecisionMigration(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		resetAcceptanceTables(t, db, &precisionV1{})
		before := precisionV1{Name: "preserved", Amount: 9876.125, At: time.Date(2025, 1, 2, 3, 4, 5, 120000000, time.UTC)}
		if err := db.Create(&before).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&precisionV2{}); err != nil {
			t.Fatal(err)
		}
		columns, err := db.Migrator().ColumnTypes(&precisionV2{})
		if err != nil || len(columns) != 4 {
			t.Fatalf("columns=%v err=%v", columns, err)
		}
		seen := map[string]bool{}
		for _, column := range columns {
			if seen[column.Name()] {
				t.Fatalf("duplicate metadata %s", column.Name())
			}
			seen[column.Name()] = true
			switch column.Name() {
			case "id":
				if auto, known := column.AutoIncrement(); !known || !auto {
					t.Fatalf("identity=(%t,%t)", auto, known)
				}
			case "name":
				if length, known := column.Length(); !known || length != 128 {
					t.Fatalf("name length=(%d,%t)", length, known)
				}
			case "amount":
				if p, s, known := column.DecimalSize(); !known || p != 14 || s != 6 {
					t.Fatalf("amount decimal=(%d,%d,%t)", p, s, known)
				}
			case "at":
				if p, _, known := column.DecimalSize(); !known || p != 2 {
					t.Fatalf("time precision=(%d,%t)", p, known)
				}
			default:
				t.Fatalf("unexpected column %s", column.Name())
			}
		}
		var p, s, datetime int64
		if err := db.Raw("SELECT numeric_precision,numeric_scale FROM information_schema.columns WHERE table_schema=CURRENT_SCHEMA() AND table_name=? AND column_name='amount'", before.TableName()).Row().Scan(&p, &s); err != nil || p != 14 || s != 6 {
			t.Fatalf("catalog decimal=%d,%d err=%v", p, s, err)
		}
		if err := db.Raw("SELECT datetime_precision FROM information_schema.columns WHERE table_schema=CURRENT_SCHEMA() AND table_name=? AND column_name='at'", before.TableName()).Row().Scan(&datetime); err != nil || datetime != 2 {
			t.Fatalf("catalog time precision=%d err=%v", datetime, err)
		}
		var stored precisionV2
		if err := db.First(&stored, before.ID).Error; err != nil || stored.Name != "preserved" || stored.Amount != 9876.125 || !stored.At.Equal(before.At) {
			t.Fatalf("migrated data=%#v err=%v", stored, err)
		}
		var statements []string
		session := db.Session(&gorm.Session{Logger: migrationTraceLogger{Interface: db.Logger, statements: &statements}})
		if err := session.AutoMigrate(&precisionV2{}); err != nil {
			t.Fatal(err)
		}
		for _, statement := range statements {
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(statement)), "ALTER TABLE") {
				t.Errorf("repeat migration changed schema: %s", statement)
			}
		}
	})
}

type schemaScopeA struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"size:64;index:idx_kb_scope"`
}
type schemaScopeB struct {
	ID    uint   `gorm:"primaryKey"`
	Name  string `gorm:"size:128"`
	Extra int    `gorm:"index:idx_kb_scope"`
}

func TestMetadataSchemaIsolation(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		for i, schema := range []string{"gorm_kb_accept_scope_a", "gorm_kb_accept_scope_b"} {
			if err := db.Exec("DROP SCHEMA IF EXISTS ? CASCADE", clause.Column{Name: schema}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("CREATE SCHEMA ?", clause.Column{Name: schema}).Error; err != nil {
				t.Fatal(err)
			}
			schema := schema
			t.Cleanup(func() {
				if err := db.Exec("DROP SCHEMA ? CASCADE", clause.Column{Name: schema}).Error; err != nil {
					t.Errorf("cleanup schema %s: %v", schema, err)
				}
			})
			model := interface{}(&schemaScopeA{})
			if i == 1 {
				model = &schemaScopeB{}
			}
			table := schema + ".scope_probe"
			if err := db.Table(table).AutoMigrate(model); err != nil {
				t.Fatal(err)
			}
			for _, viaTable := range []bool{false, true} {
				m, value := db.Migrator(), interface{}(table)
				if viaTable {
					m, value = db.Table(table).Migrator(), model
				}
				if !m.HasTable(value) || !m.HasColumn(value, "name") || m.HasColumn(value, "missing") || m.HasColumn(value, "extra") != (i == 1) {
					t.Fatalf("wrong scoped presence for %s", table)
				}
				columns, err := m.ColumnTypes(value)
				if err != nil || len(columns) != 2+i {
					t.Fatalf("%s columns=%v err=%v", table, columns, err)
				}
				wantColumns := map[string]bool{"id": true, "name": true}
				if i == 1 {
					wantColumns["extra"] = true
				}
				for _, column := range columns {
					if !wantColumns[column.Name()] {
						t.Fatalf("scoped column metadata: %s unexpected or duplicate column %q", table, column.Name())
					}
					delete(wantColumns, column.Name())
					if column.Name() == "name" {
						if n, known := column.Length(); !known || n != int64(64+64*i) {
							t.Fatalf("%s name length=(%d,%t)", table, n, known)
						}
					}
					if column.Name() == "id" {
						if primary, known := column.PrimaryKey(); !known || !primary {
							t.Fatalf("%s id primary=(%t,%t)", table, primary, known)
						}
						if auto, known := column.AutoIncrement(); !known || !auto {
							t.Fatalf("%s id identity=(%t,%t)", table, auto, known)
						}
					}
				}
				if len(wantColumns) != 0 {
					t.Fatalf("scoped column metadata: %s missing columns %v", table, wantColumns)
				}
				indexes, err := m.GetIndexes(value)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				want := []string{"name"}
				if i == 1 {
					want = []string{"extra"}
				}
				for _, index := range indexes {
					if index.Name() == "idx_kb_scope" {
						found = true
						if !reflect.DeepEqual(index.Columns(), want) {
							t.Fatalf("%s index columns=%v want=%v", table, index.Columns(), want)
						}
					}
				}
				if !found || !m.HasIndex(value, "idx_kb_scope") || m.HasIndex(value, "missing_index") {
					t.Fatalf("%s wrong index discovery", table)
				}
				kind, err := m.TableType(value)
				if err != nil || kind.Schema() != schema || kind.Name() != "scope_probe" || kind.Type() != "BASE TABLE" {
					t.Fatalf("%s TableType=%#v err=%v", table, kind, err)
				}
			}
		}
	})
}

func TestSingleConnectionSessionState(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		if err := db.Connection(func(tx *gorm.DB) (result error) {
			if _, ok := tx.Statement.ConnPool.(*sql.Conn); !ok {
				return fmt.Errorf("Connection pool=%T, want pinned *sql.Conn", tx.Statement.ConnPool)
			}
			var before string
			var first, second int64
			if err := tx.Raw("SELECT current_setting('application_name'), pg_backend_pid()").Row().Scan(&before, &first); err != nil {
				return err
			}
			defer func() {
				var restored string
				if err := tx.Raw("SELECT set_config('application_name', ?, false)", before).Row().Scan(&restored); err != nil {
					result = errors.Join(result, err)
				} else if restored != before {
					result = errors.Join(result, errors.New("session state was not restored"))
				}
			}()
			var set, got string
			if err := tx.Raw("SELECT set_config('application_name', ?, false)", "gorm_kb_accept_session").Row().Scan(&set); err != nil {
				return err
			}
			if err := tx.Raw("SELECT current_setting('application_name'), pg_backend_pid()").Row().Scan(&got, &second); err != nil {
				return err
			}
			if set != "gorm_kb_accept_session" || got != set || first != second {
				return fmt.Errorf("session state=%q/%q backend=%d/%d", set, got, first, second)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
