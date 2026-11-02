package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	kingbase "kingbase.com/gorm-driver-kingbase"
)

// These tests are the executable integration layer for the corresponding
// KB-DML/KB-TX/KB-DDL/KB-ADV cases. Every table name is reserved for this test
// package, and each subtest drops only those known tables. Never point the DSN
// variables at a production database or a shared production schema.

type acceptanceMode struct {
	name string
	mode kingbase.Mode
	env  string
}

var acceptanceModes = []acceptanceMode{
	{name: "mysql", mode: kingbase.ModeMySQL, env: "KINGBASE_MYSQL_DSN"},
	{name: "oracle", mode: kingbase.ModeOracle, env: "KINGBASE_ORACLE_DSN"},
}

func runAcceptanceModes(t *testing.T, config *gorm.Config, test func(*testing.T, *gorm.DB, acceptanceMode)) {
	t.Helper()
	missing := make([]string, 0, len(acceptanceModes))
	for _, mode := range acceptanceModes {
		if os.Getenv(mode.env) == "" {
			missing = append(missing, mode.env)
		}
	}
	if len(missing) > 0 && os.Getenv("KINGBASE_TEST_REQUIRE_DSN") == "yes" {
		t.Fatalf("real-database test requires both DSNs; missing %v", missing)
	}
	if len(missing) == len(acceptanceModes) {
		t.Skip("real-database test skipped: set KINGBASE_MYSQL_DSN and KINGBASE_ORACLE_DSN")
	}
	for _, mode := range acceptanceModes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			dsn := os.Getenv(mode.env)
			if dsn == "" {
				t.Skipf("set %s to run the KingbaseES %s integration test", mode.env, mode.name)
			}
			gormConfig := &gorm.Config{}
			if config != nil {
				copied := *config
				gormConfig = &copied
			}
			db, err := gorm.Open(kingbase.New(kingbase.Config{DSN: dsn, Mode: kingbase.ModeAuto}), gormConfig)
			if err != nil {
				t.Fatalf("open KingbaseES: %v", err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatalf("get database/sql pool: %v", err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			if got := db.Dialector.(*kingbase.Dialector).DatabaseMode(); got != mode.mode {
				t.Fatalf("database mode = %q, want %q", got, mode.mode)
			}
			test(t, db, mode)
		})
	}
}

func resetAcceptanceTables(t *testing.T, db *gorm.DB, models ...interface{}) {
	t.Helper()
	if err := db.Migrator().DropTable(models...); err != nil {
		t.Fatalf("drop previous acceptance tables: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Migrator().DropTable(models...); err != nil {
			t.Errorf("cleanup acceptance tables: %v", err)
		}
	})
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("AutoMigrate acceptance tables: %v", err)
	}
}

