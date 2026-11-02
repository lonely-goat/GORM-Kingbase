package integration_test

import (
	"context"
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

// These regressions deliberately make the search path disagree with the API's
// explicit target. Both the requested change and the untouched neighbour must
// be checked. Every schema and all of its objects are rolled back afterwards.
func withIndexSchemas(t *testing.T, db *gorm.DB, fn func(*gorm.DB, string, string)) {
	t.Helper()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	a, b := "gorm_kb_fix_a_"+suffix, "gorm_kb_fix_b_"+suffix
	t.Cleanup(func() {
		if err := tx.Rollback().Error; err != nil {
			t.Errorf("rollback index fixture: %v", err)
		}
		var remaining int64
		if err := db.Raw("SELECT count(*) FROM pg_catalog.pg_namespace WHERE nspname IN (?, ?)", a, b).Scan(&remaining).Error; err != nil || remaining != 0 {
			t.Errorf("index fixture cleanup: remaining=%d error=%v", remaining, err)
		}
	})
	for _, schema := range []string{a, b} {
		if err := tx.Exec("CREATE SCHEMA ?", clause.Column{Name: schema}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var path string
	if err := tx.Raw("SELECT set_config('search_path', ?, true)", b+", public").Row().Scan(&path); err != nil {
		t.Fatal(err)
	}
	fn(tx, a, b)
}

func requireCatalogIndex(t *testing.T, db *gorm.DB, schema, name string, want bool) {
	t.Helper()
	var count int64
	if err := db.Raw("SELECT count(*) FROM pg_catalog.pg_indexes WHERE schemaname=? AND tablename='probe' AND indexname=?", schema, name).Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	if (count == 1) != want || count > 1 {
		t.Fatalf("catalog index %s.%s count=%d want present=%t", schema, name, count, want)
	}
}

type scopedIndexModel struct {
	Name string `gorm:"size:64;index:kb_fix_idx,comment:owner's scoped index"`
}

func TestSchemaIndexOperations(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		for _, viaTable := range []bool{false, true} {
			operations := []string{"drop", "rename", "unique-constraint"}
			// Dynamic model creation uses db.Table; only label its actual API.
			if viaTable {
				operations = append(operations, "comment")
			}
			for _, operation := range operations {
				t.Run(fmt.Sprintf("%s/table-api=%t", operation, viaTable), func(t *testing.T) {
					withIndexSchemas(t, db, func(tx *gorm.DB, a, b string) {
						for _, schema := range []string{a, b} {
							if schema == a && operation == "comment" {
								continue
							}
							ddl := "CREATE TABLE ? (name varchar(64))"
							if operation == "unique-constraint" {
								ddl = "CREATE TABLE ? (name varchar(64), CONSTRAINT kb_fix_idx UNIQUE(name))"
							}
							if err := tx.Exec(ddl, clause.Table{Name: schema + ".probe"}).Error; err != nil {
								t.Fatal(err)
							}
							if operation != "unique-constraint" {
								if err := tx.Exec("CREATE INDEX kb_fix_idx ON ? (name)", clause.Table{Name: schema + ".probe"}).Error; err != nil {
									t.Fatal(err)
								}
							}
							requireCatalogIndex(t, tx, schema, "kb_fix_idx", true)
						}
						m, value := tx.Migrator(), interface{}(a+".probe")
						if viaTable {
							m, value = tx.Table(a+".probe").Migrator(), &scopedIndexModel{}
						}
						switch operation {
						case "drop", "unique-constraint":
							if err := m.DropIndex(value, "kb_fix_idx"); err != nil {
								t.Fatal(err)
							}
							requireCatalogIndex(t, tx, a, "kb_fix_idx", false)
							requireCatalogIndex(t, tx, b, "kb_fix_idx", true)
						case "rename":
							if err := m.RenameIndex(value, "kb_fix_idx", "kb_fix_renamed"); err != nil {
								t.Fatal(err)
							}
							requireCatalogIndex(t, tx, a, "kb_fix_idx", false)
							requireCatalogIndex(t, tx, a, "kb_fix_renamed", true)
							requireCatalogIndex(t, tx, b, "kb_fix_idx", true)
							requireCatalogIndex(t, tx, b, "kb_fix_renamed", false)
						case "comment":
							if err := tx.Exec("COMMENT ON INDEX ? IS 'neighbour remains'", clause.Column{Name: b + ".kb_fix_idx"}).Error; err != nil {
								t.Fatal(err)
							}
							if err := tx.Table(a + ".probe").AutoMigrate(&scopedIndexModel{}); err != nil {
								t.Fatal(err)
							}
							for schema, want := range map[string]string{a: "owner's scoped index", b: "neighbour remains"} {
								var comment string
								if err := tx.Raw("SELECT COALESCE(obj_description(c.oid,'pg_class'),'') FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=? AND c.relname='kb_fix_idx'", schema).Row().Scan(&comment); err != nil || comment != want {
									t.Fatalf("index comment %s=%q want=%q error=%v", schema, comment, want, err)
								}
							}
						}
					})
				})
			}
		}
	})
}

type partialUnique struct {
	Name   string `gorm:"size:64;uniqueIndex:kb_fix_partial,where:active = true"`
	Active bool
}
type partialUniqueComment struct {
	Name   string `gorm:"size:64;uniqueIndex:kb_fix_partial,where:active = true,comment:owner's partial index"`
	Active bool
}
type partialPlain struct {
	Name   string `gorm:"size:64;index:kb_fix_partial,where:active = true"`
	Active bool
}
type partialPlainComment struct {
	Name   string `gorm:"size:64;index:kb_fix_partial,where:active = true,comment:owner's partial index"`
	Active bool
}

func TestPartialIndexPredicateAndUniqueness(t *testing.T) {
	runAcceptanceModes(t, &gorm.Config{TranslateError: true}, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		for _, tc := range []struct {
			name    string
			model   interface{}
			unique  bool
			comment string
		}{
			{"unique", &partialUnique{}, true, ""},
			{"unique-comment", &partialUniqueComment{}, true, "owner's partial index"},
			{"plain", &partialPlain{}, false, ""},
			{"plain-comment", &partialPlainComment{}, false, "owner's partial index"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				withIndexSchemas(t, db, func(tx *gorm.DB, a, _ string) {
					table := a + ".probe"
					if err := tx.Table(table).AutoMigrate(tc.model); err != nil {
						t.Fatal(err)
					}
					var predicate sql.NullString
					var unique bool
					var comment string
					if err := tx.Raw("SELECT pg_get_expr(i.indpred,i.indrelid), i.indisunique, COALESCE(obj_description(c.oid,'pg_class'),'') FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indexrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=? AND c.relname='kb_fix_partial'", a).Row().Scan(&predicate, &unique, &comment); err != nil {
						t.Fatal(err)
					}
					if !predicate.Valid || !strings.Contains(predicate.String, "active") || unique != tc.unique || comment != tc.comment {
						t.Fatalf("partial index predicate=%v unique=%t comment=%q want unique=%t comment=%q", predicate, unique, comment, tc.unique, tc.comment)
					}
					// Equal names outside the predicate and one inside must coexist.
					if err := tx.Exec("INSERT INTO ? (name,active) VALUES ('shared',false),('shared',false),('shared',true)", clause.Table{Name: table}).Error; err != nil {
						t.Fatalf("rows allowed by partial uniqueness were rejected: %v", err)
					}
					err := tx.Transaction(func(nested *gorm.DB) error {
						return nested.Exec("INSERT INTO ? (name,active) VALUES ('shared',true)", clause.Table{Name: table}).Error
					})
					wantCount := int64(4)
					if tc.unique {
						wantCount = 3
						if !errors.Is(err, gorm.ErrDuplicatedKey) {
							t.Fatalf("duplicate within predicate error=%v want ErrDuplicatedKey", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					var count int64
					if err := tx.Table(table).Count(&count).Error; err != nil || count != wantCount {
						t.Fatalf("partial index rows=%d want=%d error=%v", count, wantCount, err)
					}
					var statements []string
					repeat := tx.Session(&gorm.Session{Logger: migrationTraceLogger{Interface: tx.Logger, statements: &statements}})
					if err := repeat.Table(table).AutoMigrate(tc.model); err != nil {
						t.Fatal(err)
					}
					for _, statement := range statements {
						if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(statement)), "CREATE ") {
							t.Errorf("repeat migration recreated object: %s", statement)
						}
					}
				})
			})
		}
	})
}

