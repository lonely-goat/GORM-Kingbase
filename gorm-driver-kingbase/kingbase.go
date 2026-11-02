// Package kingbase provides an independent GORM v2 Dialector for KingbaseES.
// It uses gokb as the database/sql driver and supports KingbaseES MySQL and Oracle
// compatibility modes without modifying gorm.io/gorm.
package kingbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
	_ "kingbase.com/gokb" // 注册 database/sql 驱动名 "kingbase"。
)

const (
	DefaultDriverName          = "kingbase"
	defaultIdentifierMaxLength = 63
	defaultStringSize          = 4000
	defaultDetectionTimeout    = 5 * time.Second
)

// Config 配置 Kingbase Dialector。
type Config struct {
	DriverName string
	DSN        string
	Conn       gorm.ConnPool

	// Mode 默认为 ModeAuto。显式指定 ModeMySQL/ModeOracle 时不会执行模式探测，
	// 离线构建 SQL 时还需按调用方式配置自动 Ping、默认事务及连接池。
	Mode Mode

	// ModeDetectionTimeout 仅用于 ModeAuto 的 SHOW database_mode。
	// 小于等于零时采用 5 秒，避免初始化因网络故障无限等待。
	ModeDetectionTimeout time.Duration

	// DefaultStringSize 是 Oracle 模式字段没有 size 标签时的默认 VARCHAR2 长度。
	// MySQL 模式不使用此配置，字符串长度由字段及其标签决定。
	DefaultStringSize uint

	// WithoutReturning 仅供兼容性排障。正常情况下必须保持 false，方言会使用
	// INSERT ... RETURNING 结果集扫描来回填 identity，避免 LastInsertId 和 RETURNING INTO。
	WithoutReturning bool
}

// Dialector 实现 gorm.Dialector。resolvedMode 在 Initialize 中确定，之后只读。
type Dialector struct {
	Config

	// KingbaseES compatibility (KB-NFR-001/003): Initialize writes the detected
	// mode once per gorm.Open lifecycle while SQL builders can read it from
	// concurrent statements. Keep configuration initialization and mode reads
	// synchronized so -race runs do not report a Dialector state race.
	initializeMu sync.Mutex
	modeMu       sync.RWMutex
	resolvedMode Mode
}

// borrowedConnPool deliberately exposes only gorm.ConnPool while Initialize is
// in progress. GORM closes a concrete *sql.DB whenever Dialector.Initialize
// fails; hiding the concrete type prevents it from closing a caller-owned pool.
// The original pool is restored before a successful Initialize returns, so
// normal transaction and *sql.DB access semantics remain unchanged.
type borrowedConnPool struct {
	gorm.ConnPool
}

// Open 使用 gokb DSN 创建自动识别模式的 Dialector。
func Open(dsn string) gorm.Dialector {
	return New(Config{DSN: dsn, Mode: ModeAuto})
}

// New 使用完整配置创建 Dialector。
func New(config Config) gorm.Dialector {
	return &Dialector{Config: config}
}

func (d *Dialector) Name() string { return "kingbase" }

// DatabaseMode 返回初始化后冻结的实际模式。初始化前，显式模式会直接返回；
// ModeAuto 在初始化前仍返回 auto。
func (d *Dialector) DatabaseMode() Mode {
	d.modeMu.RLock()
	defer d.modeMu.RUnlock()
	if d.resolvedMode != "" {
		return d.resolvedMode
	}
	return normalizeMode(d.Mode)
}

func (d *Dialector) setConfiguredMode(mode Mode) {
	d.modeMu.Lock()
	d.Mode = mode
	d.modeMu.Unlock()
}

func (d *Dialector) setResolvedMode(mode Mode) {
	d.modeMu.Lock()
	d.resolvedMode = mode
	d.modeMu.Unlock()
}

