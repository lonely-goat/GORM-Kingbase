#!/usr/bin/env python3
"""Prove corrected assertions reject the audited faults, using isolated overlays."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path, help="directory for baselines, mutation logs and result.json")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    if os.environ.get("KINGBASE_TEST_ALLOW_DDL") != "yes":
        parser.error("set KINGBASE_TEST_ALLOW_DDL=yes for disposable integration databases")
    for mode in ("MYSQL", "ORACLE"):
        if not os.environ.get("KINGBASE_" + mode + "_DSN"):
            parser.error("both KINGBASE_MYSQL_DSN and KINGBASE_ORACLE_DSN are required")
    gokb_source = subprocess.check_output(
        ["go", "list", "-m", "-f", "{{.Dir}}", "kingbase.com/gokb"],
        cwd=root, env=dict(os.environ, GOWORK="off"), text=True).strip()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    original = {p: hashlib.sha256(p.read_bytes()).hexdigest() for p in root.rglob("*.go")}
    environment = dict(os.environ, TZ="UTC", GOWORK="off", KINGBASE_TEST_REQUIRE_DSN="yes")
    results = []

    def run(label, cwd, package, pattern, changes=None, diagnostic=None, extra_env=None):
        overlay = {}
        for index, (source, content) in enumerate((changes or {}).items()):
            replacement = temporary / (label + "-" + str(index) + ".go")
            replacement.write_text(content)
            overlay[str(source)] = str(replacement)
        command = ["go", "test"]
        if overlay:
            mapping = temporary / (label + ".json")
            mapping.write_text(json.dumps({"Replace": overlay}))
            command += ["-overlay", str(mapping)]
        command += ["-mod=mod", package, "-run", pattern, "-count=1", "-p=1", "-parallel=1", "-v"]
        with (output / (label + ".log")).open("w") as log:
            result = subprocess.run(command, cwd=cwd, env=dict(environment, **(extra_env or {})), stdout=log, stderr=subprocess.STDOUT)
        body = (output / (label + ".log")).read_text()
        if changes:
            valid = result.returncode != 0 and "--- FAIL: Test" in body and diagnostic in body
        else:
            valid = result.returncode == 0 and "--- PASS: Test" in body
        results.append({"name": label, "expected": "assertion failure" if changes else "baseline pass", "verified": valid, "exit_code": result.returncode})
        print(label + ": " + ("verified" if valid else "FAILED; inspect log"), flush=True)
        if not valid:
            raise RuntimeError(label + " did not produce its required test outcome")

    def changed(path, before, after):
        source = path.read_text()
        if source.count(before) != 1:
            raise RuntimeError("mutation anchor drifted: " + str(path))
        return {path: source.replace(before, after)}

    with tempfile.TemporaryDirectory(prefix="kingbase-expectations-") as directory:
        temporary = Path(directory)
        for mode in ("mysql", "oracle"):
            subprocess.run(["go", "run", "./cmd/autodetect", "-expect-mode", mode], cwd=root,
                           env=dict(environment, KINGBASE_DSN=os.environ["KINGBASE_" + mode.upper() + "_DSN"]), check=True)

        unit_pattern = "^(TestKBMetadataColumnTypesIndexesAndTables|TestKBMetadataExplicitSchema|TestKBDDLModeSpecificDryRun)$"
        run("baseline-unit", root, ".", unit_pattern)
        integration_pattern = "^(TestP0MigrationMetadataAndIdempotence|TestCreateFromMapReturningIdentity)$"
        run("baseline-integration", root, "./integration", integration_pattern)

        path = root / "migrator.go"
        run("wrong-current-database", root, ".", "^TestKBMetadataColumnTypesIndexesAndTables$",
            changed(path, 'm.queryRaw("SELECT CURRENT_DATABASE()")', 'm.queryRaw("SELECT CURRENT_SCHEMA()")'), "want test")
        filters = {}
        for relative, before, after in (
            ("migrator_mysql.go", "WHERE c.table_schema = ? AND c.table_name = ?", "WHERE c.table_schema = ? OR c.table_name = ?"),
            ("migrator_oracle.go", "WHERE c.table_schema = ? AND c.table_name = ?", "WHERE c.table_schema = ? OR c.table_name = ?"),
            ("migrator_columns.go", "WHERE tc.constraint_schema = ? AND tc.table_name = ?", "WHERE tc.constraint_schema = ? OR tc.table_name = ?"),
            ("migrator.go", "WHERE ns.nspname = ? AND tbl.relname = ?", "WHERE ns.nspname = ? OR tbl.relname = ?"),
        ):
            filters.update(changed(root / relative, before, after))
        run("wrong-metadata-filter", root, ".", "^TestKBMetadataColumnTypesIndexesAndTables$", filters, "recording metadata requires")

        text = path.read_text()
        start = text.index("func (m Migrator) CurrentSchema(")
        end = text.index("\nfunc ", start + 1)
        run("ignored-schema", root, ".", "^TestKBMetadataExplicitSchema$", {path: text[:start] + 'func (m Migrator) CurrentSchema(stmt *gorm.Statement, table string) (interface{}, interface{}) { return clause.Expr{SQL: "CURRENT_SCHEMA()"}, table }\n' + text[end:]}, "tenant columns")
        start = text.index("func (m Migrator) AddColumn(")
        end = text.index("\nfunc ", start + 1)
        run("missing-add-column", root, ".", "^TestKBDDLModeSpecificDryRun$", {path: text[:start] + 'func (m Migrator) AddColumn(value interface{}, fieldName string) error { return nil }\n' + text[end:]}, "does not contain")
        run("missing-column-resize", root, "./integration", "^TestP0MigrationMetadataAndIdempotence$",
            changed(path, "\t\t// KingbaseES accepts the type/nullability change through MODIFY, but", '\t\tif stmt.Table == "gorm_kb_accept_migrations" && field.DBName == "name" { return nil }\n\t\t// KingbaseES accepts the type/nullability change through MODIFY, but'), "name length=")
        run("reordered-input-maps", root, "./integration", "^TestCreateFromMapReturningIdentity$",
            changed(root / "map_returning.go", "\t*batch = (*batch)[:count]", "\t*batch = (*batch)[:count]\n\tif count >= 2 { (*batch)[0], (*batch)[1] = (*batch)[1], (*batch)[0] }"), "no longer refers to its original input map")

        boundary_pattern = "^(TestSchemaIndexOperations|TestPartialIndexPredicateAndUniqueness|TestIndexMethodPlacement|TestExpressionIndexMetadata|TestExplicitZeroTimePrecision)$"
        run("baseline-index-boundaries", root, "./integration", boundary_pattern)
        for label, before, after, pattern, diagnostic in (
            ("drop-wrong-schema", 'return m.DB.Exec("DROP INDEX ?", m.indexIdentifier(stmt, name)).Error',
             'return m.DB.Exec("DROP INDEX ?", clause.Column{Name: name}).Error', "^TestSchemaIndexOperations$/mysql/drop", "catalog index"),
            ("rename-wrong-schema", "m.indexIdentifier(stmt, oldName), clause.Column{Name: newName}",
             "clause.Column{Name: oldName}, clause.Column{Name: newName}", "^TestSchemaIndexOperations$/oracle/rename", "catalog index"),
            ("comment-wrong-schema", 'tx.Exec("COMMENT ON INDEX ? IS ?", m.indexIdentifier(stmt, idx.Name), commentLiteral)',
             'tx.Exec("COMMENT ON INDEX ? IS ?", clause.Column{Name: idx.Name}, commentLiteral)', "^TestSchemaIndexOperations$/oracle/comment", "index comment"),
            ("missing-index-predicate", '\t\tif idx.Where != "" {\n\t\t\tcreateSQL += " WHERE " + idx.Where\n\t\t}',
             "", "^TestPartialIndexPredicateAndUniqueness$", "partial index predicate="),
            ("missing-expression-key", "LEFT JOIN pg_catalog.pg_attribute AS attr ON attr.attrelid = tbl.oid AND attr.attnum = keys.attnum",
             "JOIN pg_catalog.pg_attribute AS attr ON attr.attrelid = tbl.oid AND attr.attnum = keys.attnum", "^TestExpressionIndexMetadata$", "expression indexes count="),
            ("wrong-index-method-placement", '\t\tif idx.Type != "" {\n\t\t\tcreateSQL += " USING " + idx.Type\n\t\t}\n\t\tcreateSQL += "?"',
             '\t\tcreateSQL += "?"\n\t\tif idx.Type != "" {\n\t\t\tcreateSQL += " USING " + idx.Type\n\t\t}', "^TestIndexMethodPlacement$", "USING"),
        ):
            run(label, root, "./integration", pattern, changed(path, before, after), diagnostic)
        run("lost-zero-time-precision", root, "./integration", "^TestExplicitZeroTimePrecision$/oracle",
            changed(root / "oracle_types.go", 'field.Precision > 0 || field.TagSettings["PRECISION"] == "0"', "field.Precision > 0"), "zero_at precision=6 want=0")

        run("baseline-submission-boundaries", root, "./integration",
            "^(TestUppercaseModelReadAndReturning|TestUppercaseAssociationReturningAndJoins|TestUppercaseModelMigrationSchema|TestGroupedAndDistinctPagination|TestMapSliceCreateReturningVariants)$")
        run("lost-uppercase-result-mapping", root, "./integration", "^TestUppercaseModelReadAndReturning$/oracle",
            changed(root / "oracle_names.go", "func (d *Dialector) mapOracleColumns(db *gorm.DB) {",
                    "func (d *Dialector) mapOracleColumns(db *gorm.DB) {\n return"), "uppercase identity RETURNING")
        run("lost-uppercase-catalog-folding", root, "./integration", "^TestUppercaseModelMigrationSchema$/oracle",
            changed(root / "oracle_names.go", "func (d *Dialector) catalogName(name string) string {",
                    "func (d *Dialector) catalogName(name string) string {\n return name"), "uppercase catalog presence")
        run("lost-uppercase-join-mapping", root, "./integration", "^TestUppercaseAssociationReturningAndJoins$/oracle",
            changed(root / "oracle_names.go", "for _, join := range stmt.Joins {", "for _, join := range stmt.Joins[:0] {"), "uppercase JOIN=")
        run("invalid-projection-pagination", root, "./integration", "^TestGroupedAndDistinctPagination$/oracle",
            changed(root / "clause.go", 'if !stmt.Distinct && !grouped && len(stmt.Selects) == 0 && stmt.Schema != nil && stmt.Schema.PrioritizedPrimaryField != nil {',
                    'if (grouped || !grouped) && stmt.Schema != nil && stmt.Schema.PrioritizedPrimaryField != nil {'), "grouped page=")
        run("lost-map-value-adaptation", root, "./integration", "^TestMapSliceCreateReturningVariants$",
            changed(root / "map_returning.go", "func createWithMapReturning(config *callbacks.Config) func(*gorm.DB) {",
                    "func createWithMapReturning(config *callbacks.Config) func(*gorm.DB) {\n return callbacks.Create(config)"), "map batch len=")

        official = temporary / "gorm"
        subprocess.run(["python3", str(root / "scripts/prepare-official-gorm.py"), str(official)], check=True)
        for patch in ("official-gorm-tests.patch", "official-gorm-assertions.patch"):
            subprocess.run(["patch", "-s", "-p1", "-i", str(root / "scripts" / patch)], cwd=official, check=True)
        subprocess.run(["go", "mod", "edit", "-require=kingbase.com/gorm-driver-kingbase@v0.0.0", "-replace=kingbase.com/gorm-driver-kingbase=" + str(root), "-replace=kingbase.com/gokb=" + gokb_source], cwd=official / "tests", check=True)
        run("baseline-assert-equal", official, "./utils/tests", "^TestAssertEqualReverseConversion$")
        run("wrong-assert-equal", official, "./utils/tests", "^TestAssertEqualReverseConversion$",
            changed(official / "utils/tests/utils.go", "expect = reflect.ValueOf(expect).Convert(reflect.ValueOf(got).Type()).Interface()", "expect = reflect.ValueOf(got).Convert(reflect.ValueOf(got).Type()).Interface()"), "unequal reverse-convertible values were not rejected")
        for mode in ("mysql", "oracle"):
            env = {"GORM_DIALECT": "kingbase", "GORM_DSN": os.environ["KINGBASE_" + mode.upper() + "_DSN"]}
            run("baseline-official-" + mode, official / "tests", ".", "^(TestSmartMigrateColumn(GaussDB)?|TestTransactionWithBlock|TestJoinWithOmit)$", extra_env=env)
            run("missing-join-table-" + mode, official / "tests", ".", "^TestJoinWithOmit$",
                changed(official / "tests/joins_test.go",
                        'DB.Table("users").Omit("name").Where("users.name = ?", "joins_with_omit").Joins("left join pets on pets.user_id = users.id")',
                        'DB.Table("users").Omit("name").Where("users.name = ?", "joins_with_omit").Joins("left join gorm_kb_intentionally_missing_join on true")'), "join with omit:", env)
            run("zero-metadata-" + mode, official / "tests", ".", "^TestSmartMigrateColumn(GaussDB)?$",
                changed(root / "migrator_columns.go", "\t\t\tcompleteColumnTypeMetadata(column, dataType)", '\t\t\tcompleteColumnTypeMetadata(column, dataType)\n\t\t\tif tableName == "user_migrate_columns" { column.LengthValue = sql.NullInt64{Valid:true}; column.DecimalSizeValue = sql.NullInt64{Valid:true}; column.ScaleValue = sql.NullInt64{Valid:true} }'), "unknown/zero", env)
            transaction = "func (db *DB) Transaction(fc func(tx *DB) error, opts ...*sql.TxOptions) (err error) {"
            run("swallowed-transaction-error-" + mode, official / "tests", ".", "^TestTransactionWithBlock$",
                changed(official / "finisher_api.go", transaction, transaction + '\n defer func(){ if err != nil && err.Error() == "the error message" { err=nil } }()'), "Transaction return error will equal the block returns error", env)

    if any(hashlib.sha256(path.read_bytes()).hexdigest() != digest for path, digest in original.items()):
        raise RuntimeError("original source files changed during mutation verification")
    (output / "result.json").write_text(json.dumps({"results": results, "original_go_files_unchanged": True}, indent=2) + "\n")
    print("All baselines passed; every injected fault was rejected by its intended assertion.")


if __name__ == "__main__":
    main()
