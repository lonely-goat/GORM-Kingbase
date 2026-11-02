package integration_test

import (
	"reflect"
	"sort"
	"testing"

	"gorm.io/gorm"
)

// The upstream customized foreign-key tests use ID alone as the association
// reference while ID is only one component of each model's primary key. The
// database cannot enforce those references, but GORM can still use the join
// tables when physical foreign-key creation is disabled.
type nonUniqueReferenceBlog struct {
	ID         uint                    `gorm:"primaryKey;autoIncrement"`
	Locale     string                  `gorm:"primaryKey"`
	SharedTags []nonUniqueReferenceTag `gorm:"many2many:gorm_kb_accept_shared_tags;foreignKey:ID;references:ID"`
	LocaleTags []nonUniqueReferenceTag `gorm:"many2many:gorm_kb_accept_locale_tags;foreignKey:ID,Locale;references:ID"`
}

func (nonUniqueReferenceBlog) TableName() string { return "gorm_kb_accept_nonunique_blogs" }

type nonUniqueReferenceTag struct {
	ID     uint   `gorm:"primaryKey;autoIncrement"`
	Locale string `gorm:"primaryKey"`
	Value  string
}

func (nonUniqueReferenceTag) TableName() string { return "gorm_kb_accept_nonunique_tags" }