// Apply 将 GORM 默认命名长度设为 63，保留显式配置；不额外校验任意对象名的字节数。
func (d *Dialector) Apply(config *gorm.Config) error {
	if config.NamingStrategy == nil {
		config.NamingStrategy = schema.NamingStrategy{IdentifierMaxLength: defaultIdentifierMaxLength}
		return nil
	}

	switch naming := config.NamingStrategy.(type) {
	case schema.NamingStrategy:
		if naming.IdentifierMaxLength <= 0 {
			naming.IdentifierMaxLength = defaultIdentifierMaxLength
			config.NamingStrategy = naming
		}
	case *schema.NamingStrategy:
		if naming.IdentifierMaxLength <= 0 {
			naming.IdentifierMaxLength = defaultIdentifierMaxLength
		}
	}
	return nil
}

func (d *Dialector) Initialize(db *gorm.DB) error {
	d.initializeMu.Lock()
	defer d.initializeMu.Unlock()

	if d.DriverName == "" {
		d.DriverName = DefaultDriverName
	}
	if d.DefaultStringSize == 0 {
		d.DefaultStringSize = defaultStringSize
	}
	if d.Mode == "" {
		d.setConfiguredMode(ModeAuto)
	}
	configuredMode := normalizeMode(d.Mode)
	d.setConfiguredMode(configuredMode)
	if !configuredMode.validConfiguredMode() {
		return fmt.Errorf("kingbase: invalid configured mode %q; use auto, mysql, or oracle", configuredMode)
	}

	openedByDialector := false
	if d.Conn != nil {
		// KingbaseES compatibility (KB-MODE-013): protect a caller-owned
		// *sql.DB from GORM's unconditional cleanup on Initialize errors.
		db.ConnPool = borrowedConnPool{ConnPool: d.Conn}
	} else {
		conn, err := sql.Open(d.DriverName, d.DSN)
		if err != nil {
			return fmt.Errorf("kingbase: open gokb connection: %w", err)
		}
		db.ConnPool = conn
		openedByDialector = true
	}

	closeOnError := func() {
		if openedByDialector {
			if conn, ok := db.ConnPool.(*sql.DB); ok {
				_ = conn.Close()
			}
		}
	}

	if configuredMode == ModeAuto {
		timeout := d.ModeDetectionTimeout
		if timeout <= 0 {
			timeout = defaultDetectionTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		var rawMode string
		if err := db.ConnPool.QueryRowContext(ctx, "SHOW database_mode").Scan(&rawMode); err != nil {
			closeOnError()
			return fmt.Errorf("kingbase: detect database mode with SHOW database_mode: %w", err)
		}
		mode, err := parseDatabaseMode(rawMode)
		if err != nil {
			closeOnError()
			return err
		}
		d.setResolvedMode(mode)
	} else {
		d.setResolvedMode(configuredMode)
	}

	// 新方言直接生成 RETURNING 并按结果集扫描。继续开启 gokb 的 LastInsertId
	// 选项会让驱动再次追加 RETURNING *，得到重复子句，因此在可检查的 DSN 上尽早报错。
	if !d.WithoutReturning && d.resolvedMode == ModeMySQL && hasEnabledLastInsertID(d.DSN) {
		closeOnError()
		return fmt.Errorf("kingbase: remove get_last_insert_id=yes from DSN; this Dialector uses INSERT ... RETURNING directly")
	}

	callbackConfig := &callbacks.Config{
		CreateClauses: []string{"INSERT", "VALUES", "ON CONFLICT"},
		UpdateClauses: []string{"UPDATE", "SET", "FROM", "WHERE"},
		DeleteClauses: []string{"DELETE", "FROM", "WHERE"},
	}
	if !d.WithoutReturning {
		callbackConfig.CreateClauses = append(callbackConfig.CreateClauses, "RETURNING")
		callbackConfig.UpdateClauses = append(callbackConfig.UpdateClauses, "RETURNING")
		callbackConfig.DeleteClauses = append(callbackConfig.DeleteClauses, "RETURNING")
	}
	callbacks.RegisterDefaultCallbacks(db, callbackConfig)
	if d.DatabaseMode() == ModeOracle {
		// Scan uses these statement-local labels for both queries and DML
		// RETURNING, including Model(...).Scan through the Row processor.
		for _, register := range []func() error{
			func() error {
				return db.Callback().Create().Before("gorm:create").Register("kingbase:oracle_columns", d.mapOracleColumns)
			},
			func() error {
				return db.Callback().Query().Before("gorm:query").Register("kingbase:oracle_columns", d.mapOracleColumns)
			},
			func() error {
				return db.Callback().Update().Before("gorm:update").Register("kingbase:oracle_columns", d.mapOracleColumns)
			},
			func() error {
				return db.Callback().Delete().Before("gorm:delete").Register("kingbase:oracle_columns", d.mapOracleColumns)
			},
			func() error {
				return db.Callback().Row().Before("gorm:row").Register("kingbase:oracle_columns", d.mapOracleColumns)
			},
		} {
			if err := register(); err != nil {
				closeOnError()
				return fmt.Errorf("kingbase: register Oracle column mapping: %w", err)
			}
		}
	}
	if !d.WithoutReturning {
		if err := db.Callback().Create().Replace("gorm:create", createWithMapReturning(callbackConfig)); err != nil {
			closeOnError()
			return fmt.Errorf("kingbase: register map slice create: %w", err)
		}
		// GORM's generic Scan appends RETURNING rows to *[]map instead of
		// updating the maps that were inserted. Merge a one-to-one batch back
		// before later callbacks observe the destination.
		if err := db.Callback().Create().After("gorm:create").Before("gorm:save_after_associations").
			Register("kingbase:merge_map_returning", mergeMapReturning); err != nil {
			closeOnError()
			return fmt.Errorf("kingbase: register map RETURNING callback: %w", err)
		}
	}

	for name, builder := range d.ClauseBuilders() {
		db.ClauseBuilders[name] = builder
	}
	if d.Conn != nil {
		// Initialization succeeded; expose the original pool so GORM can use
		// optional interfaces such as ConnPoolBeginner and GetDBConnector.
		db.ConnPool = d.Conn
	}
	return nil
}

func hasEnabledLastInsertID(dsn string) bool {
	lower := strings.ToLower(dsn)
	for _, item := range strings.FieldsFunc(lower, func(r rune) bool {
		// KingbaseES compatibility (KB-MODE-014): gokb accepts both keyword
		// DSNs and URL-style query strings, so recognize the option after '?'
		// as well as after whitespace, '&' or ';'.
		return unicode.IsSpace(r) || r == '?' || r == '&' || r == ';'
	}) {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) == "get_last_insert_id" {
			switch strings.Trim(strings.TrimSpace(parts[1]), "'\"") {
			case "yes", "true", "1", "on":
				return true
			}
		}
	}
	return false
}

