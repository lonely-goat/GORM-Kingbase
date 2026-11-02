package integration_test

import (
	"database/sql"
	"testing"
	"time"

	"gorm.io/gorm"
)

type mysqlDefaultDateTimeProbe struct {
	ID        uint `gorm:"primaryKey"`
	UpdatedAt time.Time
}

func (mysqlDefaultDateTimeProbe) TableName() string {
	return "gorm_kb_accept_datetime_precision"
}

func TestMySQLDefaultDateTimePrecision(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, mode acceptanceMode) {
		if mode.name != "mysql" {
			t.Skip("MySQL mode default datetime precision")
		}
		model := &mysqlDefaultDateTimeProbe{}
		resetAcceptanceTables(t, db, model)

		var precision sql.NullInt64
		err := db.Raw(`
			SELECT datetime_precision
			FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = ? AND column_name = 'updated_at'`, model.TableName()).Row().Scan(&precision)
		if err != nil {
			t.Fatalf("read updated_at precision: %v", err)
		}
		if !precision.Valid || precision.Int64 != 3 {
			t.Fatalf("updated_at precision = %v, want 3 milliseconds", precision)
		}
	})
}
