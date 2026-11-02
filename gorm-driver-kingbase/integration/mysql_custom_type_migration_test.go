package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type mysqlCustomizedTypesProbe struct {
	Def string `gorm:"size:512;index:idx_kb_custom_def,unique"`
	Abc string `gorm:"size:65000000"`
}

func (mysqlCustomizedTypesProbe) TableName() string { return "gorm_kb_accept_custom_types" }

type uniqueConstraintTransitionPlain struct {
	Code string
}

func (uniqueConstraintTransitionPlain) TableName() string { return "gorm_kb_accept_unique_transition" }

type uniqueConstraintTransitionConstraint struct {
	Code string `gorm:"unique"`
}

func (uniqueConstraintTransitionConstraint) TableName() string {
	return "gorm_kb_accept_unique_transition"
}

type uniqueConstraintTransitionIndex struct {
	Code string `gorm:"uniqueIndex:idx_kb_unique_transition_code"`
}

func (uniqueConstraintTransitionIndex) TableName() string { return "gorm_kb_accept_unique_transition" }

type migrationTraceLogger struct {
	logger.Interface
	statements *[]string
}

func (l migrationTraceLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	sqlText, rowsAffected := fc()
	*l.statements = append(*l.statements, sqlText)
	l.Interface.Trace(ctx, begin, func() (string, int64) { return sqlText, rowsAffected }, err)
}

func TestMySQLCustomizedTypesNoUnneededAlter(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, mode acceptanceMode) {
		if mode.name != "mysql" {
			t.Skip("MySQL type compatibility case")
		}
		model := &mysqlCustomizedTypesProbe{}
		if err := db.Migrator().DropTable(model); err != nil {
			t.Fatalf("drop test table: %v", err)
		}
		t.Cleanup(func() { _ = db.Migrator().DropTable(model) })
		if err := db.Exec("CREATE TABLE `gorm_kb_accept_custom_types` (`def` varchar(512), `abc` longtext, UNIQUE INDEX `idx_kb_custom_def` (`def`))").Error; err != nil {
			t.Fatalf("create documented existing table: %v", err)
		}
		var statements []string
		session := db.Session(&gorm.Session{Logger: migrationTraceLogger{Interface: db.Logger, statements: &statements}})
		if err := session.AutoMigrate(model); err != nil {
			t.Fatalf("AutoMigrate existing compatible table: %v", err)
		}
		for _, statement := range statements {
			sql := strings.ToUpper(strings.TrimSpace(statement))
			for _, prefix := range []string{"ALTER TABLE", "ALTER INDEX", "CREATE INDEX", "CREATE UNIQUE INDEX", "DROP INDEX", "COMMENT ON INDEX"} {
				if strings.HasPrefix(sql, prefix) {
					t.Errorf("unneeded schema change: %s", statement)
				}
			}
		}
		indexes, err := db.Migrator().GetIndexes(model)
		if err != nil {
			t.Fatalf("GetIndexes: %v", err)
		}
		assertUniqueIndex(t, indexes, "idx_kb_custom_def", "def")
	})
}

func TestUniqueConstraintAndUniqueIndexMigration(t *testing.T) {
	runAcceptanceModes(t, &gorm.Config{TranslateError: true}, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		plain := &uniqueConstraintTransitionPlain{}
		constraint := &uniqueConstraintTransitionConstraint{}
		index := &uniqueConstraintTransitionIndex{}
		if err := db.Migrator().DropTable(plain); err != nil {
			t.Fatalf("drop test table: %v", err)
		}
		t.Cleanup(func() { _ = db.Migrator().DropTable(plain) })
		if err := db.AutoMigrate(plain); err != nil {
			t.Fatalf("migrate plain column: %v", err)
		}
		constraintName := db.NamingStrategy.UniqueName(plain.TableName(), "code")
		if err := db.AutoMigrate(constraint); err != nil {
			t.Fatalf("add unique constraint: %v", err)
		}
		if !db.Migrator().HasConstraint(constraint, constraintName) {
			t.Fatalf("unique constraint %q is absent", constraintName)
		}
		if err := db.Create(&uniqueConstraintTransitionPlain{Code: "constraint-duplicate"}).Error; err != nil {
			t.Fatalf("insert first constraint value: %v", err)
		}
		if err := db.Create(&uniqueConstraintTransitionPlain{Code: "constraint-duplicate"}).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
			t.Fatalf("duplicate constraint error = %v, want ErrDuplicatedKey", err)
		}
		if err := db.AutoMigrate(index); err != nil {
			t.Fatalf("migrate from unique constraint to unique index: %v", err)
		}
		if db.Migrator().HasConstraint(index, constraintName) {
			t.Fatalf("old unique constraint %q is still present", constraintName)
		}
		if !db.Migrator().HasIndex(index, "idx_kb_unique_transition_code") {
			t.Fatal("unique index is absent")
		}
		indexes, err := db.Migrator().GetIndexes(index)
		if err != nil {
			t.Fatalf("GetIndexes: %v", err)
		}
		assertUniqueIndex(t, indexes, "idx_kb_unique_transition_code", "code")
		if err := db.Create(&uniqueConstraintTransitionPlain{Code: "index-duplicate"}).Error; err != nil {
			t.Fatalf("insert first index value: %v", err)
		}
		if err := db.Create(&uniqueConstraintTransitionPlain{Code: "index-duplicate"}).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
			t.Fatalf("duplicate index error = %v, want ErrDuplicatedKey", err)
		}
		if err := db.AutoMigrate(index); err != nil {
			t.Fatalf("repeat unique index migration: %v", err)
		}
	})
}

func assertUniqueIndex(t *testing.T, indexes []gorm.Index, name, column string) {
	t.Helper()
	for _, index := range indexes {
		if index.Name() == name {
			unique, known := index.Unique()
			columns := index.Columns()
			if !known || !unique || len(columns) != 1 || columns[0] != column {
				t.Fatalf("index %q metadata: unique=(%t,%t) columns=%v", name, unique, known, columns)
			}
			return
		}
	}
	t.Fatalf("index %q not found in GetIndexes result", name)
}
