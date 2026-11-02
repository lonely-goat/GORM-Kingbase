package kingbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
	"kingbase.com/gokb"
)

const testDriverName = "gorm_kingbase_recording_driver"

var testQueries = struct {
	sync.Mutex
	items []string
}{}

func init() {
	sql.Register(testDriverName, recordingDriver{})
}

type recordingDriver struct{}

func (recordingDriver) Open(name string) (driver.Conn, error) {
	return &recordingConn{mode: name}, nil
}

type recordingConn struct{ mode string }

func (*recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not implemented by the test connection")
}
func (*recordingConn) Close() error { return nil }
func (*recordingConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not implemented")
}
func (*recordingConn) Ping(context.Context) error { return nil }

func (conn *recordingConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	testQueries.Lock()
	testQueries.items = append(testQueries.items, query)
	testQueries.Unlock()

	lowerQuery := strings.ToLower(query)
	normalized, values := recordingArguments(query, args)
	if strings.Contains(strings.ToUpper(query), "SHOW DATABASE_MODE") {
		return &recordingRows{columns: []string{"database_mode"}, values: [][]driver.Value{{conn.mode}}}, nil
	}
	if strings.Contains(lowerQuery, "from information_schema.columns as c") {
		if err := recordingFilter(normalized, values, "c.table_schema", "c.table_name", 2); err != nil {
			return nil, err
		}
		// Reproduce V9R1C10's NULL EXTRA: the catalog identity fallback must
		// actually be present in the executed query to report auto-increment.
		autoMarker := ""
		if strings.Contains(normalized, "a.attidentity") {
			autoMarker = "auto_increment"
		}
		if normalizeMode(Mode(conn.mode)) == ModeOracle {
			autoMarker = "YES"
		}
		rows := &recordingRows{
			columns: []string{
				"column_name", "data_type", "character_maximum_length",
				"numeric_precision", "numeric_scale", "datetime_precision",
				"column_default", "is_nullable", "auto_marker", "comment",
			},
			values: [][]driver.Value{
				{"id", "bigint", nil, int64(64), int64(0), nil, nil, "NO", autoMarker, "identifier"},
				{"name", "character varying", int64(64), nil, nil, nil, "'guest'::character varying", "YES", "", "display name"},
				{"active", "boolean", nil, nil, nil, nil, nil, "YES", "", nil},
				{"amount", "numeric", nil, int64(12), int64(4), nil, nil, "YES", "", nil},
			},
		}
		if values[1] != "sql_models" || (values[0] != "public" && values[0] != "tenant") {
			rows.values = nil
		} else if values[0] == "tenant" {
			rows.values = rows.values[:2]
			rows.values[0][7], rows.values[0][8] = "YES", ""
			rows.values[1][2] = int64(128)
		}
		return rows, nil
	}
	if strings.Contains(lowerQuery, "from information_schema.table_constraints as tc") {
		if err := recordingFilter(normalized, values, "tc.constraint_schema", "tc.table_name", 2); err != nil {
			return nil, err
		}
		rows := &recordingRows{
			columns: []string{"column_name", "constraint_type", "column_count"},
			values: [][]driver.Value{
				{"id", "PRIMARY KEY", int64(1)},
				{"name", "UNIQUE", int64(1)},
				{"name", "UNIQUE", int64(2)},
				{"active", "UNIQUE", int64(2)},
			},
		}
		if values[1] != "sql_models" || values[0] != "public" {
			rows.values = nil
		}
		return rows, nil
	}
	if strings.Contains(lowerQuery, "from pg_catalog.pg_index as ind") {
		if err := recordingFilter(normalized, values, "ns.nspname", "tbl.relname", 2); err != nil {
			return nil, err
		}
		rows := &recordingRows{
			columns: []string{"table_name", "index_name", "is_unique", "is_primary", "column_name"},
			values: [][]driver.Value{
				{"sql_models", "idx_sql_models_name_active", true, false, "name"},
				{"sql_models", "idx_sql_models_name_active", true, false, "active"},
				{"sql_models", "sql_models_name_key", true, false, "name"},
				{"sql_models", "sql_models_pkey", true, true, "id"},
			},
		}
		if values[1] != "sql_models" || (values[0] != "public" && values[0] != "tenant") {
			rows.values = nil
		} else if values[0] == "tenant" {
			rows.values = [][]driver.Value{{"sql_models", "tenant_name_idx", false, false, "name"}}
		}
		return rows, nil
	}
	if strings.Contains(lowerQuery, "select table_schema, table_name, table_type") {
		if err := recordingFilter(normalized, values, "table_schema", "table_name", 2); err != nil {
			return nil, err
		}
		rows := &recordingRows{
			columns: []string{"table_schema", "table_name", "table_type"},
			values:  [][]driver.Value{{"public", "sql_models", "BASE TABLE"}},
		}
		if values[1] == "sql_models" && (values[0] == "public" || values[0] == "tenant") {
			rows.values[0][0] = values[0]
		} else {
			rows.values = nil
		}
		return rows, nil
	}
	if strings.Contains(lowerQuery, "select table_name from information_schema.tables") {
		if err := recordingFilter(normalized, values, "table_schema", "table_type", 2); err != nil {
			return nil, err
		}
		rows := &recordingRows{
			columns: []string{"table_name"},
			values:  [][]driver.Value{{"sql_models"}},
		}
		if values[0] != "public" || values[1] != "BASE TABLE" {
			rows.values = nil
		}
		return rows, nil
	}
	if normalized == "select current_database()" {
		return &recordingRows{columns: []string{"current_database"}, values: [][]driver.Value{{"test"}}}, nil
	}
	if strings.Contains(lowerQuery, "current_schema()") && !strings.Contains(lowerQuery, "from") {
		return &recordingRows{columns: []string{"current_schema"}, values: [][]driver.Value{{"public"}}}, nil
	}
	if strings.Contains(lowerQuery, "from information_schema.table_constraints") && strings.Contains(lowerQuery, "constraint_type = 'unique'") {
		if err := recordingFilter(normalized, values, "constraint_schema", "table_name", 3); err != nil {
			return nil, err
		}
		// The DryRun index probe uses a regular index unless a test explicitly
		// creates a constraint-backed one.
		return &recordingRows{columns: []string{"count"}, values: [][]driver.Value{{int64(0)}}}, nil
	}
	if strings.Contains(lowerQuery, "count(*)") {
		schemaColumn, tableColumn := "table_schema", "table_name"
		switch {
		case strings.Contains(lowerQuery, "from information_schema.table_constraints"):
			schemaColumn = "constraint_schema"
		case strings.Contains(lowerQuery, "from pg_catalog.pg_indexes"):
			schemaColumn, tableColumn = "schemaname", "tablename"
		}
		if err := recordingFilter(normalized, values, schemaColumn, tableColumn, len(values)); err != nil {
			return nil, err
		}
		count := int64(0)
		if len(values) >= 2 && (values[0] == "public" || values[0] == "tenant") {
			switch {
			case strings.Contains(lowerQuery, "from information_schema.tables"):
				// ddl_models is the existing-table fixture used by the Oracle
				// DryRun DropTable path; no CREATE is executed in DryRun.
				if (len(values) == 2 || (len(values) == 3 && values[2] == "BASE TABLE")) && (values[1] == "sql_models" || (values[0] == "public" && values[1] == "ddl_models")) {
					count = 1
				}
			case strings.Contains(lowerQuery, "from information_schema.columns"):
				if values[1] == "sql_models" && len(values) == 3 && (values[2] == "id" || values[2] == "name" || (values[0] == "public" && (values[2] == "active" || values[2] == "amount"))) {
					count = 1
				}
			case strings.Contains(lowerQuery, "from information_schema.table_constraints"):
				if values[0] == "public" && values[1] == "sql_models" && len(values) == 3 && (values[2] == "sql_models_pkey" || values[2] == "sql_models_name_key") {
					count = 1
				}
			case strings.Contains(lowerQuery, "from pg_catalog.pg_indexes"):
				if values[1] == "sql_models" && len(values) == 3 && ((values[0] == "public" && (values[2] == "idx_sql_models_name_active" || values[2] == "sql_models_name_key" || values[2] == "sql_models_pkey")) || (values[0] == "tenant" && values[2] == "tenant_name_idx")) {
					count = 1
				}
			}
		}
		return &recordingRows{columns: []string{"count"}, values: [][]driver.Value{{count}}}, nil
	}
	return nil, fmt.Errorf("unexpected recording query: %s", query)
}