func (d *Dialector) Migrator(db *gorm.DB) gorm.Migrator {
	return Migrator{
		Migrator: migrator.Migrator{Config: migrator.Config{
			DB:                          db,
			Dialector:                   d,
			CreateIndexAfterCreateTable: true,
		}},
		dialector: d,
	}
}

func (d *Dialector) DefaultValueOf(*schema.Field) clause.Expression {
	return clause.Expr{SQL: "DEFAULT"}
}

func (d *Dialector) DataTypeOf(field *schema.Field) string {
	if d.DatabaseMode() == ModeOracle {
		return d.oracleDataTypeOf(field)
	}
	return d.mysqlDataTypeOf(field)
}

func (d *Dialector) SavePoint(tx *gorm.DB, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("kingbase: savepoint name must not be empty")
	}
	// KingbaseES compatibility (KB-SQL-018): treat a savepoint name as an
	// identifier instead of concatenating it. This preserves GORM-generated
	// names and prevents a caller-supplied name from injecting another command.
	return tx.Exec("SAVEPOINT ?", clause.Column{Name: name}).Error
}

func (d *Dialector) RollbackTo(tx *gorm.DB, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("kingbase: savepoint name must not be empty")
	}
	// See SavePoint: RollbackTo must apply exactly the same identifier quoting.
	return tx.Exec("ROLLBACK TO SAVEPOINT ?", clause.Column{Name: name}).Error
}
