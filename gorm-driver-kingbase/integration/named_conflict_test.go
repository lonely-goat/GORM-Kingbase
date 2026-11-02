package integration_test

import (
	"errors"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type namedConflictProbe struct {
	ID      uint   `gorm:"primaryKey"`
	Code    string `gorm:"unique"`
	Alias   string `gorm:"unique"`
	Payload string
}

func (namedConflictProbe) TableName() string { return "gorm_kb_accept_named_conflict" }

func TestNamedConflictTargetsOnlyNamedConstraint(t *testing.T) {
	runAcceptanceModes(t, &gorm.Config{TranslateError: true}, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		resetAcceptanceTables(t, db, &namedConflictProbe{})
		first := namedConflictProbe{Code: "one", Alias: "a", Payload: "original"}
		second := namedConflictProbe{Code: "two", Alias: "b", Payload: "original"}
		if err := db.Create(&first).Error; err != nil {
			t.Fatalf("insert first row: %v", err)
		}
		if err := db.Create(&second).Error; err != nil {
			t.Fatalf("insert second row: %v", err)
		}
		constraintName := db.NamingStrategy.UniqueName(first.TableName(), "code")
		byConstraint := clause.OnConflict{
			OnConstraint: constraintName,
			DoUpdates:    clause.AssignmentColumns([]string{"payload"}),
		}
		if err := db.Clauses(byConstraint).Create(&namedConflictProbe{Code: "one", Alias: "c", Payload: "updated"}).Error; err != nil {
			t.Fatalf("update on named code constraint: %v", err)
		}
		if err := db.Clauses(byConstraint).Create(&namedConflictProbe{Code: "three", Alias: "b", Payload: "must not update"}).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
			t.Errorf("alias conflict error = %v, want ErrDuplicatedKey for a non-targeted unique constraint", err)
		}
		var gotFirst, gotSecond namedConflictProbe
		if err := db.First(&gotFirst, first.ID).Error; err != nil {
			t.Fatalf("read first row: %v", err)
		}
		if err := db.First(&gotSecond, second.ID).Error; err != nil {
			t.Fatalf("read second row: %v", err)
		}
		if gotFirst.Payload != "updated" || gotSecond.Payload != "original" {
			t.Errorf("wrong conflict target: first=%q second=%q", gotFirst.Payload, gotSecond.Payload)
		}
		byColumns := clause.OnConflict{
			Columns:   []clause.Column{{Name: "code"}},
			DoUpdates: clause.AssignmentColumns([]string{"payload"}),
		}
		if err := db.Clauses(byColumns).Create(&namedConflictProbe{Code: "four", Alias: "b", Payload: "must not update"}).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
			t.Errorf("alias conflict error = %v, want ErrDuplicatedKey for a non-targeted code column", err)
		}
		if err := db.First(&gotSecond, second.ID).Error; err != nil {
			t.Fatalf("read second row again: %v", err)
		}
		if gotSecond.Payload != "original" {
			t.Errorf("non-target row was updated: %q", gotSecond.Payload)
		}
		var count int64
		if err := db.Model(&namedConflictProbe{}).Count(&count).Error; err != nil || count != 2 {
			t.Errorf("rows after targeted conflicts = %d, want 2; err:%v", count, err)
		}
	})
}