var recordingBindNumber = regexp.MustCompile(`:\d+`)

func recordingArguments(query string, args []driver.NamedValue) (string, []string) {
	normalized := strings.Join(strings.Fields(strings.ToLower(query)), " ")
	normalized = recordingBindNumber.ReplaceAllString(normalized, "?")
	values := make([]string, len(args))
	for i, arg := range args {
		values[i] = fmt.Sprint(arg.Value)
	}
	if strings.Contains(normalized, "current_schema()") && strings.Contains(normalized, " from ") {
		normalized = strings.ReplaceAll(normalized, "current_schema()", "?")
		values = append([]string{"public"}, values...)
	}
	return normalized, values
}

func recordingFilter(query string, args []string, schemaColumn, tableColumn string, count int) error {
	want := "where " + schemaColumn + " = ? and " + tableColumn + " = ?"
	if !strings.Contains(query, want) || strings.Contains(query, " or ") || len(args) != count || count < 2 {
		return fmt.Errorf("recording metadata requires %q and %d arguments; got %q %v", want, count, query, args)
	}
	return nil
}

type recordingRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *recordingRows) Columns() []string { return rows.columns }
func (*recordingRows) Close() error           { return nil }
func (rows *recordingRows) Next(dest []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(dest, rows.values[rows.index])
	rows.index++
	return nil
}

