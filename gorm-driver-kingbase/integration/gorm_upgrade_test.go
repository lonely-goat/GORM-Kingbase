package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 这些用例覆盖 GORM v1.31.2 相对 v1.31.1 修复的行为，避免升级只验证编译。
type gorm1312Record struct {
	ID   uint `gorm:"primaryKey"`
	Name string
}

func (gorm1312Record) TableName() string { return "gorm_kb_gorm1312_records" }

func TestGORM1312GenericsModelInference(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		resetAcceptanceTables(t, db, &gorm1312Record{})
		records := []gorm1312Record{{Name: "first"}, {Name: "second"}}
		if err := db.Create(&records).Error; err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		// 特意不调用 Model/Table：新版应根据泛型参数推导表名。
		row := gorm.G[gorm1312Record](db).Select("name").Where("id = ?", records[0].ID).Row(ctx)
		if row == nil {
			t.Fatal("generic Row returned nil")
		}
		var name string
		if err := row.Scan(&name); err != nil || name != "first" {
			t.Fatalf("inferred-model Row: name=%q err=%v", name, err)
		}
		rows, err := gorm.G[gorm1312Record](db).Select("id", "name").Order("id").Rows(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []gorm1312Record
		for rows.Next() {
			var record gorm1312Record
			if err := rows.Scan(&record.ID, &record.Name); err != nil {
				t.Fatal(err)
			}
			got = append(got, record)
		}
		if err := rows.Err(); err != nil || !reflect.DeepEqual(got, records) {
			t.Fatalf("inferred-model Rows: got=%#v want=%#v err=%v", got, records, err)
		}
	})
}

func TestGORM1312WithResultErrorAndReuse(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		resetAcceptanceTables(t, db, &gorm1312Record{})
		record := gorm1312Record{Name: "present"}
		if err := db.Create(&record).Error; err != nil {
			t.Fatal(err)
		}
		ctx, result := context.Background(), gorm.WithResult()
		if _, err := gorm.G[gorm1312Record](db, result).Where("id = ?", record.ID).First(ctx); err != nil || result.RowsAffected != 1 || result.Error != nil {
			t.Fatalf("successful WithResult: rows=%d resultErr=%v err=%v", result.RowsAffected, result.Error, err)
		}
		if _, err := gorm.G[gorm1312Record](db, result).Where("id = ?", record.ID+1000).First(ctx); !errors.Is(err, gorm.ErrRecordNotFound) || !errors.Is(result.Error, gorm.ErrRecordNotFound) || result.RowsAffected != 0 {
			t.Fatalf("missing-row WithResult: rows=%d resultErr=%v err=%v", result.RowsAffected, result.Error, err)
		}
		// 同一个结果对象再次成功查询，旧错误和行数必须被正确重置。
		got, err := gorm.G[gorm1312Record](db, result).Where("id = ?", record.ID).Find(ctx)
		if err != nil || result.Error != nil || result.RowsAffected != 1 || len(got) != 1 || got[0] != record {
			t.Fatalf("reused WithResult: got=%#v rows=%d resultErr=%v err=%v", got, result.RowsAffected, result.Error, err)
		}
	})
}

func TestGORM1312NamedSliceParameters(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		resetAcceptanceTables(t, db, &gorm1312Record{})
		records := []gorm1312Record{{Name: "first"}, {Name: "second"}, {Name: "third"}}
		if err := db.Create(&records).Error; err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name string
			ids  interface{}
			want []gorm1312Record
		}{
			{"slice", []uint{records[0].ID, records[2].ID}, []gorm1312Record{records[0], records[2]}},
			{"array", [2]uint{records[0].ID, records[2].ID}, []gorm1312Record{records[0], records[2]}},
			{"empty", []uint{}, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, err := gorm.G[gorm1312Record](db).Where("id IN (@ids)", sql.Named("ids", tc.ids)).Order("id").Find(context.Background())
				if err != nil || len(got) != len(tc.want) || len(got) > 0 && !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("named parameter expansion: got=%#v want=%#v err=%v", got, tc.want, err)
				}
			})
		}
	})
}

type gorm1312MigrationV1 struct {
	ID    uint  `gorm:"primaryKey"`
	Value int64 `gorm:"type:integer;default:7"`
}

func (gorm1312MigrationV1) TableName() string { return "gorm_kb_gorm1312_migration" }

type gorm1312MigrationV2 struct {
	ID    uint  `gorm:"primaryKey"`
	Value int64 `gorm:"type:bigint;default:7"`
}

func (gorm1312MigrationV2) TableName() string { return "gorm_kb_gorm1312_migration" }

func TestGORM1312MigrateTypeWithMatchingDefault(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		resetAcceptanceTables(t, db, &gorm1312MigrationV1{})
		before := gorm1312MigrationV1{Value: 7}
		if err := db.Create(&before).Error; err != nil {
			t.Fatal(err)
		}
		checkType := func(want string) {
			t.Helper()
			var dataType string
			if err := db.Raw("SELECT data_type FROM information_schema.columns WHERE table_schema=CURRENT_SCHEMA() AND table_name=? AND column_name='value'", before.TableName()).Row().Scan(&dataType); err != nil || dataType != want {
				t.Fatalf("migrated column type=%q want=%q err=%v", dataType, want, err)
			}
		}
		checkType("integer")
		// 默认值仍为 7，但 integer → bigint 的类型迁移必须实际发生。
		if err := db.AutoMigrate(&gorm1312MigrationV2{}); err != nil {
			t.Fatal(err)
		}
		checkType("bigint")
		if err := db.AutoMigrate(&gorm1312MigrationV2{}); err != nil {
			t.Fatal(err)
		}
		var got gorm1312MigrationV2
		if err := db.First(&got, before.ID).Error; err != nil || got.Value != before.Value {
			t.Fatalf("migration preserves row: got=%#v err=%v", got, err)
		}
		var fromDefault gorm1312MigrationV2
		// 由数据库执行 DEFAULT，避免 GORM 按模型标签填值而掩盖默认值丢失。
		if err := db.Raw("INSERT INTO ? (value) VALUES (DEFAULT) RETURNING id,value", clause.Table{Name: before.TableName()}).Row().Scan(&fromDefault.ID, &fromDefault.Value); err != nil || fromDefault.Value != 7 {
			t.Fatalf("migration preserves default: got=%#v err=%v", fromDefault, err)
		}
	})
}
