package integration_test

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

type quotedIndexNameProbe struct {
	gorm.Model
	Name  string `gorm:"size:255;index:AS"`
	Code  string `gorm:"size:64;index:MiXeD"`
	Upper string `gorm:"size:64;index:UPPERCASE"`
}

func (quotedIndexNameProbe) TableName() string {
	return "gorm_kb_accept_quoted_index_name"
}

func requireQuotedIndexNames(t *testing.T, db *gorm.DB, model *quotedIndexNameProbe, names ...string) {
	t.Helper()
	want := map[string]bool{
		model.TableName() + "_pkey":                                  true,
		db.NamingStrategy.IndexName(model.TableName(), "deleted_at"): true,
	}
	for _, name := range names {
		want[name] = true
	}
	indexes, err := db.Migrator().GetIndexes(model)
	if err != nil || len(indexes) != len(want) {
		t.Fatalf("quoted index metadata: count=%d want=%d error=%v", len(indexes), len(want), err)
	}
	for _, index := range indexes {
		if !want[index.Name()] {
			t.Fatalf("quoted index metadata: unexpected or duplicate name %q", index.Name())
		}
		delete(want, index.Name())
	}
	if len(want) != 0 {
		t.Fatalf("quoted index metadata: missing names %v", want)
	}
}

func TestQuotedIndexName(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, mode acceptanceMode) {
		model := &quotedIndexNameProbe{}
		resetAcceptanceTables(t, db, model)

		asName, upperName := "AS", "UPPERCASE"
		if mode.name == "oracle" {
			asName, upperName = "as", "uppercase"
		}
		requireQuotedIndexNames(t, db, model, asName, "MiXeD", upperName)

		for _, name := range []string{"AS", "MiXeD", "UPPERCASE"} {
			hasIndex := db.Migrator().HasIndex(model, name)
			t.Logf("HasIndex(%s) = %t", name, hasIndex)
			if !hasIndex {
				t.Errorf("AutoMigrate completed, but HasIndex(%s) returned false", name)
			}
		}

		if err := db.AutoMigrate(model); err != nil {
			t.Fatalf("repeated AutoMigrate: %v", err)
		}
		requireQuotedIndexNames(t, db, model, asName, "MiXeD", upperName)
		if err := db.Migrator().DropIndex(model, "AS"); err != nil {
			t.Fatalf("DropIndex(AS): %v", err)
		}
		if db.Migrator().HasIndex(model, "AS") {
			t.Error("HasIndex(AS) returned true after DropIndex(AS)")
		}
		requireQuotedIndexNames(t, db, model, "MiXeD", upperName)

		if mode.name == "oracle" {
			// Oracle mode folds all-uppercase names, but "MiXeD" and "mixed"
			// remain separate identifiers. Check both sides of that rule.
			if db.Migrator().HasIndex(model, "mixed") {
				t.Error("HasIndex(mixed) matched MiXeD before mixed was created")
			}
			if !db.Migrator().HasIndex(model, "uppercase") {
				t.Error("HasIndex(uppercase) missed the folded UPPERCASE index")
			}
			if err := db.Exec(`CREATE INDEX "mixed" ON "gorm_kb_accept_quoted_index_name"("name")`).Error; err != nil {
				t.Fatalf("create separate lowercase mixed index: %v", err)
			}
			for _, name := range []string{"MiXeD", "mixed"} {
				if !db.Migrator().HasIndex(model, name) {
					t.Errorf("HasIndex(%s) returned false after both indexes were created", name)
				}
			}
			requireQuotedIndexNames(t, db, model, "MiXeD", "mixed", upperName)
			if err := db.Migrator().DropIndex(model, "MiXeD"); err != nil {
				t.Fatalf("DropIndex(MiXeD): %v", err)
			} else {
				if db.Migrator().HasIndex(model, "MiXeD") {
					t.Error("HasIndex(MiXeD) returned true after DropIndex(MiXeD)")
				}
				if !db.Migrator().HasIndex(model, "mixed") {
					t.Error("DropIndex(MiXeD) also removed the separate mixed index")
				}
			}
			requireQuotedIndexNames(t, db, model, "mixed", upperName)
		} else if mode.name == "mysql" {
			// MySQL mode delegates identifier comparison to the instance's
			// enable_ci setting. The current acceptance instance has it on, but
			// other installations may initialize it differently.
			var enableCI string
			if err := db.Raw("SHOW enable_ci").Scan(&enableCI).Error; err != nil {
				t.Fatalf("SHOW enable_ci: %v", err)
			}
			got := db.Migrator().HasIndex(model, "mixed")
			want := strings.EqualFold(enableCI, "on")
			if got != want {
				t.Errorf("HasIndex(mixed) = %t with enable_ci=%s; want %t", got, enableCI, want)
			}
		}
	})
}
