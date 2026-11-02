// Command data-results demonstrates requirements 6.2.4 on real test instances.
// Every case creates isolated objects, rolls them back and checks for residue.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"kingbase.com/gokb"
	kingbase "kingbase.com/gorm-driver-kingbase"
)

type Record struct {
	ID        uint   `gorm:"primaryKey"`
	Code      string `gorm:"size:48;unique;not null"`
	Name      string `gorm:"size:96;not null"`
	Score     int
	Active    bool
	Note      *string
	DeletedAt gorm.DeletedAt
}

func main() {
	name := flag.String("case", "data-ok", "data-ok, data-error, result-ok or result-error")
	mode := flag.String("mode", "", "expected actual mode: mysql or oracle")
	flag.Parse()
	cases := map[string]func(*gorm.DB) error{
		"data-ok": dataOK, "data-error": dataError,
		"result-ok": resultOK, "result-error": resultError,
	}
	run, known := cases[*name]
	if !known || (*mode != "mysql" && *mode != "oracle") {
		fmt.Fprintln(os.Stderr, "ERROR: specify a supported -case and -mode mysql|oracle")
		os.Exit(2)
	}
	fmt.Printf("case=%s expected_mode=%s\n", *name, *mode)
	db, closePool, err := openDatabase(*mode)
	if err == nil {
		err = run(db)
		closePool()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func dataOK(db *gorm.DB) error {
	return withRecords(db, func(tx *gorm.DB, table string) error {
		item := Record{Code: "crud", Name: "金仓数据", Score: 9, Active: true}
		if err := affected(tx.Table(table).Create(&item), 1); err != nil {
			return err
		}
		var read Record
		if err := tx.Table(table).First(&read, item.ID).Error; err != nil {
			return err
		}
		if item.ID == 0 || !reflect.DeepEqual(read, item) {
			return fmt.Errorf("created/read record mismatch: %+v", read)
		}
		result := tx.Table(table).Model(&Record{}).Where("id = ?", item.ID).
			Updates(map[string]interface{}{"score": 0, "active": false})
		if err := affected(result, 1); err != nil {
			return err
		}
		var updated Record
		if err := tx.Table(table).First(&updated, item.ID).Error; err != nil {
			return err
		}
		if updated.Score != 0 || updated.Active || updated.Code != item.Code || updated.Name != item.Name {
			return fmt.Errorf("zero-value update mismatch: %+v", updated)
		}
		fmt.Println("create_rows=1 query_match=true update_rows=1 score=0 active=false")
		if err := affected(tx.Table(table).Delete(&item), 1); err != nil {
			return err
		}
		if err := verifyDeletion(tx, table, item.ID, true); err != nil {
			return err
		}
		if err := affected(tx.Table(table).Unscoped().Delete(&item), 1); err != nil {
			return err
		}
		return verifyDeletion(tx, table, item.ID, false)
	})
}

func dataError(db *gorm.DB) error {
	return withRecords(db, func(tx *gorm.DB, table string) error {
		first := Record{Code: "duplicate", Name: "已有记录"}
		if err := tx.Table(table).Create(&first).Error; err != nil {
			return err
		}
		duplicate := Record{Code: first.Code, Name: "重复编码"}
		err := tx.Table(table).Create(&duplicate).Error
		if !errors.Is(err, gorm.ErrDuplicatedKey) {
			return fmt.Errorf("expected ErrDuplicatedKey, got %v", err)
		}
		var original *gokb.Error
		if errors.As(err, &original) || duplicate.ID != 0 {
			return errors.New("unexpected original error retention or failed-insert identity")
		}
		fmt.Println("translate_error=true error_is_ErrDuplicatedKey=true")
		fmt.Println("original_gokb_error_available=false failed_insert_id=0")
		return err // Keep this duplicate insert as a failed operation (exit 1).
	})
}

func resultOK(db *gorm.DB) error {
	return withRecords(db, func(tx *gorm.DB, table string) error {
		note := "非空备注"
		single := Record{Code: "single", Name: "单条映射", Score: 7, Active: true, Note: &note}
		if err := affected(tx.Table(table).Create(&single), 1); err != nil {
			return err
		}
		batch := []Record{{Code: "batch-a", Name: "批量甲", Score: 11},
			{Code: "batch-b", Name: "批量乙", Score: 22, Active: true, Note: &note},
			{Code: "batch-c", Name: "批量丙", Score: 33}}
		if err := affected(tx.Table(table).CreateInBatches(&batch, 2), 3); err != nil {
			return err
		}
		if err := verifyReturned(tx, table, append([]Record{single}, batch...)); err != nil {
			return err
		}
		var dto struct {
			RecordID uint `gorm:"column:record_id"`
			Label    string
			Note     *string
		}
		result := tx.Table(table).Model(&Record{}).Select("id AS record_id, name AS label, note").
			Where("id = ?", batch[0].ID).Scan(&dto)
		if err := affected(result, 1); err != nil {
			return err
		}
		if dto.RecordID != batch[0].ID || dto.Label != batch[0].Name || dto.Note != nil {
			return fmt.Errorf("projection mapping mismatch: %+v", dto)
		}
		fmt.Printf("projection: record_id=%d label=%s note_is_null=%t\n", dto.RecordID, dto.Label, dto.Note == nil)
		return nil
	})
}

func resultError(db *gorm.DB) error {
	return withRecords(db, func(tx *gorm.DB, table string) error {
		item := Record{Code: "scan-error", Name: "无法转换为整数"}
		if err := tx.Table(table).Create(&item).Error; err != nil {
			return err
		}
		var wrong struct {
			Name int // Deliberately incompatible with the stored text.
		}
		err := tx.Table(table).Model(&Record{}).Select("name").
			Where("id = ?", item.ID).Scan(&wrong).Error
		if err == nil || !strings.Contains(err.Error(), "Scan error") ||
			!strings.Contains(err.Error(), "converting driver.Value") {
			return fmt.Errorf("expected result conversion error, got %v", err)
		}
		var databaseError *gokb.Error
		if errors.As(err, &databaseError) {
			return fmt.Errorf("expected a client scan error, got database error: %w", err)
		}
		fmt.Println("scan_error=true destination=Name(int) original_gokb_error_available=false")
		return err // The destination must not be treated as a valid result.
	})
}

func affected(result *gorm.DB, want int64) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != want {
		return fmt.Errorf("RowsAffected=%d, want %d", result.RowsAffected, want)
	}
	return nil
}

func verifyDeletion(tx *gorm.DB, table string, id uint, soft bool) error {
	var visible Record
	if err := tx.Table(table).First(&visible, id).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("expected hidden/deleted row, got %v", err)
	}
	var stored Record
	err := tx.Table(table).Unscoped().First(&stored, id).Error
	if soft {
		if err != nil || !stored.DeletedAt.Valid || stored.ID != id {
			return fmt.Errorf("soft delete verification failed: record=%+v error=%v", stored, err)
		}
		fmt.Println("soft_delete_rows=1 default_query_not_found=true unscoped_visible=true")
	} else {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("hard delete verification failed: %v", err)
		}
		fmt.Println("hard_delete_rows=1 unscoped_query_not_found=true")
	}
	return nil
}

