package kingbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
	"kingbase.com/gokb"
)

// This file keeps the executable acceptance checks named after the matching
// cases in "Gorm-v2-KingbaseES 适配测试用例.md". Tests that require a real
// KingbaseES instance remain in integration/ and are never reported as passed
// merely because no DSN was configured.

const blockingDriverName = "gorm_kingbase_blocking_driver"

var blockingDriverCloseCount atomic.Int64

func init() {
	sql.Register(blockingDriverName, blockingDriver{})
}

type blockingDriver struct{}

func (blockingDriver) Open(string) (driver.Conn, error) { return &blockingConn{}, nil }

type blockingConn struct{}

func (*blockingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not implemented by the blocking test connection")
}
func (*blockingConn) Close() error {
	blockingDriverCloseCount.Add(1)
	return nil
}
func (*blockingConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not implemented by the blocking test connection")
}
func (*blockingConn) Ping(context.Context) error { return nil }
func (*blockingConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type capturedCall struct {
	query string
	args  []interface{}
}

type capturePool struct {
	mu    sync.Mutex
	calls []capturedCall
}

func (*capturePool) PrepareContext(context.Context, string) (*sql.Stmt, error) {
	return nil, errors.New("prepare is not implemented by the capture pool")
}
func (pool *capturePool) ExecContext(_ context.Context, query string, args ...interface{}) (sql.Result, error) {
	pool.mu.Lock()
	pool.calls = append(pool.calls, capturedCall{query: query, args: append([]interface{}(nil), args...)})
	pool.mu.Unlock()
	return captureResult(1), nil
}
func (*capturePool) QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error) {
	return nil, errors.New("query is not implemented by the capture pool")
}
func (*capturePool) QueryRowContext(context.Context, string, ...interface{}) *sql.Row {
	return &sql.Row{}
}

func (pool *capturePool) lastQuery(t *testing.T) string {
	t.Helper()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.calls) == 0 {
		t.Fatal("no SQL call was captured")
	}
	return pool.calls[len(pool.calls)-1].query
}

type captureResult int64

func (result captureResult) LastInsertId() (int64, error) { return 0, errors.New("not supported") }
func (result captureResult) RowsAffected() (int64, error) { return int64(result), nil }

type traceLogger struct {
	mu   sync.Mutex
	sqls []string
}

func (log *traceLogger) LogMode(logger.LogLevel) logger.Interface  { return log }
func (*traceLogger) Info(context.Context, string, ...interface{})  {}
func (*traceLogger) Warn(context.Context, string, ...interface{})  {}
func (*traceLogger) Error(context.Context, string, ...interface{}) {}
func (log *traceLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sqlText, _ := fc()
	log.mu.Lock()
	log.sqls = append(log.sqls, sqlText)
	log.mu.Unlock()
}

func (log *traceLogger) joinedSQL() string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return strings.Join(log.sqls, "\n")
}