type nopPool struct{}

func (nopPool) PrepareContext(context.Context, string) (*sql.Stmt, error) {
	return nil, errors.New("not implemented")
}
func (nopPool) ExecContext(context.Context, string, ...interface{}) (sql.Result, error) {
	return nil, errors.New("not implemented")
}
func (nopPool) QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error) {
	return nil, errors.New("not implemented")
}
func (nopPool) QueryRowContext(context.Context, string, ...interface{}) *sql.Row { return &sql.Row{} }

type sqlModel struct {
	ID        uint `gorm:"primaryKey"`
	Name      string
	Active    bool
	CreatedAt time.Time
}

func openDryRun(t *testing.T, mode Mode) (*gorm.DB, *Dialector) {
	t.Helper()
	dialector := New(Config{Conn: nopPool{}, Mode: mode}).(*Dialector)
	db, err := gorm.Open(dialector, &gorm.Config{
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		DryRun:                 true,
	})
	if err != nil {
		t.Fatalf("open dry-run DB: %v", err)
	}
	return db, dialector
}

func TestAutoModeDetection(t *testing.T) {
	for _, mode := range []Mode{ModeMySQL, ModeOracle} {
		t.Run(string(mode), func(t *testing.T) {
			sqlDB, err := sql.Open(testDriverName, string(mode))
			if err != nil {
				t.Fatal(err)
			}
			defer sqlDB.Close()

			dialector := New(Config{Conn: sqlDB, Mode: ModeAuto}).(*Dialector)
			_, err = gorm.Open(dialector, &gorm.Config{DisableAutomaticPing: true})
			if err != nil {
				t.Fatalf("initialize auto mode: %v", err)
			}
			if got := dialector.DatabaseMode(); got != mode {
				t.Fatalf("detected mode = %q, want %q", got, mode)
			}
		})
	}
}

func TestAutoModeRejectsUnsupportedMode(t *testing.T) {
	sqlDB, err := sql.Open(testDriverName, "sqlserver")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	_, err = gorm.Open(New(Config{Conn: sqlDB, Mode: ModeAuto}), &gorm.Config{DisableAutomaticPing: true})
	if err == nil || !strings.Contains(err.Error(), "unsupported database_mode") {
		t.Fatalf("expected unsupported-mode error, got %v", err)
	}
}

