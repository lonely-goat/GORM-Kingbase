// Command autodetect performs a read-only connectivity and mode-detection smoke test.
// It intentionally does not create, update, or delete any database object.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"gorm.io/gorm"
	kingbase "kingbase.com/gorm-driver-kingbase"
)

type databaseContext struct {
	DatabaseName string `gorm:"column:database_name"`
	SchemaName   string `gorm:"column:schema_name"`
	LoginUser    string `gorm:"column:login_user"`
}

func main() {
	expectedMode := flag.String("expect-mode", "", "require mysql or oracle; reject a mismatched instance")
	flag.Parse()
	if *expectedMode != "" && *expectedMode != string(kingbase.ModeMySQL) && *expectedMode != string(kingbase.ModeOracle) {
		log.Fatal("expect-mode must be mysql or oracle")
	}
	dsn := os.Getenv("KINGBASE_DSN")
	if dsn == "" {
		log.Fatal("KINGBASE_DSN is required")
	}

	// Open 默认创建 ModeAuto Dialector。Initialize 会先通过 gokb 建立 database/sql
	// 连接，再执行 SHOW database_mode，最后注册识别到的 MySQL/Oracle SQL 策略。
	db, err := gorm.Open(kingbase.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("open KingbaseES through the Kingbase Dialector: %v", err)
	}

	dialector, ok := db.Dialector.(*kingbase.Dialector)
	if !ok {
		log.Fatalf("unexpected Dialector type %T", db.Dialector)
	}
	pool, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if *expectedMode != "" && string(dialector.DatabaseMode()) != *expectedMode {
		log.Fatalf("database mode mismatch: requested %s, connected to %s", *expectedMode, dialector.DatabaseMode())
	}

	var context databaseContext
	if err := db.Raw(`
		SELECT CURRENT_DATABASE() AS database_name,
		       CURRENT_SCHEMA() AS schema_name,
		       CURRENT_USER AS login_user`,
	).Scan(&context).Error; err != nil {
		log.Fatalf("read database context: %v", err)
	}

	fmt.Printf("connection ok: mode=%s database=%s schema=%s user=%s\n",
		dialector.DatabaseMode(), context.DatabaseName, context.SchemaName, context.LoginUser)
}