type acceptanceCRUD struct {
	ID        uint `gorm:"primaryKey"`
	Code      string
	Name      string
	Score     int
	Active    bool
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

func (acceptanceCRUD) TableName() string { return "gorm_kb_accept_crud" }

func TestP0CRUDReturningAndBatch(t *testing.T) {
	// KB-DML-001 through KB-DML-014 and KB-SQL-015/016.
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		resetAcceptanceTables(t, db, &acceptanceCRUD{})
		if err := db.AutoMigrate(&acceptanceCRUD{}); err != nil {
			t.Fatalf("repeated AutoMigrate: %v", err)
		}

		single := acceptanceCRUD{Code: "single", Name: "single", Score: 1, Active: true}
		if err := db.Create(&single).Error; err != nil || single.ID == 0 {
			t.Fatalf("single Create = id:%d err:%v", single.ID, err)
		}
		batch := []acceptanceCRUD{
			{Code: "batch-1", Name: "batch-1", Score: 2, Active: true},
			{Code: "batch-2", Name: "batch-2", Score: 3, Active: false},
			{Code: "batch-3", Name: "batch-3", Score: 4, Active: true},
		}
		expectedBatch := append([]acceptanceCRUD(nil), batch...)
		if err := db.CreateInBatches(&batch, 2).Error; err != nil {
			t.Fatalf("CreateInBatches: %v", err)
		}
		seenIDs := map[uint]bool{single.ID: true}
		for index, item := range batch {
			if item.ID == 0 || seenIDs[item.ID] {
				t.Fatalf("batch[%d] identity = %d; all identities must be non-zero and unique", index, item.ID)
			}
			seenIDs[item.ID] = true
			var stored acceptanceCRUD
			want := expectedBatch[index]
			if err := db.First(&stored, item.ID).Error; err != nil || stored.Code != want.Code || stored.Name != want.Name || stored.Score != want.Score || stored.Active != want.Active {
				t.Fatalf("batch[%d] stored=%#v want=%#v err=%v", index, stored, want, err)
			}
		}

		malicious := "x'; DROP TABLE gorm_kb_accept_crud; -- 中文"
		injected := acceptanceCRUD{Code: "parameter", Name: malicious}
		if err := db.Create(&injected).Error; err != nil {
			t.Fatalf("parameterized Create: %v", err)
		}
		var parameterizedRoundTrip acceptanceCRUD
		if err := db.Where("name = ?", malicious).First(&parameterizedRoundTrip).Error; err != nil || parameterizedRoundTrip.Name != malicious {
			t.Fatalf("parameterized round trip = %#v, err:%v", parameterizedRoundTrip, err)
		}

		var first, take, last acceptanceCRUD
		// First/Last 已按主键分别追加升序和降序。这里不能预先指定
		// Order("id")：显式升序会排在 Last 的主键降序之前，导致 Last
		// 仍返回首行，从而把 GORM 查询语义误判为方言缺陷。
		if err := db.First(&first).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Take(&take, single.ID).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Last(&last).Error; err != nil || first.ID != single.ID || take.ID != single.ID || last.ID != injected.ID {
			t.Fatalf("First/Take/Last = %d/%d/%d, want %d/%d/%d, err:%v", first.ID, take.ID, last.ID, single.ID, single.ID, injected.ID, err)
		}

		if err := db.Model(&single).Updates(map[string]interface{}{"score": 0, "active": false}).Error; err != nil {
			t.Fatalf("Updates zero values: %v", err)
		}
		// 每次按主键查询都使用新的目标对象。GORM 会把目标对象中已有的非零
		// 主键也加入 WHERE；复用上一条记录会形成互相冲突的双重主键条件。
		var updated acceptanceCRUD
		if err := db.First(&updated, single.ID).Error; err != nil || updated.Score != 0 || updated.Active {
			t.Fatalf("updated row = %#v, err:%v", updated, err)
		}
		single.Name = "saved"
		if err := db.Save(&single).Error; err != nil {
			t.Fatalf("Save existing row: %v", err)
		}
		var saved acceptanceCRUD
		if err := db.First(&saved, single.ID).Error; err != nil || saved.Name != "saved" {
			t.Fatalf("saved row = %#v, err:%v", saved, err)
		}

		var firstOrCreate acceptanceCRUD
		result := db.Where(acceptanceCRUD{Code: "first-or-create"}).
			Attrs(acceptanceCRUD{Name: "attrs", Score: 9}).FirstOrCreate(&firstOrCreate)
		if result.Error != nil || result.RowsAffected != 1 || firstOrCreate.ID == 0 ||
			firstOrCreate.Code != "first-or-create" || firstOrCreate.Name != "attrs" || firstOrCreate.Score != 9 {
			t.Fatalf("FirstOrCreate = %#v rows:%d err:%v", firstOrCreate, result.RowsAffected, result.Error)
		}

		var returned []acceptanceCRUD
		result = db.Model(&returned).Clauses(clause.Returning{}).
			Where("id = ?", single.ID).Update("score", 77)
		if result.Error != nil || result.RowsAffected != 1 || len(returned) != 1 || returned[0].ID != single.ID || returned[0].Score != 77 {
			t.Fatalf("Update RETURNING = %#v rows:%d err:%v", returned, result.RowsAffected, result.Error)
		}

		if err := db.Delete(&single).Error; err != nil {
			t.Fatalf("soft Delete: %v", err)
		}
		if err := db.First(&acceptanceCRUD{}, single.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("soft-deleted row query error = %v", err)
		}
		var softDeleted acceptanceCRUD
		if err := db.Unscoped().First(&softDeleted, single.ID).Error; err != nil {
			t.Fatalf("Unscoped query: %v", err)
		}
		if err := db.Unscoped().Delete(&single).Error; err != nil {
			t.Fatalf("hard Delete: %v", err)
		}
		if err := db.Unscoped().First(&acceptanceCRUD{}, single.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("hard-deleted row query error = %v", err)
		}
		if err := db.Delete(&acceptanceCRUD{}).Error; !errors.Is(err, gorm.ErrMissingWhereClause) {
			t.Fatalf("global delete error = %v, want ErrMissingWhereClause", err)
		}
	})
}