type indexMethodModel struct {
	Name string `gorm:"size:64;index:kb_fix_method,type:btree"`
}
type indexMethodCommentModel struct {
	Name string `gorm:"size:64;index:kb_fix_method,type:btree,comment:explicit method"`
}
type hashIndexMethodModel struct {
	Name string `gorm:"size:64;index:kb_fix_method,type:hash"`
}
type hashIndexMethodCommentModel struct {
	Name string `gorm:"size:64;index:kb_fix_method,type:hash,comment:explicit hash method"`
}

func TestIndexMethodPlacement(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		for _, tc := range []struct {
			name, method string
			model        interface{}
		}{
			{"btree", "btree", &indexMethodModel{}},
			{"btree-comment", "btree", &indexMethodCommentModel{}},
			{"hash", "hash", &hashIndexMethodModel{}},
			{"hash-comment", "hash", &hashIndexMethodCommentModel{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				withIndexSchemas(t, db, func(tx *gorm.DB, a, _ string) {
					if err := tx.Table(a + ".probe").AutoMigrate(tc.model); err != nil {
						t.Fatal(err)
					}
					var method string
					if err := tx.Raw("SELECT am.amname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace JOIN pg_catalog.pg_am am ON am.oid=c.relam WHERE n.nspname=? AND c.relname='kb_fix_method'", a).Row().Scan(&method); err != nil || method != tc.method {
						t.Fatalf("index method=%q want=%s error=%v", method, tc.method, err)
					}
				})
			})
		}
	})
}