func verifyReturned(tx *gorm.DB, table string, items []Record) error {
	seen := make(map[uint]bool, len(items))
	for i, item := range items {
		if item.ID == 0 || seen[item.ID] {
			return fmt.Errorf("record[%d] has a zero or duplicate generated identity", i)
		}
		seen[item.ID] = true
		var stored Record
		if err := tx.Table(table).First(&stored, item.ID).Error; err != nil {
			return err
		}
		if !reflect.DeepEqual(stored, item) {
			return fmt.Errorf("record[%d] identity or field mapping mismatch", i)
		}
		fmt.Printf("record[%d]: id=%d code=%s fields_match=true note_is_null=%t\n", i, item.ID, item.Code, item.Note == nil)
	}
	fmt.Println("single_rows=1 batch_rows=3 all_ids_nonzero_unique=true")
	return nil
}

func openDatabase(mode string) (*gorm.DB, func(), error) {
	dsn := os.Getenv("KINGBASE_DSN")
	if dsn == "" {
		return nil, nil, errors.New("KINGBASE_DSN is required; configure the target test instance first")
	}
	db, err := gorm.Open(kingbase.Open(dsn), &gorm.Config{
		TranslateError: true, Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, nil, err
	}
	pool, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	closePool := func() { _ = pool.Close() }
	actual := string(db.Dialector.(*kingbase.Dialector).DatabaseMode())
	if actual != mode {
		closePool()
		return nil, nil, fmt.Errorf("actual mode=%s, expected %s", actual, mode)
	}
	fmt.Printf("detected_mode=%s\n", actual)
	return db, closePool, nil
}

func withRecords(db *gorm.DB, body func(*gorm.DB, string) error) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return tx.Error
	}
	namespace := fmt.Sprintf("gorm_doc_624_%d", time.Now().UnixNano())
	defer func() {
		rollbackErr := tx.Rollback().Error
		fresh, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var count int64
		cleanupErr := db.WithContext(fresh).Raw(
			"SELECT count(*) FROM pg_catalog.pg_namespace WHERE nspname=?", namespace,
		).Row().Scan(&count)
		if cleanupErr == nil && count != 0 {
			cleanupErr = fmt.Errorf("fixture namespace remains: count=%d", count)
		}
		if rollbackErr == nil && cleanupErr == nil {
			fmt.Println("fixture_cleanup_count=0")
		}
		err = errors.Join(err, rollbackErr, cleanupErr)
	}()
	if err := tx.Exec("CREATE SCHEMA ?", clause.Column{Name: namespace}).Error; err != nil {
		return err
	}
	table := namespace + ".data_records"
	if err := tx.Table(table).Migrator().CreateTable(&Record{}); err != nil {
		return err
	}
	return body(tx, table)
}