func TestP0TransactionsSavepointsContextAndPreparedStatements(t *testing.T) {
	// KB-TX-006 through KB-TX-013.
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		resetAcceptanceTables(t, db, &acceptanceCRUD{})
		if err := db.Transaction(func(tx *gorm.DB) error {
			return tx.Create(&acceptanceCRUD{Code: "commit", Name: "commit"}).Error
		}); err != nil {
			t.Fatalf("commit transaction: %v", err)
		}
		sentinel := errors.New("rollback sentinel")
		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&acceptanceCRUD{Code: "rollback", Name: "rollback"}).Error; err != nil {
				return err
			}
			return sentinel
		}); !errors.Is(err, sentinel) {
			t.Fatalf("rollback transaction error = %v", err)
		}

		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&acceptanceCRUD{Code: "outer", Name: "outer"}).Error; err != nil {
				return err
			}
			innerErr := tx.Transaction(func(inner *gorm.DB) error {
				if err := inner.Create(&acceptanceCRUD{Code: "inner", Name: "inner"}).Error; err != nil {
					return err
				}
				return sentinel
			})
			if !errors.Is(innerErr, sentinel) {
				return fmt.Errorf("nested rollback error: %w", innerErr)
			}
			return nil
		}); err != nil {
			t.Fatalf("nested transaction: %v", err)
		}

		tx := db.Begin()
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		if err := tx.Create(&acceptanceCRUD{Code: "save-a", Name: "save-a"}).Error; err != nil {
			_ = tx.Rollback().Error
			t.Fatal(err)
		}
		if err := tx.SavePoint("acceptance_sp").Error; err != nil {
			_ = tx.Rollback().Error
			t.Fatal(err)
		}
		if err := tx.Create(&acceptanceCRUD{Code: "save-b", Name: "save-b"}).Error; err != nil {
			_ = tx.Rollback().Error
			t.Fatal(err)
		}
		if err := tx.RollbackTo("acceptance_sp").Error; err != nil {
			_ = tx.Rollback().Error
			t.Fatal(err)
		}
		if err := tx.Commit().Error; err != nil {
			t.Fatal(err)
		}

		for code, want := range map[string]int64{"commit": 1, "rollback": 0, "outer": 1, "inner": 0, "save-a": 1, "save-b": 0} {
			var count int64
			if err := db.Model(&acceptanceCRUD{}).Where("code = ?", code).Count(&count).Error; err != nil || count != want {
				t.Errorf("count(%s) = %d, want %d, err:%v", code, count, want, err)
			}
		}

		prepared := db.Session(&gorm.Session{PrepareStmt: true})
		if err := prepared.Create(&acceptanceCRUD{Code: "prepared", Name: "prepared"}).Error; err != nil {
			t.Fatalf("prepared Create: %v", err)
		}
		var preparedRow acceptanceCRUD
		if err := prepared.Where("code = ?", "prepared").First(&preparedRow).Error; err != nil {
			t.Fatalf("prepared query: %v", err)
		}
		manager, ok := prepared.ConnPool.(*gorm.PreparedStmtDB)
		if !ok {
			t.Fatalf("PrepareStmt pool type=%T, want *gorm.PreparedStmtDB", prepared.ConnPool)
		}
		if len(manager.Stmts.Keys()) < 2 {
			t.Fatalf("prepared statement cache has %d entries, want insert and select", len(manager.Stmts.Keys()))
		}
		if preparedRow.Code != "prepared" || preparedRow.ID == 0 {
			t.Fatalf("prepared query returned wrong row: %#v", preparedRow)
		}
		manager.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := db.WithContext(ctx).Find(&[]acceptanceCRUD{}).Error; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled context query error=%v, want context.Canceled", err)
		}
		sqlDB, _ := db.DB()
		if err := sqlDB.Ping(); err != nil {
			t.Fatalf("connection unusable after canceled context: %v", err)
		}
	})
}