func TestExpressionIndexMetadata(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		withIndexSchemas(t, db, func(tx *gorm.DB, a, _ string) {
			table := a + ".probe"
			for _, ddl := range []string{
				"CREATE TABLE ? (age integer, code integer, name varchar(64))",
				"CREATE INDEX kb_fix_expression ON ? ((age + 1))",
				"CREATE UNIQUE INDEX kb_fix_mixed ON ? (code, (age + 1), name)",
			} {
				if err := tx.Exec(ddl, clause.Table{Name: table}).Error; err != nil {
					t.Fatal(err)
				}
			}
			indexes, err := tx.Migrator().GetIndexes(table)
			if err != nil || len(indexes) != 2 {
				t.Fatalf("expression indexes count=%d want=2 error=%v", len(indexes), err)
			}
			seen := map[string]bool{}
			for _, index := range indexes {
				if seen[index.Name()] {
					t.Fatalf("duplicate index %s", index.Name())
				}
				seen[index.Name()] = true
				columns := index.Columns()
				for i := range columns {
					columns[i] = strings.NewReplacer(" ", "", "(", "", ")", "").Replace(columns[i])
				}
				want := []string{"age+1"}
				wantUnique := false
				if index.Name() == "kb_fix_mixed" {
					want, wantUnique = []string{"code", "age+1", "name"}, true
				} else if index.Name() != "kb_fix_expression" {
					t.Fatalf("unexpected index %s", index.Name())
				}
				if !reflect.DeepEqual(columns, want) {
					t.Errorf("index %s keys=%v want=%v", index.Name(), columns, want)
				}
				if unique, known := index.Unique(); !known || unique != wantUnique {
					t.Errorf("index %s unique=(%t,%t) want=%t", index.Name(), unique, known, wantUnique)
				}
				if primary, known := index.PrimaryKey(); !known || primary {
					t.Errorf("index %s primary=(%t,%t)", index.Name(), primary, known)
				}
			}
			for _, name := range []string{"kb_fix_expression", "kb_fix_mixed"} {
				requireCatalogIndex(t, tx, a, name, true)
				if !tx.Migrator().HasIndex(table, name) || !seen[name] {
					t.Errorf("missing expression index %s", name)
				}
			}
		})
	})
}