func TestModeSpecificSQL(t *testing.T) {
	tests := []struct {
		mode       Mode
		createPart []string
		queryPart  []string
	}{
		{
			mode:       ModeMySQL,
			createPart: []string{"INSERT INTO `sql_models`", "VALUES (?,?,?)", "RETURNING `id`"},
			queryPart:  []string{"FROM `sql_models`", "name = ?", "LIMIT ?"},
		},
		{
			mode:       ModeOracle,
			createPart: []string{`INSERT INTO "sql_models"`, "VALUES (:1,:2,:3)", `RETURNING "id"`},
			queryPart:  []string{`FROM "sql_models"`, "name = :1", "FETCH NEXT 1 ROW ONLY"},
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			db, _ := openDryRun(t, tt.mode)
			created := db.Create(&sqlModel{Name: "alice", Active: true})
			if created.Error != nil {
				t.Fatalf("build create SQL: %v", created.Error)
			}
			assertContainsAll(t, created.Statement.SQL.String(), tt.createPart...)

			queried := db.Where("name = ?", "alice").First(&sqlModel{})
			if queried.Error != nil {
				t.Fatalf("build query SQL: %v", queried.Error)
			}
			assertContainsAll(t, queried.Statement.SQL.String(), tt.queryPart...)
		})
	}
}

func TestDataTypeCompatibility(t *testing.T) {
	type typeModel struct {
		ID      uint `gorm:"primaryKey"`
		Tiny    int8
		Small   int16
		Regular int32
		Big     int64
		Flag    bool
		Amount  float64 `gorm:"precision:12;scale:4"`
		Code    string  `gorm:"size:64"`
		Notes   string
		Payload []byte
		At      time.Time `gorm:"precision:3"`
	}

	tests := []struct {
		mode Mode
		want map[string]string
	}{
		{ModeMySQL, map[string]string{
			"ID": "bigint AUTO_INCREMENT", "Tiny": "tinyint", "Small": "smallint",
			"Regular": "integer", "Big": "bigint", "Flag": "boolean",
			"Amount": "decimal(12,4)", "Code": "varchar(64)", "Notes": "longtext",
			"Payload": "longblob", "At": "datetime(3)",
		}},
		{ModeOracle, map[string]string{
			"ID": "bigint GENERATED BY DEFAULT AS IDENTITY", "Tiny": "tinyint", "Small": "smallint",
			"Regular": "integer", "Big": "bigint", "Flag": "boolean",
			"Amount": "NUMBER(12,4)", "Code": "VARCHAR2(64)", "Notes": "VARCHAR2(4000)",
			"Payload": "BLOB", "At": "TIMESTAMP(3) WITH TIME ZONE",
		}},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			_, dialector := openDryRun(t, tt.mode)
			parsed, err := schema.Parse(&typeModel{}, &sync.Map{}, schema.NamingStrategy{})
			if err != nil {
				t.Fatal(err)
			}
			for fieldName, want := range tt.want {
				field := parsed.LookUpField(fieldName)
				if got := dialector.DataTypeOf(field); got != want {
					t.Errorf("%s type = %q, want %q", fieldName, got, want)
				}
			}
		})
	}
}

func TestMigratorUsesCurrentSchemaAndModeMetadata(t *testing.T) {
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
			dialector := New(Config{Conn: sqlDB, Mode: mode})
			db, err := gorm.Open(dialector, &gorm.Config{DisableAutomaticPing: true})
			if err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasTable(&sqlModel{}) {
				t.Fatal("recording metadata query should report table exists")
			}

			testQueries.Lock()
			queries := strings.Join(testQueries.items, "\n")
			testQueries.Unlock()
			assertContainsAll(t, queries, "information_schema.tables", "CURRENT_SCHEMA()")
			if mode == ModeMySQL {
				if !strings.Contains(queries, "?") {
					t.Fatalf("MySQL metadata query should use ? binding: %s", queries)
				}
			} else if !strings.Contains(queries, ":1") {
				t.Fatalf("Oracle metadata query should use :N binding: %s", queries)
			}
		})
	}
}

func TestModeSpecificIndexAndColumnMetadata(t *testing.T) {
	for _, tt := range []struct {
		mode            Mode
		wantIndexView   string
		wantIdentityCol string
	}{
		{ModeMySQL, "pg_catalog.pg_indexes", "c.extra"},
		{ModeOracle, "pg_catalog.pg_indexes", "c.is_identity"},
	} {
		t.Run(string(tt.mode), func(t *testing.T) {
			testQueries.Lock()
			testQueries.items = nil
			testQueries.Unlock()

			sqlDB, err := sql.Open(testDriverName, string(tt.mode))
			if err != nil {
				t.Fatal(err)
			}
			defer sqlDB.Close()
			db, err := gorm.Open(New(Config{Conn: sqlDB, Mode: tt.mode}), &gorm.Config{DisableAutomaticPing: true})
			if err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasIndex(&sqlModel{}, "idx_sql_models_name_active") {
				t.Fatal("recording metadata query should report the fixture index")
			}
			if db.Migrator().HasIndex(&sqlModel{}, "idx_sql_models_name") {
				t.Fatal("recording metadata query reported a nonexistent index")
			}
			if _, err := db.Migrator().ColumnTypes(&sqlModel{}); err != nil {
				t.Fatalf("query column metadata: %v", err)
			}

			testQueries.Lock()
			queries := strings.Join(testQueries.items, "\n")
			testQueries.Unlock()
			assertContainsAll(t, queries, tt.wantIndexView, tt.wantIdentityCol)
		})
	}
}

