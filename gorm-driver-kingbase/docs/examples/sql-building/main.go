// Command sql-building demonstrates requirements 6.2.3 on a real test instance.
// Every case uses its own schema and rolls back all fixture changes.
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

type SQLRow struct {
	ID    int    `gorm:"primaryKey;autoIncrement:false"`
	Name  string `gorm:"size:96;not null"`
	Score int
}

func main() {
	name := flag.String("case", "sql-ok", "sql-ok or sql-error")
	mode := flag.String("mode", "", "expected actual mode: mysql or oracle")
	flag.Parse()
	if (*name != "sql-ok" && *name != "sql-error") || (*mode != "mysql" && *mode != "oracle") {
		fmt.Fprintln(os.Stderr, "ERROR: use -case sql-ok|sql-error -mode mysql|oracle")
		os.Exit(2)
	}
	fmt.Printf("case=%s expected_mode=%s\n", *name, *mode)
	db, closePool, err := openDatabase(*mode)
	if err == nil {
		defer closePool()
		if *name == "sql-ok" {
			err = sqlOK(db, *mode)
		} else {
			err = sqlError(db)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func sqlOK(db *gorm.DB, mode string) error {
	return withFixture(db, func(tx *gorm.DB, namespace string) error {
		table := namespace + ".gorm_doc_sql_rows"
		if err := tx.Table(table).Migrator().CreateTable(&SQLRow{}); err != nil {
			return err
		}
		literal := "O'Reilly; -- 中文"
		rows := []SQLRow{{1, literal, 10}, {2, literal, 20}, {3, literal, 30}, {4, "other", 40}}
		if err := tx.Table(table).Create(&rows).Error; err != nil {
			return err
		}
		var got []SQLRow
		plan := pageQuery(tx.Session(&gorm.Session{DryRun: true}), table, literal).Find(&got)
		if err := verifyPlan(plan, mode, literal); err != nil {
			return err
		}
		fmt.Printf("sql=%s\nvars=%#v\n", plan.Statement.SQL.String(), plan.Statement.Vars)
		result := pageQuery(tx, table, literal).Find(&got)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 || len(got) != 1 || got[0] != (SQLRow{2, literal, 20}) {
			return fmt.Errorf("unexpected page result: rows=%d data=%+v", result.RowsAffected, got)
		}
		fmt.Println("sql_and_vars_check=PASS")
		fmt.Printf("returned_rows=%d id=%d score=%d name=%q\n", len(got), got[0].ID, got[0].Score, got[0].Name)
		return nil
	})
}

func pageQuery(db *gorm.DB, table, name string) *gorm.DB {
	return db.Table(table).
		Where(clause.Eq{Column: clause.Column{Name: "name"}, Value: name}).
		Where(clause.Gte{Column: clause.Column{Name: "score"}, Value: 10}).
		Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}}).
		Limit(1).Offset(1)
}

func sqlError(db *gorm.DB) error {
	return withFixture(db, func(tx *gorm.DB, namespace string) error {
		table := namespace + ".gorm_doc_sql_rows"
		if err := tx.Table(table).Migrator().CreateTable(&SQLRow{}); err != nil {
			return err
		}
		// Deliberate SQL grammar error: the comparison has no right operand.
		query := func(conn *gorm.DB) *gorm.DB {
			return conn.Table(table).Where("score > AND score < ?", 30)
		}
		var rows []SQLRow
		plan := query(tx.Session(&gorm.Session{DryRun: true})).Find(&rows)
		if plan.Error != nil {
			return fmt.Errorf("unexpected build error: %w", plan.Error)
		}
		fmt.Printf("dry_run_build=ok\nsql=%s\n", plan.Statement.SQL.String())
		err := query(tx).Find(&rows).Error
		var databaseError *gokb.Error
		if !errors.As(err, &databaseError) || string(databaseError.Code) != "42601" {
			return fmt.Errorf("expected SQL syntax error 42601, got %v", err)
		}
		fmt.Printf("sqlstate=%s\n", databaseError.Code)
		return err // This invalid SQL must remain a failed operation (exit 1).
	})
}

func verifyPlan(plan *gorm.DB, mode, literal string) error {
	if plan.Error != nil {
		return plan.Error
	}
	sqlText := plan.Statement.SQL.String()
	wantVars := []interface{}{literal, 10, 1, 1}
	fragments := []string{"`name` = ?", "`score` >= ?", "ORDER BY `id`", "LIMIT ? OFFSET ?"}
	if mode == "oracle" {
		wantVars = []interface{}{literal, 10}
		fragments = []string{`"name" = :1`, `"score" >= :2`, `ORDER BY "id"`, "OFFSET 1 ROWS FETCH NEXT 1 ROW ONLY"}
	}
	if strings.Contains(sqlText, literal) || !reflect.DeepEqual(plan.Statement.Vars, wantVars) {
		return errors.New("SQL inlined data or bound parameters differ from the query")
	}
	for _, fragment := range fragments {
		if !strings.Contains(sqlText, fragment) {
			return fmt.Errorf("SQL lacks required mode-specific fragment %q", fragment)
		}
	}
	return nil
}

func openDatabase(mode string) (*gorm.DB, func(), error) {
	dsn := os.Getenv("KINGBASE_DSN")
	if dsn == "" {
		return nil, nil, errors.New("KINGBASE_DSN is required; configure the target test instance first")
	}
	db, err := gorm.Open(kingbase.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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

func withFixture(db *gorm.DB, body func(*gorm.DB, string) error) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return tx.Error
	}
	namespace := fmt.Sprintf("gorm_doc_623_%d", time.Now().UnixNano())
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
	return body(tx, namespace)
}