type explicitTimePrecisions struct {
	ZeroAt    time.Time `gorm:"precision:0"`
	DefaultAt time.Time
	MilliAt   time.Time `gorm:"precision:3"`
}

func TestExplicitZeroTimePrecision(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, mode acceptanceMode) {
		withIndexSchemas(t, db, func(tx *gorm.DB, a, _ string) {
			table := a + ".probe"
			model := &explicitTimePrecisions{}
			for i := 0; i < 2; i++ {
				if err := tx.Table(table).AutoMigrate(model); err != nil {
					t.Fatal(err)
				}
				defaultPrecision := int64(6)
				if mode.name == "mysql" {
					defaultPrecision = 3
				}
				for name, want := range map[string]int64{"zero_at": 0, "default_at": defaultPrecision, "milli_at": 3} {
					var got int64
					if err := tx.Raw("SELECT datetime_precision FROM information_schema.columns WHERE table_schema=? AND table_name='probe' AND column_name=?", a, name).Row().Scan(&got); err != nil || got != want {
						t.Fatalf("%s precision=%d want=%d error=%v", name, got, want, err)
					}
				}
			}
			at := time.Date(2025, 1, 2, 3, 4, 5, 123456000, time.UTC)
			if err := tx.Table(table).Create(&explicitTimePrecisions{ZeroAt: at, DefaultAt: at, MilliAt: at}).Error; err != nil {
				t.Fatal(err)
			}
			var got explicitTimePrecisions
			if err := tx.Table(table).Take(&got).Error; err != nil {
				t.Fatal(err)
			}
			wantDefault := at
			if mode.name == "mysql" {
				wantDefault = at.Truncate(time.Millisecond)
			}
			if !got.ZeroAt.Equal(at.Truncate(time.Second)) || !got.MilliAt.Equal(at.Truncate(time.Millisecond)) || !got.DefaultAt.Equal(wantDefault) {
				t.Fatalf("time precision round trip: %#v", got)
			}
		})
	})
}

// GORM v1.31.1's typed Select(...).Row/Rows does not infer a table from T.
// Verify the explicit-table API supported by that release without changing
// GORM's runtime core or treating an unsupported query's error as a pass.
func TestGenericsRowsWithExplicitTable(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		withIndexSchemas(t, db, func(tx *gorm.DB, a, _ string) {
			table := a + ".probe"
			if err := tx.Exec("CREATE TABLE ? (id integer, name varchar(64))", clause.Table{Name: table}).Error; err != nil {
				t.Fatal(err)
			}
			if err := tx.Exec("INSERT INTO ? (id,name) VALUES (1,'first'),(2,'second')", clause.Table{Name: table}).Error; err != nil {
				t.Fatal(err)
			}
			type record struct {
				ID   int
				Name string
			}
			var name string
			row := gorm.G[record](tx).Table(table).Select("name").Where("id = ?", 1).Row(context.Background())
			if row == nil {
				t.Fatal("explicit-table generic Row returned nil")
			}
			if err := row.Scan(&name); err != nil || name != "first" {
				t.Fatalf("generic Row name=%q want=first error=%v", name, err)
			}
			rows, err := gorm.G[record](tx).Table(table).Select("id", "name").Order("id").Rows(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []record
			for rows.Next() {
				var value record
				if err := rows.Scan(&value.ID, &value.Name); err != nil {
					t.Fatal(err)
				}
				got = append(got, value)
			}
			if err := rows.Err(); err != nil || !reflect.DeepEqual(got, []record{{1, "first"}, {2, "second"}}) {
				t.Fatalf("generic Rows=%#v error=%v", got, err)
			}
		})
	})
}