func TestCustomizedManyToManyWithoutPhysicalForeignKeys(t *testing.T) {
	runAcceptanceModes(t, &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true}, func(t *testing.T, db *gorm.DB, mode acceptanceMode) {
		models := []interface{}{
			"gorm_kb_accept_shared_tags", "gorm_kb_accept_locale_tags",
			&nonUniqueReferenceBlog{}, &nonUniqueReferenceTag{},
		}
		if err := db.Migrator().DropTable(models...); err != nil {
			t.Fatalf("%s drop previous tables: %v", mode.name, err)
		}
		t.Cleanup(func() { _ = db.Migrator().DropTable(models...) })
		if err := db.AutoMigrate(&nonUniqueReferenceBlog{}, &nonUniqueReferenceTag{}); err != nil {
			t.Fatalf("%s migrate without physical foreign keys: %v", mode.name, err)
		}

		zh := nonUniqueReferenceBlog{
			Locale:     "ZH",
			SharedTags: []nonUniqueReferenceTag{{Locale: "ZH", Value: "shared-1"}, {Locale: "ZH", Value: "shared-2"}},
			LocaleTags: []nonUniqueReferenceTag{{Locale: "ZH", Value: "locale-1"}, {Locale: "ZH", Value: "locale-2"}},
		}
		if err := db.Create(&zh).Error; err != nil {
			t.Fatalf("%s create ZH blog: %v", mode.name, err)
		}
		en := nonUniqueReferenceBlog{ID: zh.ID, Locale: "EN"}
		if err := db.Create(&en).Error; err != nil {
			t.Fatalf("%s create EN blog with the same ID: %v", mode.name, err)
		}
		knownIDs := map[string]uint{}
		for _, tag := range append(append([]nonUniqueReferenceTag{}, zh.SharedTags...), zh.LocaleTags...) {
			knownIDs[tag.Value] = tag.ID
		}
		checkMembers := func(blog *nonUniqueReferenceBlog, relation string, want ...string) {
			t.Helper()
			association := db.Model(blog).Association(relation)
			got := association.Count()
			if association.Error != nil || got != int64(len(want)) {
				t.Fatalf("%s %s/%s count = %d, want %d, err = %v", mode.name, blog.Locale, relation, got, len(want), association.Error)
			}
			var members []nonUniqueReferenceTag
			if err := db.Model(blog).Association(relation).Find(&members); err != nil {
				t.Fatal(err)
			}
			assertTagMembers(t, members, knownIDs, want...)
		}
		checkMembers(&zh, "SharedTags", "shared-1", "shared-2")
		checkMembers(&en, "SharedTags", "shared-1", "shared-2")
		checkMembers(&zh, "LocaleTags", "locale-1", "locale-2")
		checkMembers(&en, "LocaleTags")

		shared3 := nonUniqueReferenceTag{Locale: "ZH", Value: "shared-3"}
		if err := db.Model(&zh).Association("SharedTags").Append(&shared3); err != nil {
			t.Fatalf("%s append shared tag: %v", mode.name, err)
		}
		locale3 := nonUniqueReferenceTag{Locale: "ZH", Value: "locale-3"}
		if err := db.Model(&zh).Association("LocaleTags").Append(&locale3); err != nil {
			t.Fatalf("%s append ZH locale tag: %v", mode.name, err)
		}
		knownIDs["shared-3"], knownIDs["locale-3"] = shared3.ID, locale3.ID
		checkMembers(&zh, "SharedTags", "shared-1", "shared-2", "shared-3")
		checkMembers(&en, "SharedTags", "shared-1", "shared-2", "shared-3")
		checkMembers(&zh, "LocaleTags", "locale-1", "locale-2", "locale-3")
		checkMembers(&en, "LocaleTags")

		var loaded nonUniqueReferenceBlog
		if err := db.Preload("SharedTags").Preload("LocaleTags").First(&loaded, "id = ? AND locale = ?", zh.ID, zh.Locale).Error; err != nil {
			t.Fatalf("%s preload ZH blog: %v", mode.name, err)
		}
		if loaded.ID != zh.ID || loaded.Locale != "ZH" {
			t.Fatalf("wrong preloaded blog: %#v", loaded)
		}
		assertTagMembers(t, loaded.SharedTags, knownIDs, "shared-1", "shared-2", "shared-3")
		assertTagMembers(t, loaded.LocaleTags, knownIDs, "locale-1", "locale-2", "locale-3")

		shared5 := &nonUniqueReferenceTag{Locale: "ZH", Value: "shared-5"}
		shared6 := &nonUniqueReferenceTag{Locale: "ZH", Value: "shared-6"}
		if err := db.Model(&en).Association("SharedTags").Replace(shared5, shared6); err != nil {
			t.Fatalf("%s replace shared tags: %v", mode.name, err)
		}
		knownIDs["shared-5"], knownIDs["shared-6"] = shared5.ID, shared6.ID
		checkMembers(&zh, "SharedTags", "shared-5", "shared-6")
		if err := db.Model(&zh).Association("SharedTags").Delete(shared5); err != nil {
			t.Fatalf("%s delete shared tag: %v", mode.name, err)
		}
		checkMembers(&en, "SharedTags", "shared-6")
		if err := db.Model(&en).Association("SharedTags").Clear(); err != nil {
			t.Fatalf("%s clear shared tags: %v", mode.name, err)
		}
		checkMembers(&zh, "SharedTags")

		locale4 := nonUniqueReferenceTag{Locale: "ZH", Value: "locale-4"}
		if err := db.Model(&en).Association("LocaleTags").Append(&locale4); err != nil {
			t.Fatalf("%s append EN locale tag: %v", mode.name, err)
		}
		knownIDs["locale-4"] = locale4.ID
		checkMembers(&zh, "LocaleTags", "locale-1", "locale-2", "locale-3")
		checkMembers(&en, "LocaleTags", "locale-4")
		locale5 := &nonUniqueReferenceTag{Locale: "ZH", Value: "locale-5"}
		locale6 := &nonUniqueReferenceTag{Locale: "ZH", Value: "locale-6"}
		if err := db.Model(&zh).Association("LocaleTags").Replace(locale5, locale6); err != nil {
			t.Fatalf("%s replace ZH locale tags: %v", mode.name, err)
		}
		knownIDs["locale-5"], knownIDs["locale-6"] = locale5.ID, locale6.ID
		checkMembers(&zh, "LocaleTags", "locale-5", "locale-6")
		checkMembers(&en, "LocaleTags", "locale-4")
		if err := db.Model(&zh).Association("LocaleTags").Delete(locale5); err != nil {
			t.Fatalf("%s delete ZH locale tag: %v", mode.name, err)
		}
		checkMembers(&zh, "LocaleTags", "locale-6")
		if err := db.Model(&en).Association("LocaleTags").Clear(); err != nil {
			t.Fatalf("%s clear EN locale tags: %v", mode.name, err)
		}
		checkMembers(&en, "LocaleTags")
		checkMembers(&zh, "LocaleTags", "locale-6")
	})
}

func assertTagMembers(t *testing.T, members []nonUniqueReferenceTag, knownIDs map[string]uint, want ...string) {
	t.Helper()
	got := make([]string, 0, len(members))
	ids := map[uint]bool{}
	for _, member := range members {
		if member.ID == 0 || ids[member.ID] || member.Locale != "ZH" || knownIDs[member.Value] != member.ID {
			t.Fatalf("wrong tag identity or locale: %#v", member)
		}
		ids[member.ID] = true
		got = append(got, member.Value)
	}
	expected := append([]string{}, want...)
	sort.Strings(expected)
	sort.Strings(got)
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("tag values=%v want=%v", got, expected)
	}
}