type acceptanceConstraint struct {
	ID    uint   `gorm:"primaryKey"`
	Code  string `gorm:"size:64;uniqueIndex"`
	Score int    `gorm:"check:chk_kb_accept_score,score >= 0"`
}

func (acceptanceConstraint) TableName() string { return "gorm_kb_accept_constraints" }

type acceptanceParent struct {
	ID   uint `gorm:"primaryKey"`
	Name string
}

func (acceptanceParent) TableName() string { return "gorm_kb_accept_parents" }

type acceptanceChild struct {
	ID       uint `gorm:"primaryKey"`
	ParentID uint
	Parent   acceptanceParent `gorm:"constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
}

func (acceptanceChild) TableName() string { return "gorm_kb_accept_children" }

func TestP0ConstraintsErrorsAndUpsert(t *testing.T) {
	// KB-SQL-011 through KB-SQL-014, KB-DDL-012/013/014 and KB-TX-001/002/003.
	runAcceptanceModes(t, &gorm.Config{TranslateError: true}, func(t *testing.T, db *gorm.DB, mode acceptanceMode) {
		resetAcceptanceTables(t, db, &acceptanceConstraint{}, &acceptanceParent{}, &acceptanceChild{})
		first := acceptanceConstraint{Code: "unique", Score: 1}
		if err := db.Create(&first).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&acceptanceConstraint{Code: first.Code, Score: 2}).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
			t.Fatalf("duplicate error = %v", err)
		}
		if err := db.Create(&acceptanceConstraint{Code: "bad-check", Score: -1}).Error; !errors.Is(err, gorm.ErrCheckConstraintViolated) {
			t.Fatalf("check error = %v", err)
		}
		if err := db.Create(&acceptanceChild{ParentID: 999999}).Error; !errors.Is(err, gorm.ErrForeignKeyViolated) {
			t.Fatalf("foreign-key error = %v", err)
		}

		upsert := acceptanceConstraint{Code: first.Code, Score: 9}
		// 冲突由 Code 的唯一索引触发，必须显式指定冲突列。未指定时 GORM
		// 默认使用主键 ID，无法处理“新 ID、重复 Code”的 Upsert 场景。
		codeConflict := []clause.Column{{Name: "code"}}
		if err := db.Clauses(clause.OnConflict{Columns: codeConflict, UpdateAll: true}).Create(&upsert).Error; err != nil {
			t.Fatalf("%s UpdateAll upsert: %v", mode.name, err)
		}
		var stored acceptanceConstraint
		if err := db.Where("code = ?", first.Code).First(&stored).Error; err != nil || stored.Score != 9 {
			t.Fatalf("%s upsert result = %#v, err:%v", mode.name, stored, err)
		}
		if err := db.Clauses(clause.OnConflict{Columns: codeConflict, DoNothing: true}).Create(&acceptanceConstraint{Code: first.Code, Score: 10}).Error; err != nil {
			t.Fatalf("%s DoNothing upsert: %v", mode.name, err)
		}
		if err := db.Where("code = ?", first.Code).First(&stored).Error; err != nil || stored.Score != 9 {
			t.Fatalf("%s DoNothing changed value: %#v, err:%v", mode.name, stored, err)
		}
	})
}

func TestDoNothingBatchReturningKeepsNewIdentity(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, mode acceptanceMode) {
		resetAcceptanceTables(t, db, &acceptanceConstraint{})
		old := []acceptanceConstraint{{Code: "old-1", Score: 1}, {Code: "old-2", Score: 2}}
		if err := db.Create(&old).Error; err != nil {
			t.Fatalf("%s create existing rows: %v", mode.name, err)
		}

		// Association saves mix existing and new objects in one INSERT. A duplicate
		// row must not consume a RETURNING result intended for the new object's ID.
		batch := []acceptanceConstraint{
			{ID: old[1].ID, Code: old[1].Code, Score: old[1].Score},
			{ID: old[0].ID, Code: old[0].Code, Score: old[0].Score},
			{Code: "new", Score: 3},
		}
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&batch).Error; err != nil {
			t.Fatalf("%s mixed DoNothing batch: %v", mode.name, err)
		}
		var stored acceptanceConstraint
		if err := db.Where("code = ?", "new").First(&stored).Error; err != nil {
			t.Fatalf("%s read new row: %v", mode.name, err)
		}
		if batch[2].ID == 0 || batch[2].ID != stored.ID || batch[2].ID == old[0].ID || batch[2].ID == old[1].ID {
			t.Fatalf("%s new row ID = %d; stored = %d; old = %d,%d", mode.name, batch[2].ID, stored.ID, old[0].ID, old[1].ID)
		}
	})
}

type acceptanceUser struct {
	ID   uint `gorm:"primaryKey"`
	Name string
	// 测试模型类型名是 acceptanceUser，GORM 约定会默认寻找
	// AcceptanceUserID；显式声明 UserID 才能准确验证 HasOne/HasMany。
	Profile   acceptanceProfile    `gorm:"foreignKey:UserID;references:ID"`
	Pets      []acceptancePet      `gorm:"foreignKey:UserID;references:ID"`
	Languages []acceptanceLanguage `gorm:"many2many:gorm_kb_accept_user_languages;"`
}

func (acceptanceUser) TableName() string { return "gorm_kb_accept_users" }

type acceptanceProfile struct {
	ID     uint `gorm:"primaryKey"`
	UserID uint `gorm:"uniqueIndex"`
	Bio    string
}

func (acceptanceProfile) TableName() string { return "gorm_kb_accept_profiles" }

type acceptancePet struct {
	ID     uint `gorm:"primaryKey"`
	UserID uint
	Name   string
}

func (acceptancePet) TableName() string { return "gorm_kb_accept_pets" }

type acceptanceLanguage struct {
	Code string `gorm:"size:32;primaryKey"`
	Name string
}

func (acceptanceLanguage) TableName() string { return "gorm_kb_accept_languages" }

func TestP1AssociationsPreloadAndGenerics(t *testing.T) {
	// KB-ADV-001 through KB-ADV-010.
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		// Many2Many 中间表不是下面模型切片中的独立模型，必须显式清理；
		// 泛型 API 使用的 acceptanceCRUD 也纳入统一清理，保证测试无残留。
		if err := db.Migrator().DropTable("gorm_kb_accept_user_languages"); err != nil {
			t.Fatalf("drop previous many-to-many join table: %v", err)
		}
		t.Cleanup(func() {
			if err := db.Migrator().DropTable("gorm_kb_accept_user_languages"); err != nil {
				t.Logf("cleanup many-to-many join table: %v", err)
			}
		})
		resetAcceptanceTables(t, db,
			&acceptanceUser{}, &acceptanceProfile{}, &acceptancePet{}, &acceptanceLanguage{}, &acceptanceCRUD{},
		)
		user := acceptanceUser{
			Name:    "association-user",
			Profile: acceptanceProfile{Bio: "bio"},
			Pets:    []acceptancePet{{Name: "pet-1"}, {Name: "pet-2"}},
			Languages: []acceptanceLanguage{
				{Code: "zh-CN", Name: "Chinese"}, {Code: "en-US", Name: "English"},
			},
		}
		if err := db.Create(&user).Error; err != nil || user.ID == 0 {
			t.Fatalf("association Create = id:%d err:%v", user.ID, err)
		}
		var loaded acceptanceUser
		if err := db.Preload("Profile").Preload("Pets").Preload("Languages").First(&loaded, user.ID).Error; err != nil {
			t.Fatalf("Preload associations: %v", err)
		}
		if loaded.ID != user.ID || loaded.Name != "association-user" || loaded.Profile.ID != user.Profile.ID || loaded.Profile.UserID != user.ID || loaded.Profile.Bio != "bio" || len(loaded.Pets) != 2 || len(loaded.Languages) != 2 {
			t.Fatalf("preloaded graph = %#v", loaded)
		}
		wantPets := map[uint]string{user.Pets[0].ID: "pet-1", user.Pets[1].ID: "pet-2"}
		if len(wantPets) != 2 || wantPets[0] != "" {
			t.Fatalf("invalid returned pet identities: %#v", user.Pets)
		}
		for _, pet := range loaded.Pets {
			if name, ok := wantPets[pet.ID]; !ok || name != pet.Name || pet.UserID != user.ID {
				t.Fatalf("wrong preloaded pet: %#v", pet)
			}
			delete(wantPets, pet.ID)
		}
		if len(wantPets) != 0 {
			t.Fatalf("preload omitted pets: %v", wantPets)
		}
		wantLanguages := map[string]string{"zh-CN": "Chinese", "en-US": "English"}
		for _, language := range loaded.Languages {
			if name, ok := wantLanguages[language.Code]; !ok || name != language.Name {
				t.Fatalf("wrong preloaded language: %#v", language)
			}
			delete(wantLanguages, language.Code)
		}
		if len(wantLanguages) != 0 {
			t.Fatalf("preload omitted languages: %v", wantLanguages)
		}
		association := db.Model(&loaded).Association("Pets")
		if count := association.Count(); association.Error != nil || count != 2 {
			t.Fatalf("pet count=%d err=%v", count, association.Error)
		}
		newPet := acceptancePet{Name: "pet-3"}
		if err := db.Model(&loaded).Association("Pets").Append(&newPet); err != nil {
			t.Fatalf("append pet: %v", err)
		}
		var appended []acceptancePet
		if err := db.Where("user_id = ?", user.ID).Order("id").Find(&appended).Error; err != nil {
			t.Fatal(err)
		}
		if len(appended) != 3 || newPet.ID == 0 || newPet.UserID != user.ID {
			t.Fatalf("appended pet=%#v graph=%#v", newPet, appended)
		}
		wantPets = map[uint]string{user.Pets[0].ID: "pet-1", user.Pets[1].ID: "pet-2", newPet.ID: "pet-3"}
		for _, pet := range appended {
			if name, ok := wantPets[pet.ID]; !ok || name != pet.Name || pet.UserID != user.ID {
				t.Fatalf("wrong stored pet after append: %#v", pet)
			}
			delete(wantPets, pet.ID)
		}
		if len(wantPets) != 0 {
			t.Fatalf("append omitted pets: %v", wantPets)
		}

		ctx := context.Background()
		generic := acceptanceCRUD{Code: "generic", Name: "generic", Score: 1}
		if err := db.AutoMigrate(&acceptanceCRUD{}); err != nil {
			t.Fatal(err)
		}
		if err := gorm.G[acceptanceCRUD](db).Create(ctx, &generic); err != nil {
			t.Fatalf("generic Create: %v", err)
		}
		found, err := gorm.G[acceptanceCRUD](db).Where("id = ?", generic.ID).First(ctx)
		if err != nil || generic.ID == 0 || found.ID != generic.ID || found.Name != "generic" || found.Code != "generic" || found.Score != 1 {
			t.Fatalf("generic First = %#v, err:%v", found, err)
		}
		if rows, err := gorm.G[acceptanceCRUD](db).Where("id = ?", generic.ID).Update(ctx, "score", 2); err != nil || rows != 1 {
			t.Fatalf("generic Update rows:%d err:%v", rows, err)
		}
		updated, err := gorm.G[acceptanceCRUD](db).Where("id = ?", generic.ID).First(ctx)
		if err != nil || updated.ID != generic.ID || updated.Score != 2 {
			t.Fatalf("generic stored update=%#v err=%v", updated, err)
		}
		if rows, err := gorm.G[acceptanceCRUD](db).Where("id = ?", generic.ID).Delete(ctx); err != nil || rows != 1 {
			t.Fatalf("generic Delete rows:%d err:%v", rows, err)
		}
		if _, err := gorm.G[acceptanceCRUD](db).Where("id = ?", generic.ID).First(ctx); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("generic deleted row still visible: %v", err)
		}
		var deleted acceptanceCRUD
		if err := db.Unscoped().First(&deleted, generic.ID).Error; err != nil || !deleted.DeletedAt.Valid || deleted.Score != 2 {
			t.Fatalf("generic soft delete state=%#v err=%v", deleted, err)
		}
	})
}

type acceptanceMigrationV1 struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"size:64"`
}