func TestKBModeNormalizationAndValidation(t *testing.T) {
	// KB-MODE-005, KB-MODE-006 and KB-MODE-007.
	for raw, want := range map[string]Mode{
		"mysql": ModeMySQL, " MYSQL ": ModeMySQL,
		"oracle": ModeOracle, "\nOrAcLe\t": ModeOracle,
	} {
		got, err := parseDatabaseMode(raw)
		if err != nil || got != want {
			t.Errorf("parseDatabaseMode(%q) = (%q, %v), want (%q, nil)", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "sqlserver", "notmysql", "oracle-compatible"} {
		if _, err := parseDatabaseMode(raw); err == nil {
			t.Errorf("parseDatabaseMode(%q) should reject an unsupported mode", raw)
		}
	}

	pool := &capturePool{}
	_, err := gorm.Open(New(Config{Conn: pool, Mode: Mode("invalid")}), &gorm.Config{DisableAutomaticPing: true})
	if err == nil || !strings.Contains(err.Error(), "invalid configured mode") {
		t.Fatalf("invalid explicit mode error = %v", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.calls) != 0 {
		t.Fatalf("invalid mode executed SQL before validation: %#v", pool.calls)
	}
}

func TestKBModeExplicitSkipsDetectionAndAutoDetectsOnce(t *testing.T) {
	// KB-MODE-008, KB-MODE-009 and KB-NFR-003.
	for _, mode := range []Mode{ModeMySQL, ModeOracle} {
		t.Run("explicit-"+string(mode), func(t *testing.T) {
			testQueries.Lock()
			testQueries.items = nil
			testQueries.Unlock()

			sqlDB, err := sql.Open(testDriverName, string(mode))
			if err != nil {
				t.Fatal(err)
			}
			defer sqlDB.Close()
			if _, err = gorm.Open(New(Config{Conn: sqlDB, Mode: mode}), &gorm.Config{DisableAutomaticPing: true}); err != nil {
				t.Fatal(err)
			}
			testQueries.Lock()
			defer testQueries.Unlock()
			if len(testQueries.items) != 0 {
				t.Fatalf("explicit mode unexpectedly executed detection SQL: %v", testQueries.items)
			}
		})

		t.Run("auto-once-"+string(mode), func(t *testing.T) {
			testQueries.Lock()
			testQueries.items = nil
			testQueries.Unlock()

			sqlDB, err := sql.Open(testDriverName, string(mode))
			if err != nil {
				t.Fatal(err)
			}
			defer sqlDB.Close()
			dialector := New(Config{Conn: sqlDB, Mode: ModeAuto}).(*Dialector)
			db, err := gorm.Open(dialector, &gorm.Config{DisableAutomaticPing: true})
			if err != nil {
				t.Fatal(err)
			}
			for index := 0; index < 10; index++ {
				_ = db.Session(&gorm.Session{DryRun: true}).Where("id = ?", index).Find(&sqlModel{}).Statement.SQL.String()
			}
			testQueries.Lock()
			defer testQueries.Unlock()
			if len(testQueries.items) != 1 || !strings.Contains(strings.ToUpper(testQueries.items[0]), "SHOW DATABASE_MODE") {
				t.Fatalf("mode detection queries = %v, want exactly one", testQueries.items)
			}
		})
	}
}

func TestKBModeDetectionTimeoutAndConnectionOwnership(t *testing.T) {
	// KB-MODE-010, KB-MODE-011, KB-MODE-012 and KB-MODE-013.
	blockingDriverCloseCount.Store(0)
	timeout := 15 * time.Millisecond
	started := time.Now()
	_, err := gorm.Open(New(Config{
		DriverName:           blockingDriverName,
		DSN:                  "internal",
		Mode:                 ModeAuto,
		ModeDetectionTimeout: timeout,
	}), &gorm.Config{DisableAutomaticPing: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("internal timeout error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed < timeout || elapsed > time.Second {
		t.Fatalf("mode detection elapsed %v, want approximately %v", elapsed, timeout)
	}
	if blockingDriverCloseCount.Load() == 0 {
		t.Fatal("Dialector-owned connection was not closed after initialization failed")
	}

	externalDB, err := sql.Open(blockingDriverName, "external")
	if err != nil {
		t.Fatal(err)
	}
	defer externalDB.Close()
	_, err = gorm.Open(New(Config{
		Conn: externalDB, Mode: ModeAuto, ModeDetectionTimeout: timeout,
	}), &gorm.Config{DisableAutomaticPing: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("external timeout error = %v, want context deadline exceeded", err)
	}
	if err := externalDB.Ping(); err != nil {
		t.Fatalf("caller-owned connection was closed by Dialector: %v", err)
	}
}

func TestKBModeLegacyLastInsertIDDSNVariants(t *testing.T) {
	// KB-MODE-014.
	for _, dsn := range []string{
		"host=localhost get_last_insert_id=yes",
		"host=localhost;GET_LAST_INSERT_ID=TRUE",
		"kingbase://localhost/db?get_last_insert_id=1&sslmode=disable",
		"host=localhost\tget_last_insert_id='on'",
	} {
		if !hasEnabledLastInsertID(dsn) {
			t.Errorf("legacy LastInsertId option was not detected in %q", dsn)
		}
	}
	for _, dsn := range []string{
		"host=localhost get_last_insert_id=no",
		"host=get_last_insert_id=yes.example.com",
		"kingbase://localhost/db?other=yes",
	} {
		if hasEnabledLastInsertID(dsn) {
			t.Errorf("legacy LastInsertId option was falsely detected in %q", dsn)
		}
	}
}

func TestKBSQLPlaceholdersAndParameterization(t *testing.T) {
	// KB-SQL-001, KB-SQL-002 and KB-SQL-005.
	malicious := "x'; DROP TABLE users; -- 中文"
	for _, tt := range []struct {
		mode Mode
		want []string
	}{
		{ModeMySQL, []string{"name = ?", "active = ?"}},
		{ModeOracle, []string{"name = :1", "active = :2"}},
	} {
		db, _ := openDryRun(t, tt.mode)
		result := db.Where("name = ? AND active = ?", malicious, true).Find(&sqlModel{})
		if result.Error != nil {
			t.Fatal(result.Error)
		}
		sqlText := result.Statement.SQL.String()
		assertContainsAll(t, sqlText, tt.want...)
		if strings.Contains(sqlText, malicious) {
			t.Fatalf("%s SQL inlined a user value: %s", tt.mode, sqlText)
		}
		if len(result.Statement.Vars) != 2 || result.Statement.Vars[0] != malicious || result.Statement.Vars[1] != true {
			t.Fatalf("%s Vars = %#v", tt.mode, result.Statement.Vars)
		}

		second := db.Where("name = ?", "second").Find(&sqlModel{})
		if tt.mode == ModeOracle && !strings.Contains(second.Statement.SQL.String(), "name = :1") {
			t.Fatalf("Oracle placeholder numbering leaked across statements: %s", second.Statement.SQL.String())
		}
	}
}

func TestKBSQLIdentifierQuoting(t *testing.T) {
	// KB-SQL-003 and KB-SQL-004.
	for _, tt := range []struct {
		mode  Mode
		input string
		want  string
	}{
		{ModeMySQL, "schema.table", "`schema`.`table`"},
		{ModeMySQL, "schema.ta`ble", "`schema`.`ta``ble`"},
		{ModeOracle, "schema.table", `"schema"."table"`},
		{ModeOracle, `schema.ta"ble`, `"schema"."ta""ble"`},
	} {
		_, dialector := openDryRun(t, tt.mode)
		var writer strings.Builder
		dialector.QuoteTo(&writer, tt.input)
		if got := writer.String(); got != tt.want {
			t.Errorf("%s QuoteTo(%q) = %q, want %q", tt.mode, tt.input, got, tt.want)
		}
	}
}

func TestKBSQLPagination(t *testing.T) {
	// KB-SQL-007 through KB-SQL-010, plus the JOIN ambiguity in KB-ADV-008.
	mysqlDB, _ := openDryRun(t, ModeMySQL)
	mysqlResult := mysqlDB.Order("id").Limit(2).Offset(3).Find(&sqlModel{})
	assertContainsAll(t, mysqlResult.Statement.SQL.String(), "ORDER BY id", "LIMIT ? OFFSET ?")

	oracleDB, _ := openDryRun(t, ModeOracle)
	oracleLimit := oracleDB.Limit(2).Find(&sqlModel{})
	assertContainsAll(t, oracleLimit.Statement.SQL.String(), `ORDER BY "sql_models"."id"`, "FETCH NEXT 2 ROWS ONLY")
	oracleOffset := oracleDB.Offset(3).Find(&sqlModel{})
	assertContainsAll(t, oracleOffset.Statement.SQL.String(), "OFFSET 3 ROWS")
	oracleBoth := oracleDB.Limit(2).Offset(3).Find(&sqlModel{})
	assertContainsAll(t, oracleBoth.Statement.SQL.String(), "OFFSET 3 ROWS", "FETCH NEXT 2 ROWS ONLY")

	joined := oracleDB.Model(&sqlModel{}).
		Joins(`JOIN "other_models" ON "other_models"."id" = "sql_models"."id"`).
		Limit(1).Find(&[]sqlModel{})
	assertContainsAll(t, joined.Statement.SQL.String(), `ORDER BY "sql_models"."id"`, "FETCH NEXT 1 ROW ONLY")
}

func TestKBSQLMySQLUpsertVariants(t *testing.T) {
	// KB-SQL-011, KB-SQL-012 and KB-SQL-013.
	db, _ := openDryRun(t, ModeMySQL)
	updateAll := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&sqlModel{ID: 1, Name: "new"})
	assertContainsAll(t, updateAll.Statement.SQL.String(), "ON CONFLICT (`id`) DO UPDATE SET", "`name`=`excluded`.`name`")

	selected := db.Clauses(clause.OnConflict{DoUpdates: clause.Assignments(map[string]interface{}{
		"name": clause.Column{Table: "excluded", Name: "name"},
	})}).Create(&sqlModel{ID: 1, Name: "new"})
	assertContainsAll(t, selected.Statement.SQL.String(), "ON DUPLICATE KEY UPDATE", "`name`=VALUES(`name`)")

	doNothing := db.Table("widgets").Clauses(clause.OnConflict{DoNothing: true}).Create(map[string]interface{}{"name": "new"})
	if doNothing.Error != nil {
		t.Fatal(doNothing.Error)
	}
	assertContainsAll(t, doNothing.Statement.SQL.String(), "ON CONFLICT DO NOTHING")
}

func TestKBSQLReturningConfiguration(t *testing.T) {
	// KB-SQL-015, KB-SQL-016 and KB-SQL-017.
	for _, mode := range []Mode{ModeMySQL, ModeOracle} {
		db, _ := openDryRun(t, mode)
		created := db.Create(&sqlModel{Name: "returning"})
		if count := strings.Count(strings.ToUpper(created.Statement.SQL.String()), "RETURNING"); count != 1 {
			t.Fatalf("%s Create RETURNING count = %d: %s", mode, count, created.Statement.SQL.String())
		}
		updated := db.Model(&sqlModel{}).Where("id = ?", 1).
			Clauses(clause.Returning{}).Update("name", "updated")
		assertContainsAll(t, strings.ToUpper(updated.Statement.SQL.String()), "UPDATE", "RETURNING")
		deleted := db.Where("id = ?", 1).Clauses(clause.Returning{}).Delete(&sqlModel{})
		assertContainsAll(t, strings.ToUpper(deleted.Statement.SQL.String()), "DELETE", "RETURNING")

		without := New(Config{Conn: nopPool{}, Mode: mode, WithoutReturning: true})
		withoutDB, err := gorm.Open(without, &gorm.Config{
			DisableAutomaticPing: true, SkipDefaultTransaction: true, DryRun: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		withoutCreate := withoutDB.Create(&sqlModel{ID: 10, Name: "no-returning"})
		if strings.Contains(strings.ToUpper(withoutCreate.Statement.SQL.String()), "RETURNING") {
			t.Fatalf("%s WithoutReturning SQL = %s", mode, withoutCreate.Statement.SQL.String())
		}
	}
}

func TestKBSQLSavepointNamesAreQuoted(t *testing.T) {
	// KB-SQL-018.
	for _, tt := range []struct {
		mode Mode
		want string
	}{
		{ModeMySQL, "SAVEPOINT `sp``; DROP TABLE users;--`"},
		{ModeOracle, `SAVEPOINT "sp""; DROP TABLE users;--"`},
	} {
		pool := &capturePool{}
		dialector := New(Config{Conn: pool, Mode: tt.mode}).(*Dialector)
		db, err := gorm.Open(dialector, &gorm.Config{DisableAutomaticPing: true, SkipDefaultTransaction: true})
		if err != nil {
			t.Fatal(err)
		}
		name := "sp" + map[Mode]string{ModeMySQL: "`", ModeOracle: `"`}[tt.mode] + "; DROP TABLE users;--"
		if err := dialector.SavePoint(db, name); err != nil {
			t.Fatal(err)
		}
		if got := pool.lastQuery(t); got != tt.want {
			t.Errorf("%s SavePoint SQL = %q, want %q", tt.mode, got, tt.want)
		}
		if err := dialector.RollbackTo(db, name); err != nil {
			t.Fatal(err)
		}
		if got := pool.lastQuery(t); got != strings.Replace(tt.want, "SAVEPOINT ", "ROLLBACK TO SAVEPOINT ", 1) {
			t.Errorf("%s RollbackTo SQL = %q", tt.mode, got)
		}
		if err := dialector.SavePoint(db, " "); err == nil {
			t.Fatal("empty savepoint name should fail")
		}
	}
}

func TestKBTypeMappingsAndConfiguration(t *testing.T) {
	// KB-TYPE-001 through KB-TYPE-010, KB-TYPE-012, KB-TYPE-016 and KB-TYPE-018.
	type typeMatrix struct {
		ID       uint `gorm:"primaryKey"`
		Signed8  int8
		Signed16 int16
		Signed32 int32
		Signed64 int64
		Unsigned uint64
		Float32  float32
		Float64  float64
		Decimal  float64 `gorm:"precision:18;scale:6"`
		Short    string  `gorm:"size:64"`
		Indexed  string  `gorm:"index"`
		Text     string
		Bytes    []byte
		At       time.Time `gorm:"precision:3"`
		Explicit string    `gorm:"type:custom_text"`
	}
	parsed, err := schema.Parse(&typeMatrix{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		mode Mode
		want map[string]string
	}{
		{ModeMySQL, map[string]string{
			"ID": "bigint AUTO_INCREMENT", "Signed8": "tinyint", "Signed16": "smallint",
			"Signed32": "integer", "Signed64": "bigint", "Unsigned": "bigint",
			"Float32": "float", "Float64": "double", "Decimal": "decimal(18,6)",
			"Short": "varchar(64)", "Indexed": "varchar(191)", "Text": "longtext",
			"Bytes": "longblob", "At": "datetime(3)", "Explicit": "custom_text",
		}},
		{ModeOracle, map[string]string{
			"ID": "bigint GENERATED BY DEFAULT AS IDENTITY", "Signed8": "tinyint", "Signed16": "smallint",
			"Signed32": "integer", "Signed64": "bigint", "Unsigned": "bigint",
			"Float32": "DOUBLE PRECISION", "Float64": "DOUBLE PRECISION", "Decimal": "NUMBER(18,6)",
			"Short": "VARCHAR2(64)", "Indexed": "VARCHAR2(4000)", "Text": "VARCHAR2(4000)",
			"Bytes": "BLOB", "At": "TIMESTAMP(3) WITH TIME ZONE", "Explicit": "custom_text",
		}},
	} {
		_, dialector := openDryRun(t, tt.mode)
		for fieldName, want := range tt.want {
			if got := dialector.DataTypeOf(parsed.LookUpField(fieldName)); got != want {
				t.Errorf("%s %s type = %q, want %q", tt.mode, fieldName, got, want)
			}
		}
	}

	configured := &Dialector{Config: Config{Mode: ModeOracle, DefaultStringSize: 512}}
	if got := configured.DataTypeOf(parsed.LookUpField("Text")); got != "VARCHAR2(512)" {
		t.Errorf("configured Oracle default string type = %q", got)
	}
	configured.DefaultStringSize = 5000
	if got := configured.DataTypeOf(parsed.LookUpField("Text")); got != "CLOB" {
		t.Errorf("oversized Oracle default string type = %q, want CLOB", got)
	}
}

func TestKBMetadataQualifiedNamesAndDefaultNormalization(t *testing.T) {
	// KB-META-002 and KB-META-011.
	for _, tt := range []struct {
		input       string
		wantSchema  string
		wantTable   string
		wantSuccess bool
	}{
		{"tenant.users", "tenant", "users", true},
		{" `tenant`.`user``events` ", "tenant", "user`events", true},
		{`"tenant.name"."user""events"`, "tenant.name", `user"events`, true},
		{"users", "", "", false},
		{"catalog.tenant.users", "", "", false},
		{`"unterminated.users`, "", "", false},
	} {
		schemaName, tableName, ok := splitQualifiedName(tt.input)
		if ok != tt.wantSuccess || schemaName != tt.wantSchema || tableName != tt.wantTable {
			t.Errorf("splitQualifiedName(%q) = (%q, %q, %t)", tt.input, schemaName, tableName, ok)
		}
	}

	for _, tt := range []struct {
		input string
		want  string
	}{
		{"'value'::character varying", "value"},
		{"'scheme::value'::text", "scheme::value"},
		{"'it''s'::text", "it's"},
		{"CURRENT_TIMESTAMP", "CURRENT_TIMESTAMP"},
		{"nextval('seq'::regclass)", "nextval('seq'::regclass)"},
	} {
		got := normalizeDefaultValue(sql.NullString{String: tt.input, Valid: true})
		if !got.Valid || got.String != tt.want {
			t.Errorf("normalizeDefaultValue(%q) = %#v, want %q", tt.input, got, tt.want)
		}
	}
	if got := normalizeDefaultValue(sql.NullString{}); got.Valid {
		t.Errorf("NULL default became valid: %#v", got)
	}
}

func TestKBMetadataColumnTypesIndexesAndTables(t *testing.T) {
	// KB-META-001, KB-META-003 through KB-META-010 and KB-DDL-018.
	for _, mode := range []Mode{ModeMySQL, ModeOracle} {
		t.Run(string(mode), func(t *testing.T) {
			sqlDB, err := sql.Open(testDriverName, string(mode))
			if err != nil {
				t.Fatal(err)
			}
			defer sqlDB.Close()
			db, err := gorm.Open(New(Config{Conn: sqlDB, Mode: mode}), &gorm.Config{DisableAutomaticPing: true})
			if err != nil {
				t.Fatal(err)
			}

			if got := db.Migrator().CurrentDatabase(); got != "test" {
				t.Errorf("CurrentDatabase() = %q, want test", got)
			}
			tables, err := db.Migrator().GetTables()
			if err != nil || !reflect.DeepEqual(tables, []string{"sql_models"}) {
				t.Errorf("GetTables() = (%v, %v)", tables, err)
			}
			if !db.Migrator().HasTable(&sqlModel{}) || !db.Migrator().HasColumn(&sqlModel{}, "Name") ||
				!db.Migrator().HasConstraint(&sqlModel{}, "sql_models_pkey") || !db.Migrator().HasIndex(&sqlModel{}, "idx_sql_models_name_active") {
				t.Fatal("recording metadata should report table, column, constraint and index")
			}
			if db.Migrator().HasTable("missing_table") || db.Migrator().HasColumn(&sqlModel{}, "missing_column") ||
				db.Migrator().HasConstraint(&sqlModel{}, "missing_constraint") || db.Migrator().HasIndex(&sqlModel{}, "missing_index") {
				t.Fatal("recording metadata reported a missing table, column, constraint or index")
			}

			columnTypes, err := db.Migrator().ColumnTypes(&sqlModel{})
			if err != nil {
				t.Fatal(err)
			}
			if len(columnTypes) != 4 {
				t.Fatalf("ColumnTypes length = %d, want 4", len(columnTypes))
			}
			byName := map[string]gorm.ColumnType{}
			for _, columnType := range columnTypes {
				byName[columnType.Name()] = columnType
			}
			if auto, ok := byName["id"].AutoIncrement(); !ok || !auto {
				t.Errorf("id AutoIncrement = (%t, %t)", auto, ok)
			}
			if primary, ok := byName["id"].PrimaryKey(); !ok || !primary {
				t.Errorf("id PrimaryKey = (%t, %t)", primary, ok)
			}
			if unique, ok := byName["name"].Unique(); !ok || !unique {
				t.Errorf("name Unique = (%t, %t)", unique, ok)
			}
			if unique, ok := byName["active"].Unique(); !ok || unique {
				t.Errorf("composite-index active column Unique = (%t,%t), want false,true", unique, ok)
			}
			if length, ok := byName["name"].Length(); !ok || length != 64 {
				t.Errorf("name Length = (%d, %t)", length, ok)
			}
			if value, ok := byName["name"].DefaultValue(); !ok || value != "guest" {
				t.Errorf("name DefaultValue = (%q, %t)", value, ok)
			}
			if precision, scale, ok := byName["amount"].DecimalSize(); !ok || precision != 12 || scale != 4 {
				t.Errorf("amount DecimalSize = (%d, %d, %t)", precision, scale, ok)
			}

			indexes, err := db.Migrator().GetIndexes(&sqlModel{})
			if err != nil {
				t.Fatal(err)
			}
			if len(indexes) != 3 {
				t.Fatalf("GetIndexes length = %d, want 3", len(indexes))
			}
			columns := indexes[0].Columns()
			if !reflect.DeepEqual(columns, []string{"name", "active"}) {
				t.Errorf("composite index columns = %v", columns)
			}
			unique, _ := indexes[0].Unique()
			primary, _ := indexes[0].PrimaryKey()
			if !unique || primary {
				t.Errorf("composite index flags = unique:%t primary:%t", unique, primary)
			}

			tableType, err := db.Migrator().TableType(&sqlModel{})
			if err != nil {
				t.Fatal(err)
			}
			if tableType.Schema() != "public" || tableType.Name() != "sql_models" || tableType.Type() != "BASE TABLE" {
				t.Errorf("TableType = schema:%q name:%q type:%q", tableType.Schema(), tableType.Name(), tableType.Type())
			}
		})
	}
}

func TestKBDDLModeSpecificDryRun(t *testing.T) {
	// KB-META-013 and the SQL form of KB-DDL-001/003/004/010/015/016/017.
	type ddlModel struct {
		ID   uint   `gorm:"primaryKey"`
		Name string `gorm:"size:64;uniqueIndex;comment:owner's display name -- DROP TABLE users"`
	}
	for _, mode := range []Mode{ModeMySQL, ModeOracle} {
		t.Run(string(mode), func(t *testing.T) {
			testQueries.Lock()
			testQueries.items = nil
			testQueries.Unlock()

			sqlDB, err := sql.Open(testDriverName, string(mode))
			if err != nil {
				t.Fatal(err)
			}
			defer sqlDB.Close()
			log := &traceLogger{}
			db, err := gorm.Open(New(Config{Conn: sqlDB, Mode: mode}), &gorm.Config{
				DisableAutomaticPing: true,
				DryRun:               true,
				Logger:               log,
			})
			if err != nil {
				t.Fatal(err)
			}
			migrator := db.Migrator()
			quote := "`"
			modify, drop := " MODIFY COLUMN ", "DROP TABLE IF EXISTS "
			if mode == ModeOracle {
				quote, modify, drop = `"`, " MODIFY ", "DROP TABLE "
			}
			q := func(name string) string { return quote + name + quote }
			comment := "IS 'owner''s display name -- DROP TABLE users'"
			for _, operation := range []struct {
				name string
				run  func() error
				want []string
			}{
				{"create", func() error { return migrator.CreateTable(&ddlModel{}) }, []string{"CREATE TABLE " + q("ddl_models"), "CREATE UNIQUE INDEX", comment}},
				{"add-column", func() error { return migrator.AddColumn(&ddlModel{}, "Name") }, []string{"ALTER TABLE " + q("ddl_models") + " ADD " + q("name"), comment}},
				{"alter-column", func() error { return migrator.AlterColumn(&ddlModel{}, "Name") }, []string{"ALTER TABLE " + q("ddl_models") + modify + q("name"), comment}},
				{"rename-index", func() error { return migrator.RenameIndex(&ddlModel{}, "idx_old", "idx_new") }, []string{"ALTER INDEX " + q("idx_old") + " RENAME TO " + q("idx_new")}},
				{"drop-index", func() error { return migrator.DropIndex(&ddlModel{}, "idx_new") }, []string{"DROP INDEX " + q("idx_new")}},
				{"drop-table", func() error { return migrator.DropTable(&ddlModel{}) }, []string{drop + q("ddl_models")}},
			} {
				t.Run(operation.name, func(t *testing.T) {
					log.mu.Lock()
					log.sqls = nil
					log.mu.Unlock()
					if err := operation.run(); err != nil {
						t.Fatal(err)
					}
					sqlLog := log.joinedSQL()
					assertContainsAll(t, sqlLog, operation.want...)
					if strings.Contains(sqlLog, "IS 'owner's") || strings.Contains(sqlLog, " RENAME INDEX ") || strings.Contains(sqlLog, q("idx_new")+" ON ") {
						t.Fatalf("incorrect DDL or unescaped comment: %s", sqlLog)
					}
					if operation.name == "drop-table" && mode == ModeOracle {
						assertContainsAll(t, sqlLog, "information_schema.tables", "CASCADE CONSTRAINTS")
					}
				})
			}
		})
	}
}

func TestKBSchemaIndexIdentifierQuoting(t *testing.T) {
	for _, mode := range []Mode{ModeMySQL, ModeOracle} {
		t.Run(string(mode), func(t *testing.T) {
			pool := &capturePool{}
			db, err := gorm.Open(New(Config{Conn: pool, Mode: mode}), &gorm.Config{DisableAutomaticPing: true})
			if err != nil {
				t.Fatal(err)
			}
			quote := "`"
			if mode == ModeOracle {
				quote = `"`
			}
			// Include dots and delimiter characters inside a single part.
			namespace, index := "tenant.part"+quote+"x", "owner"+quote+"s.idx"
			q := func(value string) string { return quote + strings.ReplaceAll(value, quote, quote+quote) + quote }
			if err := db.Table(q(namespace)+"."+q("probe")).Migrator().RenameIndex(&sqlModel{}, index, "renamed"); err != nil {
				t.Fatal(err)
			}
			want := "ALTER INDEX " + q(namespace) + "." + q(index) + " RENAME TO " + q("renamed")
			if got := pool.lastQuery(t); got != want {
				t.Fatalf("quoted schema/index SQL=%s want=%s", got, want)
			}
		})
	}
}

func TestKBMetadataExplicitSchema(t *testing.T) {
	for _, mode := range []Mode{ModeMySQL, ModeOracle} {
		t.Run(string(mode), func(t *testing.T) {
			pool, err := sql.Open(testDriverName, string(mode))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pool.Close() })
			db, err := gorm.Open(New(Config{Conn: pool, Mode: mode}), &gorm.Config{DisableAutomaticPing: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, viaTable := range []bool{false, true} {
				m, model := db.Migrator(), interface{}("tenant.sql_models")
				if viaTable {
					m, model = db.Table("tenant.sql_models").Migrator(), &sqlModel{}
				}
				columns, err := m.ColumnTypes(model)
				if err != nil || len(columns) != 2 {
					t.Fatalf("tenant columns: %v, err=%v", columns, err)
				}
				length, known := columns[1].Length()
				if columns[1].Name() != "name" || !known || length != 128 {
					t.Fatalf("wrong tenant name metadata: %s length=(%d,%t)", columns[1].Name(), length, known)
				}
				indexes, err := m.GetIndexes(model)
				if err != nil || len(indexes) != 1 || indexes[0].Name() != "tenant_name_idx" {
					t.Fatalf("tenant indexes=%v err=%v", indexes, err)
				}
				kind, err := m.TableType(model)
				if err != nil || kind.Schema() != "tenant" || kind.Name() != "sql_models" {
					t.Fatalf("tenant table type=%v err=%v", kind, err)
				}
				if !m.HasTable(model) {
					t.Fatal("tenant table missing")
				}
			}
			if _, err := db.Migrator().ColumnTypes("missing.sql_models"); err == nil {
				t.Fatal("missing schema returned column metadata")
			}
			if indexes, err := db.Migrator().GetIndexes("missing.sql_models"); err != nil || len(indexes) != 0 {
				t.Fatalf("missing schema indexes=%v err=%v", indexes, err)
			}
			if _, err := db.Migrator().TableType("missing.sql_models"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("missing table type error=%v", err)
			}
		})
	}
}

func TestKBMetadataHelperCoverage(t *testing.T) {
	// KB-META-007/008/011: stable accessors for every metadata family.
	for _, tt := range []struct {
		dataType string
		wantType reflect.Type
	}{
		{"boolean", reflect.TypeOf(false)},
		{"bigint", reflect.TypeOf(int64(0))},
		{"numeric", reflect.TypeOf(float64(0))},
		{"timestamp with time zone", reflect.TypeOf(time.Time{})},
		{"blob", reflect.TypeOf([]byte(nil))},
		{"varchar", reflect.TypeOf("")},
	} {
		if got := scanTypeForDatabaseType(tt.dataType); got != tt.wantType {
			t.Errorf("scanTypeForDatabaseType(%q) = %v, want %v", tt.dataType, got, tt.wantType)
		}
	}
	if got := buildColumnTypeName("varchar", sql.NullInt64{Int64: 64, Valid: true}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}); got != "varchar(64)" {
		t.Errorf("varchar type name = %q", got)
	}
	if got := buildColumnTypeName("numeric", sql.NullInt64{}, sql.NullInt64{Int64: 12, Valid: true}, sql.NullInt64{Int64: 4, Valid: true}, sql.NullInt64{}); got != "numeric(12,4)" {
		t.Errorf("numeric type name = %q", got)
	}
	if got := buildColumnTypeName("timestamp", sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{Int64: 3, Valid: true}); got != "timestamp(3)" {
		t.Errorf("timestamp type name = %q", got)
	}
	if aliases := (Migrator{}).GetTypeAliases("VARCHAR2"); !reflect.DeepEqual(aliases, []string{"character varying", "varchar"}) {
		t.Errorf("VARCHAR2 aliases = %v", aliases)
	}
}

func TestKBErrorTranslationMatrix(t *testing.T) {
	// KB-TX-001 through KB-TX-005.
	for code, want := range map[string]error{
		"23505": gorm.ErrDuplicatedKey,
		"23503": gorm.ErrForeignKeyViolated,
		"23514": gorm.ErrCheckConstraintViolated,
		"42703": gorm.ErrInvalidField,
	} {
		wrapped := errors.Join(errors.New("outer"), &gokb.Error{Code: gokb.ErrorCode(code)})
		if got := (&Dialector{}).Translate(wrapped); !errors.Is(got, want) {
			t.Errorf("Translate(%s) = %v, want %v", code, got, want)
		}
	}
	unknown := &gokb.Error{Code: gokb.ErrorCode("99999")}
	if got := (&Dialector{}).Translate(unknown); !reflect.DeepEqual(got, unknown) {
		t.Errorf("unknown error changed: got %v, want %v", got, unknown)
	}
}

func TestKBNamingStrategyDefaults(t *testing.T) {
	// Supports KB-DDL-010/017 by keeping generated object names within KES limits.
	db, err := gorm.Open(New(Config{Conn: nopPool{}, Mode: ModeMySQL}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	naming, ok := db.NamingStrategy.(schema.NamingStrategy)
	if !ok || naming.IdentifierMaxLength != defaultIdentifierMaxLength {
		t.Fatalf("default NamingStrategy = %#v", db.NamingStrategy)
	}

	explicit := schema.NamingStrategy{IdentifierMaxLength: 32}
	db, err = gorm.Open(New(Config{Conn: nopPool{}, Mode: ModeMySQL}), &gorm.Config{
		DisableAutomaticPing: true, NamingStrategy: explicit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := db.NamingStrategy.(schema.NamingStrategy).IdentifierMaxLength; got != 32 {
		t.Fatalf("explicit IdentifierMaxLength = %d, want 32", got)
	}
}
