package integration_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
)

type compatibilityModel struct {
	ID        uint `gorm:"primaryKey"`
	Tiny      int8
	Small     int16
	Regular   int32
	Big       int64
	Unsigned  uint
	Enabled   bool    `gorm:"not null;default:false"`
	Amount    float64 `gorm:"precision:12;scale:4"`
	Code      string  `gorm:"size:64;uniqueIndex"`
	Name      string  `gorm:"size:255;not null"`
	Notes     string
	Payload   []byte
	SeenAt    time.Time `gorm:"precision:3"`
	Optional  *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (compatibilityModel) TableName() string { return "gorm_kingbase_type_matrix" }

func TestCompatibilityMatrix(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, mode acceptanceMode) {
		t.Logf("Dialector auto-detected database_mode=%s", mode.mode)

		// 测试只能连接专用数据库：这里会删除并重建固定的测试表。
		if err := db.Migrator().DropTable(&compatibilityModel{}); err != nil {
			t.Fatalf("drop old test table: %v", err)
		}
		t.Cleanup(func() { _ = db.Migrator().DropTable(&compatibilityModel{}) })
		if err := db.AutoMigrate(&compatibilityModel{}); err != nil {
			t.Fatalf("initial AutoMigrate: %v", err)
		}
		if err := db.AutoMigrate(&compatibilityModel{}); err != nil {
			t.Fatalf("repeated AutoMigrate: %v", err)
		}

		optional := "optional"
		want := compatibilityModel{
			Tiny: -8, Small: -1600, Regular: -320000, Big: -640000,
			Unsigned: 42, Enabled: true, Amount: 12345.625,
			Code: fmt.Sprintf("code-%d", time.Now().UnixNano()), Name: "Kingbase",
			Notes: "双模式字符串读写", Payload: []byte{0, 1, 2, 0xff},
			SeenAt: time.Now().UTC().Truncate(time.Millisecond), Optional: &optional,
		}
		if err := db.Create(&want).Error; err != nil {
			t.Fatalf("create type matrix: %v", err)
		}
		if want.ID == 0 {
			t.Fatal("identity was not returned to GORM")
		}
		t.Logf("Create returned identity id=%d", want.ID)

		var got compatibilityModel
		if err := db.First(&got, want.ID).Error; err != nil {
			t.Fatalf("read type matrix: %v", err)
		}
		if got.ID != want.ID || got.Tiny != want.Tiny || got.Small != want.Small ||
			got.Regular != want.Regular || got.Big != want.Big || got.Unsigned != want.Unsigned ||
			got.Enabled != want.Enabled || got.Amount != want.Amount || got.Code != want.Code ||
			got.Name != want.Name || got.Notes != want.Notes ||
			!bytes.Equal(got.Payload, want.Payload) || !got.SeenAt.Equal(want.SeenAt) ||
			got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Fatalf("round trip mismatch: got %#v, want %#v", got, want)
		}
		if got.Optional == nil || *got.Optional != optional {
			t.Fatalf("nullable string mismatch: %#v", got.Optional)
		}

		if err := db.Model(&got).Updates(map[string]interface{}{"name": "updated", "enabled": false}).Error; err != nil {
			t.Fatalf("update type matrix: %v", err)
		}
		var updated compatibilityModel
		if err := db.First(&updated, want.ID).Error; err != nil || updated.Name != "updated" || updated.Enabled {
			t.Fatalf("updated row = %#v, err: %v", updated, err)
		}
		if err := db.Delete(&got).Error; err != nil {
			t.Fatalf("delete type matrix: %v", err)
		}
		if err := db.First(&compatibilityModel{}, want.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("deleted row query error = %v", err)
		}
	})
}
