package integration_test

import (
	"fmt"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type emptyPrimaryKeyUser struct {
	ID   uint `gorm:"primaryKey"`
	Name string
	Pets []emptyPrimaryKeyPet
}

func (emptyPrimaryKeyUser) TableName() string { return "gorm_kb_accept_empty_pk_users" }

type emptyPrimaryKeyPet struct {
	EmptyPrimaryKeyUserID *uint  `gorm:"uniqueIndex:idx_kb_empty_pk_pet"`
	Name                  string `gorm:"size:256;uniqueIndex:idx_kb_empty_pk_pet"`
}

func (emptyPrimaryKeyPet) TableName() string { return "gorm_kb_accept_empty_pk_pets" }

func TestAssociationEmptyPrimaryKeyKingbase(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		resetAcceptanceTables(t, db, &emptyPrimaryKeyUser{}, &emptyPrimaryKeyPet{})
		id := uint(100)
		user := emptyPrimaryKeyUser{
			ID: id, Name: "owner",
			Pets: []emptyPrimaryKeyPet{
				{EmptyPrimaryKeyUserID: &id, Name: "bar"},
				{EmptyPrimaryKeyUserID: &id, Name: "foo"},
			},
		}
		if err := db.Session(&gorm.Session{FullSaveAssociations: true}).Create(&user).Error; err != nil {
			t.Fatalf("create owner and pets: %v", err)
		}
		var got emptyPrimaryKeyUser
		if err := db.Preload("Pets").First(&got, id).Error; err != nil {
			t.Fatalf("preload owner and pets: %v", err)
		}
		if got.ID != id || got.Name != user.Name || len(got.Pets) != 2 {
			t.Fatalf("preloaded owner = %#v, want id=%d and two pets", got, id)
		}
		seen := map[string]bool{}
		for _, pet := range got.Pets {
			if pet.EmptyPrimaryKeyUserID == nil || *pet.EmptyPrimaryKeyUserID != id {
				t.Fatalf("pet foreign key = %#v, want %d", pet.EmptyPrimaryKeyUserID, id)
			}
			seen[pet.Name] = true
		}
		if !seen["bar"] || !seen["foo"] {
			t.Fatalf("preloaded pets = %#v", got.Pets)
		}
	})
}

type mapReturningProbe struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"uniqueIndex"`
	Age  int
}

func (mapReturningProbe) TableName() string { return "gorm_kb_accept_map_returning" }

func TestCreateFromMapReturningIdentity(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		resetAcceptanceTables(t, db, &mapReturningProbe{})
		model := &mapReturningProbe{}
		values := []map[string]interface{}{
			{"name": "plain", "age": 1},
			{"name": "pointer", "age": 2},
		}
		valueRefs := append([]map[string]interface{}(nil), values...)
		if err := db.Model(model).Create(values[0]).Error; err != nil {
			t.Fatalf("create map: %v", err)
		}
		if err := db.Model(model).Create(&values[1]).Error; err != nil {
			t.Fatalf("create pointer to map: %v", err)
		}
		batch := []map[string]interface{}{
			{"name": "batch-one", "age": 3},
			{"name": "batch-two", "age": 4},
		}
		batchRefs := append([]map[string]interface{}(nil), batch...)
		if err := db.Model(model).Create(&batch).Error; err != nil {
			t.Fatalf("create map batch: %v", err)
		}
		if len(batch) != 2 {
			t.Fatalf("batch length = %d, want the two original maps", len(batch))
		}
		check := func(records, refs []map[string]interface{}, names []string, ages []int) {
			t.Helper()
			for i, record := range records {
				refs[i]["__alias_probe"] = i
				if record["__alias_probe"] != i {
					t.Fatalf("map[%d] no longer refers to its original input map", i)
				}
				delete(refs[i], "__alias_probe")
				if record["name"] != names[i] || fmt.Sprint(record["age"]) != fmt.Sprint(ages[i]) {
					t.Fatalf("map[%d] changed input fields: %#v, want name=%s age=%d", i, record, names[i], ages[i])
				}
				id, ok := record["id"]
				if !ok || fmt.Sprint(id) == "0" {
					t.Fatalf("map identity missing: %#v", record)
				}
				var stored mapReturningProbe
				if err := db.First(&stored, "name = ?", names[i]).Error; err != nil {
					t.Fatalf("read map-created row %v: %v", names[i], err)
				}
				if stored.Age != ages[i] || stored.Name != names[i] || fmt.Sprint(stored.ID) != fmt.Sprint(id) {
					t.Fatalf("map identity %v differs from stored %d", id, stored.ID)
				}
			}
		}
		check(values, valueRefs, []string{"plain", "pointer"}, []int{1, 2})
		check(batch, batchRefs, []string{"batch-one", "batch-two"}, []int{3, 4})
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil || count != 4 {
			t.Fatalf("map insert row count=%d err=%v", count, err)
		}

		// A conflict can return fewer rows than inputs. The adapter must not
		// assign the new row's identity to the skipped duplicate by position.
		conflicting := []map[string]interface{}{
			{"name": "batch-one", "age": 99},
			{"name": "batch-three", "age": 5},
		}
		result := db.Model(model).Clauses(clause.OnConflict{DoNothing: true}).Create(&conflicting)
		if result.Error != nil || result.RowsAffected != 1 {
			t.Fatalf("DoNothing map batch: rows=%d err=%v", result.RowsAffected, result.Error)
		}
		if id, ok := conflicting[0]["id"]; ok {
			t.Fatalf("skipped duplicate received identity %v", id)
		}
		var existing, inserted mapReturningProbe
		if err := db.First(&existing, "name = ?", "batch-one").Error; err != nil || existing.Age != 3 {
			t.Fatalf("duplicate row changed: %#v, err=%v", existing, err)
		}
		if err := db.First(&inserted, "name = ?", "batch-three").Error; err != nil || inserted.Age != 5 {
			t.Fatalf("new row absent: %#v, err=%v", inserted, err)
		}
		if err := db.Model(model).Count(&count).Error; err != nil || count != 5 {
			t.Fatalf("DoNothing map row count=%d err=%v", count, err)
		}
	})
}
