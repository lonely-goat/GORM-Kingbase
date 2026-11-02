package integration_test

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type uppercaseSubmission struct {
	ID    uint   `gorm:"column:ID;primaryKey"`
	Name  string `gorm:"column:NAME;size:64;uniqueIndex:UPPER_SUBMISSION_NAME"`
	Mixed string `gorm:"column:MiXeD;size:64"`
	Lower string `gorm:"column:lower_name;size:64"`
	Score int    `gorm:"column:SCORE;check:UPPER_SUBMISSION_SCORE,score >= 0"`
}

func (uppercaseSubmission) TableName() string { return "GORM_KB_SUBMISSION_UPPER" }

func TestUppercaseModelReadAndReturning(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		withIndexSchemas(t, db, func(tx *gorm.DB, _, _ string) {
			if err := tx.AutoMigrate(&uppercaseSubmission{}); err != nil {
				t.Fatal(err)
			}
			items := []uppercaseSubmission{{Name: "one", Mixed: "mixed-one", Lower: "lower-one", Score: 1}, {Name: "two", Mixed: "mixed-two", Lower: "lower-two", Score: 2}}
			if err := tx.Create(&items).Error; err != nil {
				t.Fatalf("uppercase identity RETURNING: %v", err)
			}
			if items[0].ID == 0 || items[1].ID == 0 || items[0].ID == items[1].ID {
				t.Fatalf("identities: %+v", items)
			}
			var got []uppercaseSubmission
			if err := tx.Order(clause.OrderByColumn{Column: clause.Column{Name: "ID"}}).Find(&got).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, items) {
				t.Fatalf("uppercase roundtrip got=%+v want=%+v", got, items)
			}
			var selected uppercaseSubmission
			if err := tx.Select("ID", "NAME", "MiXeD", "lower_name", "SCORE").Take(&selected, items[0].ID).Error; err != nil || selected != items[0] {
				t.Fatalf("explicit Select=%+v err=%v", selected, err)
			}
			var scanned uppercaseSubmission
			if err := tx.Model(&uppercaseSubmission{}).Where("id = ?", items[0].ID).Scan(&scanned).Error; err != nil || scanned != items[0] {
				t.Fatalf("model Scan=%+v err=%v", scanned, err)
			}
			var dto struct {
				ID   uint
				Name string
			}
			if err := tx.Model(&uppercaseSubmission{}).Select("ID", "NAME").Where("id = ?", items[0].ID).Scan(&dto).Error; err != nil || dto.ID != items[0].ID || dto.Name != "one" {
				t.Fatalf("DTO=%+v err=%v", dto, err)
			}
			var mapped struct{ Alternate string }
			callerMapping := map[string]string{"name": "alternate", "NAME": "alternate"}
			if err := tx.Model(&uppercaseSubmission{}).Select("NAME").MapColumns(callerMapping).Where("id = ?", items[0].ID).Find(&mapped).Error; err != nil || mapped.Alternate != "one" {
				t.Fatalf("explicit MapColumns=%+v err=%v", mapped, err)
			}
			if !reflect.DeepEqual(callerMapping, map[string]string{"name": "alternate", "NAME": "alternate"}) {
				t.Fatalf("caller mapping mutated: %v", callerMapping)
			}
			var updated uppercaseSubmission
			r := tx.Model(&updated).Clauses(clause.Returning{}).Where("id = ?", items[0].ID).Updates(map[string]interface{}{"NAME": "updated", "SCORE": 3})
			if r.Error != nil || updated.ID != items[0].ID || updated.Name != "updated" || updated.Mixed != items[0].Mixed || updated.Score != 3 {
				t.Fatalf("update RETURNING=%+v err=%v", updated, r.Error)
			}
			var deleted uppercaseSubmission
			r = tx.Clauses(clause.Returning{}).Where("id = ?", items[1].ID).Delete(&deleted)
			if r.Error != nil || deleted != items[1] {
				t.Fatalf("delete RETURNING=%+v err=%v", deleted, r.Error)
			}
		})
		// Each worker pins its own connection. This checks shared GORM schema
		// and mapping safety without interleaving result streams on one gokb
		// transaction/connection (the SDK requires those to be consumed serially).
		var sessions []*gorm.DB
		var records []uppercaseSubmission
		// GORM v1.31.1 ParseIndexes mutates cached UniqueIndex metadata.
		// Complete migration setup serially; only the application reads below
		// run concurrently, on separate pinned database connections.
		for i := 0; i < 4; i++ {
			withIndexSchemas(t, db, func(tx *gorm.DB, _, _ string) {
				if err := tx.AutoMigrate(&uppercaseSubmission{}); err != nil {
					t.Error(err)
					return
				}
				item := uppercaseSubmission{Name: fmt.Sprintf("worker-%d", i), Mixed: "mixed", Lower: "lower", Score: i}
				if err := tx.Create(&item).Error; err != nil {
					t.Error(err)
					return
				}
				sessions = append(sessions, tx)
				records = append(records, item)
			})
		}
		var workers sync.WaitGroup
		for i, tx := range sessions {
			workers.Add(1)
			go func(tx *gorm.DB, item uppercaseSubmission) {
				defer workers.Done()
				var got uppercaseSubmission
				if err := tx.Take(&got, item.ID).Error; err != nil || got != item {
					t.Errorf("pooled uppercase query=%+v want=%+v err=%v", got, item, err)
				}
			}(tx, records[i])
		}
		workers.Wait()
	})
}