func TestColumnTypeMetadataNeverFallsThroughToNilSQLColumnType(t *testing.T) {
	// information_schema returns NULL for bigint length/precision. This reproduces
	// the metadata shape that previously panicked during repeated AutoMigrate.
	column := &migrator.ColumnType{
		NameValue:     sql.NullString{String: "big", Valid: true},
		DataTypeValue: sql.NullString{String: "bigint", Valid: true},
	}
	completeColumnTypeMetadata(column, "bigint")

	if length, ok := column.Length(); !ok || length != 0 {
		t.Fatalf("Length() = (%d, %t), want (0, true)", length, ok)
	}
	if precision, scale, ok := column.DecimalSize(); !ok || precision != 0 || scale != 0 {
		t.Fatalf("DecimalSize() = (%d, %d, %t), want (0, 0, true)", precision, scale, ok)
	}
	if got := column.ScanType(); got != reflect.TypeOf(int64(0)) {
		t.Fatalf("ScanType() = %v, want int64", got)
	}
}

func TestDSNRejectsLegacyLastInsertIDOption(t *testing.T) {
	_, err := gorm.Open(New(Config{
		Conn: nopPool{}, Mode: ModeMySQL,
		DSN: "host=localhost get_last_insert_id=yes",
	}), &gorm.Config{DisableAutomaticPing: true})
	if err == nil || !strings.Contains(err.Error(), "remove get_last_insert_id=yes") {
		t.Fatalf("expected legacy LastInsertId option error, got %v", err)
	}
}

func TestErrorTranslation(t *testing.T) {
	dialector := &Dialector{}
	translated := dialector.Translate(&gokb.Error{Code: gokb.ErrorCode("23505")})
	if !errors.Is(translated, gorm.ErrDuplicatedKey) {
		t.Fatalf("translated error = %v, want ErrDuplicatedKey", translated)
	}
	original := errors.New("plain error")
	if got := dialector.Translate(original); got != original {
		t.Fatalf("unknown error should be returned unchanged, got %v", got)
	}
}

func TestQuoteAndExplain(t *testing.T) {
	for _, tt := range []struct {
		mode   Mode
		quoted string
		sql    string
	}{
		{ModeMySQL, "`public`.`user``events`", "SELECT 'alice'"},
		{ModeOracle, `"public"."user""events"`, "SELECT 'alice'"},
	} {
		_, dialector := openDryRun(t, tt.mode)
		var writer strings.Builder
		dialector.QuoteTo(&writer, "public.user"+map[Mode]string{ModeMySQL: "`", ModeOracle: `"`}[tt.mode]+"events")
		if got := writer.String(); got != tt.quoted {
			t.Errorf("%s quote = %q, want %q", tt.mode, got, tt.quoted)
		}
		placeholder := "?"
		if tt.mode == ModeOracle {
			placeholder = ":1"
		}
		if got := dialector.Explain("SELECT "+placeholder, "alice"); got != tt.sql {
			t.Errorf("%s explain = %q, want %q", tt.mode, got, tt.sql)
		}
	}
}

func TestMySQLOnConflict(t *testing.T) {
	db, _ := openDryRun(t, ModeMySQL)
	result := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&sqlModel{ID: 1, Name: "alice"})
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	assertContainsAll(t, result.Statement.SQL.String(), "ON CONFLICT (`id`) DO UPDATE SET", "`name`=`excluded`.`name`")
}

func assertContainsAll(t *testing.T, value string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(value, part) {
			t.Errorf("%q does not contain %q", value, part)
		}
	}
}
