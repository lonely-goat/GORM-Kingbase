package integration_test

import (
	"testing"

	"gorm.io/gorm"
)

type referenceUniqueParent struct {
	ID  uint
	Ref uint `gorm:"uniqueIndex:idx_kb_accept_fk_parent_ref"`
}

func (referenceUniqueParent) TableName() string { return "gorm_kb_accept_fk_parent" }

type referenceUniqueChild struct {
	ID        uint
	ParentRef uint
	Parent    referenceUniqueParent `gorm:"foreignKey:ParentRef;references:Ref;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (referenceUniqueChild) TableName() string { return "gorm_kb_accept_fk_child" }

func TestReferenceUniqueIndexForeignKey(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		resetAcceptanceTables(t, db, &referenceUniqueParent{}, &referenceUniqueChild{})
		const indexName = "idx_kb_accept_fk_parent_ref"
		var uniqueConstraintCount int64
		if err := db.Raw(`SELECT count(*) FROM information_schema.table_constraints
WHERE constraint_schema = CURRENT_SCHEMA() AND table_name = ? AND constraint_name = ? AND constraint_type = 'UNIQUE'`,
			(referenceUniqueParent{}).TableName(), indexName,
		).Scan(&uniqueConstraintCount).Error; err != nil || uniqueConstraintCount != 1 {
			t.Fatalf("referenced unique constraint = %d, err = %v", uniqueConstraintCount, err)
		}

		parent := referenceUniqueParent{Ref: 101}
		if err := db.Create(&parent).Error; err != nil {
			t.Fatalf("create referenced row: %v", err)
		}
		child := referenceUniqueChild{ParentRef: parent.Ref}
		if err := db.Create(&child).Error; err != nil {
			t.Fatalf("create referencing row: %v", err)
		}
		if err := db.Model(&parent).Update("Ref", 102).Error; err != nil {
			t.Fatalf("update referenced key: %v", err)
		}
		var found referenceUniqueChild
		if err := db.First(&found, child.ID).Error; err != nil || found.ParentRef != 102 {
			t.Fatalf("cascaded foreign key = %d, err = %v", found.ParentRef, err)
		}

		if err := db.Migrator().DropTable(&referenceUniqueChild{}); err != nil {
			t.Fatalf("drop referencing table: %v", err)
		}
		const renamedIndex = "idx_kb_accept_fk_parent_ref_renamed"
		if err := db.Migrator().RenameIndex(&referenceUniqueParent{}, indexName, renamedIndex); err != nil {
			t.Fatalf("rename constraint-backed unique index: %v", err)
		}
		if !db.Migrator().HasIndex(&referenceUniqueParent{}, renamedIndex) {
			t.Fatal("renamed unique index was not found")
		}
		if err := db.Migrator().DropIndex(&referenceUniqueParent{}, renamedIndex); err != nil {
			t.Fatalf("drop constraint-backed unique index: %v", err)
		}
		if db.Migrator().HasIndex(&referenceUniqueParent{}, renamedIndex) {
			t.Fatal("unique index still exists after DropIndex")
		}
	})
}

func TestColumnTypesMissingTable(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		const table = "gorm_kb_accept_missing_column_types"
		if err := db.Migrator().DropTable(table); err != nil {
			t.Fatalf("drop probe table: %v", err)
		}
		if _, err := db.Migrator().ColumnTypes(table); err == nil {
			t.Fatal("ColumnTypes for a nonexistent table must return an error")
		}
	})
}
