// Command transaction-management demonstrates requirements 6.2.6.
// Transactions really commit/rollback; a separate pinned connection observes them.
package main

import (
	"context"
	"database/sql"
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
	kingbase "kingbase.com/gorm-driver-kingbase"
)

var errBusiness = errors.New("business validation failed")

type Entry struct {
	ID   uint   `gorm:"primaryKey"`
	Code string `gorm:"size:48;unique;not null"`
}

func main() {
	name := flag.String("case", "transaction-ok", "transaction-ok, transaction-error or checks")
	mode := flag.String("mode", "", "expected actual mode: mysql or oracle")
	flag.Parse()
	cases := map[string]func(*gorm.DB) error{
		"transaction-ok": transactionOK, "transaction-error": transactionError,
		"checks": transactionChecks,
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

func transactionOK(db *gorm.DB) error {
	return withFixture(db, func(writer, reader *gorm.DB, table string, readerID int64) error {
		// screenshot:transaction-ok:start
		err := writer.Transaction(func(tx *gorm.DB) error {
			if err := separateBackend(tx, readerID); err != nil {
				return err
			}
			if err := tx.Table(table).Create(&Entry{Code: "A"}).Error; err != nil {
				return err
			}
			if err := tx.SavePoint("doc_626").Error; err != nil {
				return err
			}
			if err := tx.Table(table).Create(&Entry{Code: "B"}).Error; err != nil {
				return err
			}
			return tx.RollbackTo("doc_626").Error
		})
		if err != nil {
			return err
		}
		// screenshot:transaction-ok:end
		if err := verifyCodes(reader, table, []string{"A"}); err != nil {
			return err
		}
		fmt.Println("callback_commit=ok savepoint_rollback=ok")
		fmt.Println("independent_reader=true committed_rows=[A] A_count=1 B_count=0")
		return nil
	})
}

func transactionError(db *gorm.DB) error {
	return withFixture(db, func(writer, reader *gorm.DB, table string, readerID int64) error {
		// screenshot:transaction-error:start
		err := writer.Transaction(func(tx *gorm.DB) error {
			if err := separateBackend(tx, readerID); err != nil {
				return err
			}
			if err := tx.Table(table).Create(&Entry{Code: "rejected"}).Error; err != nil {
				return err
			}
			return errBusiness
		})
		if !errors.Is(err, errBusiness) {
			return fmt.Errorf("expected original business error, got %v", err)
		}
		// screenshot:transaction-error:end
		if checkErr := verifyCodes(reader, table, []string{}); checkErr != nil {
			return checkErr
		}
		fmt.Println("callback_error_is_business_error=true")
		fmt.Println("independent_reader=true committed_rows=[] rejected_count=0")
		return err // Preserve the failed business operation (exit 1).
	})
}

func transactionChecks(db *gorm.DB) error {
	return withFixture(db, func(writer, reader *gorm.DB, table string, readerID int64) error {
		pool, err := writer.DB()
		if err != nil {
			return err
		}
		injected, err := gorm.Open(kingbase.New(kingbase.Config{Conn: pool}),
			&gorm.Config{Logger: logger.Discard})
		if err != nil {
			return err
		}
		injected = injected.WithContext(writer.Statement.Context)
		if err := manualWrite(injected, table, "manual-commit", readerID, true); err != nil {
			return err
		}
		if err := manualWrite(writer, table, "manual-rollback", readerID, false); err != nil {
			return err
		}
		if err := verifyCodes(reader, table, []string{"manual-commit"}); err != nil {
			return err
		}
		fmt.Println("manual_commit_visible=true manual_rollback_absent=true")
		fmt.Println("external_pool_transactions=true")
		if err := nestedRollback(writer, table); err != nil {
			return err
		}
		if err := verifyCodes(reader, table, []string{"manual-commit", "outer"}); err != nil {
			return err
		}
		fmt.Println("nested_outer_committed=true nested_inner_absent=true")
		if err := rejectEmptySavepoints(writer); err != nil {
			return err
		}
		fmt.Println("empty_savepoint_rejected=true empty_rollback_to_rejected=true")
		if err := checkCanceledContext(writer, reader, table); err != nil {
			return err
		}
		fmt.Println("canceled_begin_rejected=true canceled_write_rejected=true")
		fmt.Println("pool_usable_after_cancel=true canceled_row_absent=true")
		return nil
	})
}

func manualWrite(db *gorm.DB, table, code string, readerID int64, commit bool) (err error) {
	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	finished := false
	defer func() {
		if !finished {
			cleanup := tx.Rollback().Error
			if errors.Is(cleanup, sql.ErrTxDone) {
				cleanup = nil
			}
			err = errors.Join(err, cleanup)
		}
	}()
	if err := separateBackend(tx, readerID); err != nil {
		return err
	}
	if err := tx.Table(table).Create(&Entry{Code: code}).Error; err != nil {
		return err
	}
	if commit {
		err = tx.Commit().Error
	} else {
		err = tx.Rollback().Error
	}
	finished = err == nil
	return err
}

func nestedRollback(db *gorm.DB, table string) error {
	return db.Transaction(func(outer *gorm.DB) error {
		if err := outer.Table(table).Create(&Entry{Code: "outer"}).Error; err != nil {
			return err
		}
		err := outer.Transaction(func(inner *gorm.DB) error {
			if err := inner.Table(table).Create(&Entry{Code: "inner"}).Error; err != nil {
				return err
			}
			return errBusiness
		})
		if !errors.Is(err, errBusiness) {
			return fmt.Errorf("unexpected nested transaction result: %v", err)
		}
		return nil // Inner error is handled; explicitly allow the outer commit.
	})
}

func rejectEmptySavepoints(db *gorm.DB) (err error) {
	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer func() { err = errors.Join(err, tx.Rollback().Error) }()
	// Use fresh sessions so an expected GORM error does not pollute the next check.
	for _, check := range []func(*gorm.DB) *gorm.DB{
		func(conn *gorm.DB) *gorm.DB { return conn.SavePoint(" \t") },
		func(conn *gorm.DB) *gorm.DB { return conn.RollbackTo("") },
	} {
		result := check(tx.Session(&gorm.Session{NewDB: true}))
		if result.Error == nil || !strings.Contains(result.Error.Error(), "savepoint name must not be empty") {
			return fmt.Errorf("unexpected empty savepoint result: %v", result.Error)
		}
	}
	return nil
}

func checkCanceledContext(writer, reader *gorm.DB, table string) error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writer.WithContext(ctx).Begin().Error; !errors.Is(err, context.Canceled) {
		return fmt.Errorf("canceled Begin result: %v", err)
	}
	if err := writer.WithContext(ctx).Table(table).Create(&Entry{Code: "canceled"}).Error; !errors.Is(err, context.Canceled) {
		return fmt.Errorf("canceled Create result: %v", err)
	}
	pool, err := writer.DB()
	if err != nil {
		return err
	}
	health, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := pool.PingContext(health); err != nil {
		return err
	}
	return verifyCodes(reader, table, []string{"manual-commit", "outer"})
}

func separateBackend(tx *gorm.DB, readerID int64) error {
	var writerID int64
	if err := tx.Raw("SELECT pg_backend_pid()").Row().Scan(&writerID); err != nil {
		return err
	}
	if writerID == 0 || writerID == readerID {
		return errors.New("verification requires a separate reader connection")
	}
	return nil
}

func verifyCodes(reader *gorm.DB, table string, want []string) error {
	codes := make([]string, 0)
	if err := reader.Table(table).Order("code").Pluck("code", &codes).Error; err != nil {
		return err
	}
	if !reflect.DeepEqual(codes, want) {
		return fmt.Errorf("committed records=%v, want %v", codes, want)
	}
	return nil
}

func openDatabase(mode string) (*gorm.DB, func(), error) {
	dsn := os.Getenv("KINGBASE_DSN")
	if dsn == "" {
		return nil, nil, errors.New("KINGBASE_DSN is required; configure the target test instance first")
	}
	db, err := gorm.Open(kingbase.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, nil, err
	}
	pool, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	pool.SetMaxOpenConns(4)
	closePool := func() { _ = pool.Close() }
	actual := string(db.Dialector.(*kingbase.Dialector).DatabaseMode())
	if actual != mode {
		closePool()
		return nil, nil, fmt.Errorf("actual mode=%s, expected %s", actual, mode)
	}
	fmt.Printf("detected_mode=%s\n", actual)
	return db, closePool, nil
}

func withFixture(db *gorm.DB, body func(*gorm.DB, *gorm.DB, string, int64) error) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	writer := db.WithContext(ctx)
	namespace := fmt.Sprintf("gorm_doc626_%d", time.Now().UnixNano())
	if err := writer.Exec("CREATE SCHEMA ?", clause.Column{Name: namespace}).Error; err != nil {
		return err
	}
	// DDL and fixture cleanup are outside the transactions under examination.
	defer func() {
		fresh, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		cleanup := db.WithContext(fresh)
		dropErr := cleanup.Exec("DROP SCHEMA ? CASCADE", clause.Column{Name: namespace}).Error
		var count int64
		checkErr := cleanup.Raw(
			"SELECT count(*) FROM pg_catalog.pg_namespace WHERE nspname=?", namespace,
		).Row().Scan(&count)
		if checkErr == nil && count != 0 {
			checkErr = fmt.Errorf("fixture schema remains: count=%d", count)
		}
		if dropErr == nil && checkErr == nil {
			fmt.Println("fixture_cleanup_count=0")
		}
		err = errors.Join(err, dropErr, checkErr)
	}()
	table := namespace + ".transaction_entries"
	if err := writer.Table(table).Migrator().CreateTable(&Entry{}); err != nil {
		return err
	}
	return writer.Connection(func(reader *gorm.DB) error {
		var readerID int64
		if err := reader.Raw("SELECT pg_backend_pid()").Row().Scan(&readerID); err != nil {
			return err
		}
		return body(writer, reader, table, readerID)
	})
}