type uppercaseAssociation struct {
	ID       uint                `gorm:"column:ID;primaryKey"`
	PARENTID uint                `gorm:"column:PARENT_ID"`
	PARENT   uppercaseSubmission `gorm:"foreignKey:PARENTID;references:ID"`
}

func (uppercaseAssociation) TableName() string { return "GORM_KB_SUBMISSION_CHILD" }

func TestUppercaseAssociationReturningAndJoins(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		withIndexSchemas(t, db, func(tx *gorm.DB, _, _ string) {
			if err := tx.AutoMigrate(&uppercaseSubmission{}, &uppercaseAssociation{}); err != nil {
				t.Fatal(err)
			}
			item := uppercaseAssociation{PARENT: uppercaseSubmission{Name: "parent", Mixed: "mixed", Lower: "lower", Score: 1}}
			if err := tx.Create(&item).Error; err != nil || item.ID == 0 || item.PARENT.ID == 0 || item.PARENTID != item.PARENT.ID {
				t.Fatalf("uppercase association create=%+v err=%v", item, err)
			}
			var joined uppercaseAssociation
			if err := tx.Joins("PARENT").Take(&joined, item.ID).Error; err != nil || joined != item {
				t.Fatalf("uppercase JOIN=%+v want=%+v err=%v", joined, item, err)
			}
			var preloaded uppercaseAssociation
			if err := tx.Preload("PARENT").Take(&preloaded, item.ID).Error; err != nil || preloaded != item {
				t.Fatalf("uppercase preload=%+v want=%+v err=%v", preloaded, item, err)
			}
		})
	})
}

func TestUppercaseModelMigrationSchema(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		withIndexSchemas(t, db, func(tx *gorm.DB, a, _ string) {
			// Schema and table parts are both passed in uppercase, exercising
			// the same catalog folding as a model TableName/column tag.
			table := strings.ToUpper(a) + ".GORM_KB_SUBMISSION_UPPER"
			scoped := tx.Table(table)
			if err := scoped.AutoMigrate(&uppercaseSubmission{}); err != nil {
				t.Fatal(err)
			}
			m := scoped.Migrator()
			if !m.HasTable(&uppercaseSubmission{}) || !m.HasColumn(&uppercaseSubmission{}, "Name") || !m.HasColumn(&uppercaseSubmission{}, "Mixed") || !m.HasConstraint(&uppercaseSubmission{}, "UPPER_SUBMISSION_SCORE") || !m.HasIndex(&uppercaseSubmission{}, "UPPER_SUBMISSION_NAME") {
				t.Fatal("uppercase catalog presence is inconsistent with created objects")
			}
			columns, err := m.ColumnTypes(&uppercaseSubmission{})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{"ID": true, "NAME": true, "MiXeD": true, "lower_name": true, "SCORE": true}
			for _, column := range columns {
				if !want[column.Name()] {
					t.Fatalf("unexpected model column %q", column.Name())
				}
				delete(want, column.Name())
				if column.Name() == "ID" {
					auto, known := column.AutoIncrement()
					if !known || !auto {
						t.Fatalf("uppercase identity metadata=(%t,%t)", auto, known)
					}
				}
			}
			if len(want) != 0 {
				t.Fatalf("missing columns: %v", want)
			}
			var statements []string
			repeat := scoped.Session(&gorm.Session{Logger: migrationTraceLogger{Interface: db.Logger, statements: &statements}})
			if err := repeat.AutoMigrate(&uppercaseSubmission{}); err != nil {
				t.Fatalf("repeat uppercase AutoMigrate: %v", err)
			}
			for _, sql := range statements {
				upper := strings.ToUpper(strings.TrimSpace(sql))
				if strings.HasPrefix(upper, "CREATE ") || strings.HasPrefix(upper, "ALTER ") {
					t.Errorf("repeat migration changed schema: %s", sql)
				}
			}
			if err := m.DropIndex(&uppercaseSubmission{}, "UPPER_SUBMISSION_NAME"); err != nil {
				t.Fatal(err)
			}
			if m.HasIndex(&uppercaseSubmission{}, "UPPER_SUBMISSION_NAME") {
				t.Fatal("uppercase index remained after drop")
			}
		})
	})
}

