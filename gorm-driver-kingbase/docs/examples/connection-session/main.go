// Command connection-session supplies read-only examples for requirements 6.2.1.
// It never creates tables, changes database settings, or writes business data.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	kingbase "kingbase.com/gorm-driver-kingbase"
)

func main() {
	name := flag.String("case", "connection-ok", "connection-ok, connection-error, session-ok, or session-error")
	wantMode := flag.String("mode", "", "expected actual mode: mysql or oracle")
	flag.Parse()
	if *wantMode != "mysql" && *wantMode != "oracle" {
		fmt.Fprintln(os.Stderr, "ERROR: -mode must be mysql or oracle")
		os.Exit(2)
	}
	fmt.Printf("case=%s expected_mode=%s\n", *name, *wantMode)
	var err error
	switch *name {
	case "connection-ok":
		err = connectionOK(*wantMode)
	case "connection-error":
		err = connectionError()
	case "session-ok":
		err = sessionOK(*wantMode)
	case "session-error":
		err = sessionError(*wantMode)
	default:
		fmt.Fprintln(os.Stderr, "ERROR: unknown example case")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

// openAuto checks the real server mode before a read-only example is run.
func openAuto(wantMode string) (*gorm.DB, func(), error) {
	dsn := os.Getenv("KINGBASE_DSN")
	if dsn == "" {
		return nil, nil, errors.New("KINGBASE_DSN is required")
	}
	db, err := gorm.Open(kingbase.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	pool, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	closePool := func() { _ = pool.Close() }
	mode := db.Dialector.(*kingbase.Dialector).DatabaseMode()
	if string(mode) != wantMode {
		closePool()
		return nil, nil, fmt.Errorf("actual mode %s differs from expected %s", mode, wantMode)
	}
	fmt.Printf("detected_mode=%s\n", mode)
	return db, closePool, nil
}

func connectionOK(wantMode string) error {
	db, closePool, err := openAuto(wantMode)
	if err != nil {
		return err
	}
	defer closePool()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var value int
	if err := db.WithContext(ctx).Raw("SELECT ?", 1).Row().Scan(&value); err != nil {
		return err
	}
	if value != 1 {
		return fmt.Errorf("read value=%d, want 1", value)
	}
	fmt.Printf("read_value=%d\n", value)
	return nil
}

func connectionError() error {
	// Wrong configuration: invalid is not a supported compatibility mode.
	_, err := gorm.Open(kingbase.New(kingbase.Config{
		Mode: kingbase.Mode("invalid"),
	}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err == nil {
		return errors.New("invalid mode was unexpectedly accepted")
	}
	return err // The CLI exits with status 1: this configuration must fail.
}

func sessionOK(wantMode string) error {
	db, closePool, err := openAuto(wantMode)
	if err != nil {
		return err
	}
	defer closePool()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session := db.Session(&gorm.Session{PrepareStmt: true}).WithContext(ctx)
	manager, ok := session.ConnPool.(*gorm.PreparedStmtDB)
	if !ok {
		return errors.New("prepared statement manager is unavailable")
	}
	defer manager.Close()
	for i := 1; i <= 2; i++ {
		var value int
		if err := session.Raw("SELECT ?", 7).Row().Scan(&value); err != nil {
			return err
		}
		if value != 7 {
			return fmt.Errorf("read value=%d, want 7", value)
		}
		fmt.Printf("query_%d_value=%d\n", i, value)
	}
	count := len(manager.Stmts.Keys())
	if count == 0 {
		return errors.New("prepared statement cache is empty")
	}
	fmt.Printf("prepared_cache_entries=%d\n", count)
	return nil
}

func sessionError(wantMode string) error {
	db, closePool, err := openAuto(wantMode)
	if err != nil {
		return err
	}
	defer closePool()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Wrong usage: the context is canceled before the query starts.
	var value int
	queryErr := db.WithContext(ctx).Raw("SELECT ?", 7).Row().Scan(&value)
	if !errors.Is(queryErr, context.Canceled) {
		return fmt.Errorf("want context.Canceled, got %v", queryErr)
	}
	fmt.Printf("canceled_query_error=%v\n", queryErr)
	// A fresh operation checks the pool, not the canceled context's value.
	fresh, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := db.WithContext(fresh).Raw("SELECT ?", 9).Row().Scan(&value); err != nil {
		return fmt.Errorf("recovery query: %w", err)
	}
	if value != 9 {
		return fmt.Errorf("recovery value=%d, want 9", value)
	}
	fmt.Printf("recovery_query_value=%d\n", value)
	return queryErr // The example's canceled operation is still an error.
}
