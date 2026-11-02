// Command model-mapping demonstrates requirements 6.2.2.
// Parse cases are local. Mapping cases use isolated, rolled-back database objects.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
	"kingbase.com/gokb"
	kingbase "kingbase.com/gorm-driver-kingbase"
)

type BaseFields struct {
	ID uint `gorm:"primaryKey"`
}

type Product struct {
	BaseFields
	Name    string `gorm:"column:product_name;size:64;not null"`
	Enabled bool
	Price   float64 `gorm:"precision:12;scale:4"`
	Ignored string  `gorm:"-"`
}

func (Product) TableName() string {
	return "gorm_doc_products"
}

type InvalidModel struct {
	ID         uint
	Attributes map[string]string // No serializer, explicit type, or conversion.
}

type InvalidMapping struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"type:kb_doc_missing_type_622"`
}

func main() {
	logger.Default = logger.Default.LogMode(logger.Silent)
	name := flag.String("case", "parse-ok", "parse-ok, parse-error, mapping-ok, or mapping-error")
	mode := flag.String("mode", "", "required only for mapping cases: mysql or oracle")
	flag.Parse()
	fmt.Printf("case=%s\n", *name)
	var err error
	switch *name {
	case "parse-ok":
		err = parseOK()
	case "parse-error":
		err = parseError()
	case "mapping-ok", "mapping-error":
		if *mode != "mysql" && *mode != "oracle" {
			fmt.Fprintln(os.Stderr, "ERROR: -mode must be mysql or oracle for mapping cases")
			os.Exit(2)
		}
		if *name == "mapping-ok" {
			err = mappingOK(*mode)
		} else {
			err = mappingError(*mode)
		}
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

func parseOK() error {
	parsed, err := schema.Parse(&Product{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		return err
	}
	if err := verifyParsed(parsed); err != nil {
		return err
	}
	fmt.Println("parser=GORM common (no database connection)")
	fmt.Printf("table=%s\n", parsed.Table)
	fmt.Printf("columns=%s\n", strings.Join(parsed.DBNames, ","))
	fmt.Println("id: primary_key=true auto_increment=true")
	fmt.Println("product_name: size=64 not_null=true")
	fmt.Println("price: precision=12 scale=4")
	fmt.Println("ignored_field_in_columns=false")
	return nil
}

func parseError() error {
	_, err := schema.Parse(&InvalidModel{}, &sync.Map{}, schema.NamingStrategy{})
	if err == nil || !strings.Contains(err.Error(), "unsupported data type") {
		return fmt.Errorf("unexpected parser result: %v", err)
	}
	fmt.Println("parser=GORM common (no database connection)")
	fmt.Println("unsupported_field=Attributes (map[string]string)")
	return err // This model is deliberately invalid; process status is 1.
}

func mappingOK(mode string) error {
	db, closePool, err := openDatabase(mode)
	if err != nil {
		return err
	}
	defer closePool()
	parsed, err := schema.Parse(&Product{}, &sync.Map{}, db.NamingStrategy)
	if err != nil {
		return err
	}
	if err := verifyTypes(db, parsed, mode); err != nil {
		return err
	}
	return withFixture(db, func(tx *gorm.DB, namespace string) error {
		table := namespace + "." + parsed.Table
		if err := tx.Table(table).Migrator().CreateTable(&Product{}); err != nil {
			return err
		}
		if err := verifyCatalog(tx, namespace, parsed.Table); err != nil {
			return err
		}
		want := Product{Name: "金仓模型", Enabled: true, Price: 12.5}
		if err := tx.Table(table).Create(&want).Error; err != nil {
			return err
		}
		var got Product
		if err := tx.Table(table).First(&got, want.ID).Error; err != nil {
			return err
		}
		if want.ID == 0 || got.ID != want.ID || got.Name != want.Name ||
			got.Enabled != want.Enabled || got.Price != want.Price {
			return fmt.Errorf("round trip mismatch: got=%+v want=%+v", got, want)
		}
		fmt.Println("identity_backfilled=true")
		fmt.Printf("round_trip: name=%s enabled=%t price=%.4f\n", got.Name, got.Enabled, got.Price)
		return nil
	})
}

func mappingError(mode string) error {
	db, closePool, err := openDatabase(mode)
	if err != nil {
		return err
	}
	defer closePool()
	if _, err := schema.Parse(&InvalidMapping{}, &sync.Map{}, db.NamingStrategy); err != nil {
		return err
	}
	fmt.Println("schema_parse=ok")
	fmt.Println("explicit_type=kb_doc_missing_type_622")
	return withFixture(db, func(tx *gorm.DB, namespace string) error {
		err := tx.Table(namespace + ".invalid_mapping").Migrator().CreateTable(&InvalidMapping{})
		var databaseError *gokb.Error
		if !errors.As(err, &databaseError) || string(databaseError.Code) != "42704" {
			return fmt.Errorf("expected undefined-type error, got %v", err)
		}
		fmt.Printf("sqlstate=%s\n", databaseError.Code)
		return err // Keep the database error; the fixture is still rolled back.
	})
}

func verifyParsed(parsed *schema.Schema) error {
	if parsed.Table != "gorm_doc_products" || !reflect.DeepEqual(parsed.DBNames,
		[]string{"id", "product_name", "enabled", "price"}) {
		return fmt.Errorf("unexpected table or columns: %s %v", parsed.Table, parsed.DBNames)
	}
	id, name, price, ignored := parsed.LookUpField("ID"), parsed.LookUpField("Name"),
		parsed.LookUpField("Price"), parsed.LookUpField("Ignored")
	if id == nil || !id.PrimaryKey || !id.AutoIncrement || name == nil ||
		name.DBName != "product_name" || name.Size != 64 || !name.NotNull ||
		price == nil || price.Precision != 12 || price.Scale != 4 ||
		ignored == nil || ignored.DBName != "" {
		return errors.New("model metadata differs from declared tags")
	}
	return nil
}

func verifyTypes(db *gorm.DB, parsed *schema.Schema, mode string) error {
	want := []string{"bigint AUTO_INCREMENT", "varchar(64)", "boolean", "decimal(12,4)"}
	if mode == "oracle" {
		want = []string{"bigint GENERATED BY DEFAULT AS IDENTITY", "VARCHAR2(64)", "boolean", "NUMBER(12,4)"}
	}
	for i, name := range []string{"ID", "Name", "Enabled", "Price"} {
		got := db.Dialector.DataTypeOf(parsed.LookUpField(name))
		if got != want[i] {
			return fmt.Errorf("%s type=%s, want %s", name, got, want[i])
		}
		fmt.Printf("%s_type=%s\n", name, got)
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
	namespace := fmt.Sprintf("gorm_doc_622_%d", time.Now().UnixNano())
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

func verifyCatalog(tx *gorm.DB, namespace, table string) error {
	rows, err := tx.Raw(`
SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod),
       a.attnotnull, a.attidentity
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class t ON t.oid=a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid=t.relnamespace
WHERE n.nspname=? AND t.relname=? AND a.attnum>0 AND NOT a.attisdropped
ORDER BY a.attnum`, namespace, table).Rows()
	if err != nil {
		return err
	}
	defer rows.Close()
	type column struct {
		Name, Type string
		NotNull    bool
		Identity   string
	}
	var columns []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &c.Identity); err != nil {
			return err
		}
		columns = append(columns, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var names []string
	for _, c := range columns {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, []string{"id", "product_name", "enabled", "price"}) {
		return fmt.Errorf("catalog columns=%v", names)
	}
	// Oracle format_type includes the configured CHAR/BYTE length semantics.
	nameTypeOK := regexp.MustCompile(`(?i)^(character varying|varchar2?)\(64( (char|byte))?\)$`).MatchString(columns[1].Type)
	idTypeOK := columns[0].Type == "bigint" || columns[0].Type == "int8"
	boolTypeOK := columns[2].Type == "boolean" || columns[2].Type == "bool"
	if !idTypeOK || !boolTypeOK || columns[0].Identity == "" || !columns[0].NotNull ||
		!nameTypeOK || !columns[1].NotNull ||
		!strings.Contains(strings.ReplaceAll(columns[3].Type, " ", ""), "(12,4)") {
		return fmt.Errorf("catalog attributes differ from model: %+v", columns)
	}
	var primaryCount int64
	if err := tx.Raw(`SELECT count(*) FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class t ON t.oid=i.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid=t.relnamespace
WHERE n.nspname=? AND t.relname=? AND i.indisprimary`, namespace, table).Row().Scan(&primaryCount); err != nil {
		return err
	}
	if primaryCount != 1 {
		return fmt.Errorf("primary key count=%d, want 1", primaryCount)
	}
	fmt.Printf("catalog_columns=%s\n", strings.Join(names, ","))
	fmt.Printf("catalog_name_type=%s not_null=true\n", columns[1].Type)
	fmt.Printf("catalog_price_type=%s\n", columns[3].Type)
	fmt.Println("catalog_primary_key=true identity=true")
	return nil
}
