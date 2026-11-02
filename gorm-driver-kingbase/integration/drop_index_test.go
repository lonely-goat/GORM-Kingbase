package integration_test

import (
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type dropIndexSQLProbe struct {
	Title string `gorm:"size:100;index:idx_kb_drop_sql_title"`
}

func (dropIndexSQLProbe) TableName() string { return "gorm_kb_accept_drop_index_sql" }

type dropIndexMigratorProbe struct {
	Title string `gorm:"size:100;index:idx_kb_drop_migrator_title"`
}

func (dropIndexMigratorProbe) TableName() string { return "gorm_kb_accept_drop_index_migrator" }

type indexLifecycleProbe struct {
	Title string `gorm:"size:100;index:idx_kb_lifecycle_title"`
}

func (indexLifecycleProbe) TableName() string { return "gorm_kb_accept_index_lifecycle" }

func countIndexProbe(t *testing.T, db *gorm.DB, table, index string) int64 {
	t.Helper()
	var count int64
	if err := db.Raw(`SELECT count(*) FROM pg_catalog.pg_indexes
WHERE schemaname = CURRENT_SCHEMA() AND tablename = ? AND indexname = ?`, table, index).Scan(&count).Error; err != nil {
		t.Fatalf("query index catalog: %v", err)
	}
	return count
}

// TestDropIndexSyntax separates the documented KingbaseES SQL form from the
// SQL emitted by this dialect's GORM Migrator. Each case owns a dedicated table.
func TestDropIndexSyntax(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		t.Run("documented_sql", func(t *testing.T) {
			model := &dropIndexSQLProbe{}
			const index = "idx_kb_drop_sql_title"
			resetAcceptanceTables(t, db, model)
			if got := countIndexProbe(t, db, model.TableName(), index); got != 1 {
				t.Fatalf("index catalog count before DROP INDEX = %d, want 1", got)
			}
			// clause.Column quotes the actual index name in both modes, so this
			// checks DROP INDEX <name> without identifier case ambiguity.
			if err := db.Exec("DROP INDEX ?", clause.Column{Name: index}).Error; err != nil {
				t.Fatalf("documented DROP INDEX <name> failed: %v", err)
			}
			if got := countIndexProbe(t, db, model.TableName(), index); got != 0 {
				t.Fatalf("index catalog count after documented DROP INDEX = %d, want 0", got)
			}
		})

		t.Run("gorm_migrator", func(t *testing.T) {
			model := &dropIndexMigratorProbe{}
			const index = "idx_kb_drop_migrator_title"
			resetAcceptanceTables(t, db, model)
			if got := countIndexProbe(t, db, model.TableName(), index); got != 1 {
				t.Fatalf("index catalog count before GORM DropIndex = %d, want 1", got)
			}
			if err := db.Migrator().DropIndex(model, index); err != nil {
				t.Fatalf("GORM Migrator.DropIndex failed: %v", err)
			}
			if got := countIndexProbe(t, db, model.TableName(), index); got != 0 {
				t.Fatalf("index catalog count after GORM Migrator.DropIndex = %d, want 0", got)
			}
		})
	})
}

// TestIndexLifecycle exercises the GORM API path that the upstream
// TestMigrateIndexes revealed as missing from the local acceptance suite.
func TestIndexLifecycle(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		model := &indexLifecycleProbe{}
		const oldName = "idx_kb_lifecycle_title"
		const newName = "idx_kb_lifecycle_title_new"
		resetAcceptanceTables(t, db, model)
		migrator := db.Migrator()
		if got := countIndexProbe(t, db, model.TableName(), oldName); got != 1 {
			t.Fatalf("index catalog count before RenameIndex = %d, want 1", got)
		}
		if err := migrator.RenameIndex(model, oldName, newName); err != nil {
			t.Fatalf("GORM Migrator.RenameIndex failed: %v", err)
		}
		if got := countIndexProbe(t, db, model.TableName(), oldName); got != 0 {
			t.Fatalf("old index catalog count after RenameIndex = %d, want 0", got)
		}
		if got := countIndexProbe(t, db, model.TableName(), newName); got != 1 {
			t.Fatalf("new index catalog count after RenameIndex = %d, want 1", got)
		}
		if err := migrator.DropIndex(model, newName); err != nil {
			t.Fatalf("GORM Migrator.DropIndex after rename failed: %v", err)
		}
		if got := countIndexProbe(t, db, model.TableName(), newName); got != 0 {
			t.Fatalf("index catalog count after DropIndex = %d, want 0", got)
		}
		if err := migrator.CreateIndex(model, "Title"); err != nil {
			t.Fatalf("GORM Migrator.CreateIndex failed: %v", err)
		}
		if got := countIndexProbe(t, db, model.TableName(), oldName); got != 1 {
			t.Fatalf("index catalog count after CreateIndex = %d, want 1", got)
		}
	})
}