func (acceptanceMigrationV1) TableName() string { return "gorm_kb_accept_migrations" }

type acceptanceMigrationV2 struct {
	ID    uint   `gorm:"primaryKey"`
	Name  string `gorm:"size:128"`
	Added string `gorm:"size:64;index;comment:migration column"`
}

func (acceptanceMigrationV2) TableName() string { return "gorm_kb_accept_migrations" }

func TestP0MigrationMetadataAndIdempotence(t *testing.T) {
	// Covers table/column/index metadata, repeat AutoMigrate and RenameColumn.
	// It does not exercise the CreateIndex/DropIndex/RenameIndex API lifecycle.
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		if err := db.Migrator().DropTable(&acceptanceMigrationV1{}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Migrator().DropTable(&acceptanceMigrationV1{}) })
		if err := db.AutoMigrate(&acceptanceMigrationV1{}); err != nil {
			t.Fatalf("migrate v1: %v", err)
		}
		if err := db.Create(&acceptanceMigrationV1{Name: "preserved"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&acceptanceMigrationV2{}); err != nil {
			t.Fatalf("migrate v2: %v", err)
		}
		if err := db.AutoMigrate(&acceptanceMigrationV2{}); err != nil {
			t.Fatalf("repeat migrate v2: %v", err)
		}
		migrator := db.Migrator()
		if !migrator.HasTable(&acceptanceMigrationV2{}) || !migrator.HasColumn(&acceptanceMigrationV2{}, "Added") ||
			!migrator.HasIndex(&acceptanceMigrationV2{}, "Added") {
			t.Fatal("migrated table, column or index was not discoverable")
		}
		columnTypes, err := migrator.ColumnTypes(&acceptanceMigrationV2{})
		if err != nil || len(columnTypes) != 3 {
			t.Fatalf("ColumnTypes length:%d err:%v", len(columnTypes), err)
		}
		columns := map[string]gorm.ColumnType{}
		for _, column := range columnTypes {
			columns[column.Name()] = column
		}
		for name, size := range map[string]int64{"name": 128, "added": 64} {
			column, ok := columns[name]
			if !ok {
				t.Fatalf("column %s missing", name)
			}
			if length, known := column.Length(); !known || length != size {
				t.Fatalf("%s length=(%d,%t), want %d", name, length, known, size)
			}
		}
		if comment, known := columns["added"].Comment(); !known || comment != "migration column" {
			t.Fatalf("added comment=(%q,%t)", comment, known)
		}
		if id, ok := columns["id"]; !ok {
			t.Fatal("id column missing")
		} else {
			if primary, known := id.PrimaryKey(); !known || !primary {
				t.Fatalf("id primary=(%t,%t)", primary, known)
			}
			if auto, known := id.AutoIncrement(); !known || !auto {
				t.Fatalf("id auto=(%t,%t)", auto, known)
			}
		}
		indexes, err := migrator.GetIndexes(&acceptanceMigrationV2{})
		if err != nil || len(indexes) == 0 {
			t.Fatalf("GetIndexes length:%d err:%v", len(indexes), err)
		}
		indexName := db.NamingStrategy.IndexName((acceptanceMigrationV2{}).TableName(), "added")
		foundIndex := false
		for _, index := range indexes {
			if index.Name() == indexName {
				unique, known := index.Unique()
				if !known || unique || !reflect.DeepEqual(index.Columns(), []string{"added"}) {
					t.Fatalf("added index unique=(%t,%t) columns=%v", unique, known, index.Columns())
				}
				foundIndex = true
			}
		}
		if !foundIndex {
			t.Fatalf("index %s missing from GetIndexes", indexName)
		}
		kind, err := migrator.TableType(&acceptanceMigrationV2{})
		if err != nil {
			t.Fatalf("TableType: %v", err)
		}
		var currentSchema string
		if err := db.Raw("SELECT CURRENT_SCHEMA()").Row().Scan(&currentSchema); err != nil {
			t.Fatal(err)
		}
		if kind == nil || kind.Schema() != currentSchema || kind.Name() != (acceptanceMigrationV2{}).TableName() || kind.Type() != "BASE TABLE" {
			t.Fatalf("wrong TableType: %#v", kind)
		}
		var preserved acceptanceMigrationV2
		if err := db.Where("name = ?", "preserved").First(&preserved).Error; err != nil {
			t.Fatalf("existing data was not preserved: %v", err)
		}
		wide := acceptanceMigrationV2{Name: strings.Repeat("x", 128), Added: "wide"}
		if err := db.Create(&wide).Error; err != nil {
			t.Fatalf("write expanded column: %v", err)
		}
		var wideStored acceptanceMigrationV2
		if err := db.First(&wideStored, wide.ID).Error; err != nil || wideStored.Name != strings.Repeat("x", 128) {
			t.Fatalf("expanded value=%#v err=%v", wideStored, err)
		}
		if err := migrator.RenameColumn(&acceptanceMigrationV2{}, "Added", "renamed_added"); err != nil {
			t.Fatalf("RenameColumn: %v", err)
		}
		if !migrator.HasColumn(&acceptanceMigrationV2{}, "renamed_added") || migrator.HasColumn(&acceptanceMigrationV2{}, "Added") {
			t.Fatal("renamed column was not discoverable")
		}
		if err := migrator.RenameColumn(&acceptanceMigrationV2{}, "renamed_added", "added"); err != nil {
			t.Fatalf("restore renamed column: %v", err)
		}
		if !migrator.HasColumn(&acceptanceMigrationV2{}, "Added") || migrator.HasColumn(&acceptanceMigrationV2{}, "renamed_added") {
			t.Fatal("restored column names incorrect")
		}
		var afterRename acceptanceMigrationV2
		if err := db.First(&afterRename, wide.ID).Error; err != nil || afterRename.Added != "wide" {
			t.Fatalf("renamed column data=%#v err=%v", afterRename, err)
		}
	})
}