type submissionPage struct {
	ID       uint   `gorm:"primaryKey"`
	Category string `gorm:"size:32"`
	Score    int
}

func (submissionPage) TableName() string { return "gorm_kb_submission_page" }

func TestGroupedAndDistinctPagination(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		withIndexSchemas(t, db, func(tx *gorm.DB, _, _ string) {
			if err := tx.AutoMigrate(&submissionPage{}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Create(&[]submissionPage{{Category: "a", Score: 1}, {Category: "a", Score: 2}, {Category: "b", Score: 3}, {Category: "c", Score: 4}}).Error; err != nil {
				t.Fatal(err)
			}
			var grouped []struct {
				Category string
				N        int64
			}
			if err := tx.Model(&submissionPage{}).Select("category, count(*) AS n").Group("category").Having("count(*) > ?", 1).Limit(1).Scan(&grouped).Error; err != nil || len(grouped) != 1 || grouped[0].Category != "a" || grouped[0].N != 2 {
				t.Fatalf("grouped page=%+v err=%v", grouped, err)
			}
			for _, explicit := range []bool{false, true} {
				q := tx.Model(&submissionPage{}).Distinct("category")
				if explicit {
					q = q.Order("category")
				}
				var got []string
				if err := q.Limit(1).Offset(1).Pluck("category", &got).Error; err != nil || len(got) != 1 || (explicit && got[0] != "b") {
					t.Fatalf("distinct offset explicit=%t got=%v err=%v", explicit, got, err)
				}
			}
			var count int64
			if err := tx.Model(&submissionPage{}).Select("count(*)").Limit(1).Scan(&count).Error; err != nil || count != 4 {
				t.Fatalf("aggregate=%d err=%v", count, err)
			}
			var projected []struct{ Category string }
			if err := tx.Model(&submissionPage{}).Select("category").Distinct().Limit(1).Scan(&projected).Error; err != nil || len(projected) != 1 {
				t.Fatalf("projected=%+v err=%v", projected, err)
			}
		})
	})
}

func TestMapSliceCreateReturningVariants(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		for _, variant := range []string{"value", "pointer", "batches-value", "batches-pointer"} {
			t.Run(variant, func(t *testing.T) {
				withIndexSchemas(t, db, func(tx *gorm.DB, _, _ string) {
					if err := tx.AutoMigrate(&submissionPage{}); err != nil {
						t.Fatal(err)
					}
					items := []map[string]interface{}{{"category": "a", "score": 1}, {"category": "b", "score": 2}, {"category": "c", "score": 3}}
					refs := append([]map[string]interface{}(nil), items...)
					var result *gorm.DB
					switch variant {
					case "value":
						result = tx.Model(&submissionPage{}).Create(items)
					case "pointer":
						result = tx.Model(&submissionPage{}).Create(&items)
					case "batches-value":
						result = tx.Model(&submissionPage{}).CreateInBatches(items, 2)
					case "batches-pointer":
						result = tx.Model(&submissionPage{}).CreateInBatches(&items, 2)
					}
					if result.Error != nil || result.RowsAffected != 3 || len(items) != 3 {
						t.Fatalf("map batch len=%d rows=%d err=%v", len(items), result.RowsAffected, result.Error)
					}
					for i, item := range items {
						refs[i]["alias_probe"] = i
						if item["alias_probe"] != i {
							t.Fatalf("map[%d] alias changed", i)
						}
						delete(refs[i], "alias_probe")
						var stored submissionPage
						if err := tx.Where("category = ?", string(rune('a'+i))).Take(&stored).Error; err != nil {
							t.Fatal(err)
						}
						if item["category"] != stored.Category || fmt.Sprint(item["score"]) != fmt.Sprint(stored.Score) || fmt.Sprint(item["id"]) != fmt.Sprint(stored.ID) || stored.ID == 0 {
							t.Fatalf("map[%d]=%+v stored=%+v", i, item, stored)
						}
					}
					var count int64
					if err := tx.Model(&submissionPage{}).Count(&count).Error; err != nil || count != 3 {
						t.Fatalf("map count=%d err=%v", count, err)
					}
				})
			})
		}
	})
}
