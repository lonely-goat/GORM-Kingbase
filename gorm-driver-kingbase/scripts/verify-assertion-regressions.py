#!/usr/bin/env python3
"""Verify real-Kingbase baselines and rejection of assertion-regression faults.

Requires both dedicated test DSNs and KINGBASE_TEST_ALLOW_DDL=yes. Faults are
Go overlays, never edits to the adapter or its tests. Only DB tests execute.
"""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile


DRIVER = Path(__file__).resolve().parent.parent


def replace_once(text, before, after):
    if text.count(before) != 1:
        raise RuntimeError(f"expected one mutation anchor, got {text.count(before)}: {before[:100]}")
    return text.replace(before, after)


def function_change(path, name, transform):
    text = path.read_text()
    start = text.index("func " + name + "(")
    end = text.index("\n}", start) + 2
    return {path: text[:start] + transform(text[start:end]) + text[end:]}


def source_hashes():
    return {str(path.relative_to(DRIVER)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in sorted(DRIVER.rglob("*"))
            if path.is_file() and "__pycache__" not in path.parts}


class Verification:
    def __init__(self, base, output):
        self.base, self.output, self.results = base, output, []

    def run(self, label, mode, official, functions, changes=None, diagnostic=None):
        overlay = {}
        directory = self.output / "overlays"
        directory.mkdir(exist_ok=True)
        for index, (source, content) in enumerate((changes or {}).items()):
            target = directory / f"{label}-{mode}-{index}.go"
            target.write_text(content)
            overlay[str(source)] = str(target)
        command = ["go", "test", "-json", "-count=1", "-mod=mod", "-p=1", "-timeout=5m"]
        if overlay:
            path = directory / f"{label}-{mode}.json"
            path.write_text(json.dumps({"Replace": overlay}, indent=2) + "\n")
            command += ["-overlay", str(path)]
        pattern = "^(" + "|".join(functions) + ")$"
        if not official:
            pattern += "/" + mode
        command += ["-run=" + pattern, "." if official else "./integration"]
        cwd = self.base / "tests" if official else DRIVER
        environment = dict(os.environ, TZ="UTC", GOWORK="off", KINGBASE_TEST_REQUIRE_DSN="yes",
                           GORM_DIALECT="kingbase", GORM_DSN=os.environ["KINGBASE_" + mode.upper() + "_DSN"])
        result = subprocess.run(command, cwd=cwd, env=environment, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        log = self.output / f"{label}-{mode}.jsonl"
        log.write_text(result.stdout)
        events = [json.loads(line) for line in result.stdout.splitlines() if line.startswith("{")]
        decoded = "".join(event.get("Output", "") for event in events)
        terminal = {event["Test"]: event["Action"] for event in events
                    if event.get("Test") and "/" not in event["Test"]
                    and event["Action"] in {"pass", "fail", "skip"}}
        action = "fail" if diagnostic else "pass"
        verified = terminal == {name: action for name in functions}
        verified = verified and ((result.returncode != 0) if diagnostic else result.returncode == 0)
        if diagnostic:
            verified = verified and diagnostic in decoded
        row = {"case": label, "mode": mode, "expected": action, "exit_code": result.returncode,
               "outcomes": terminal, "diagnostic": diagnostic, "verified": verified,
               "log": log.name, "command": command, "cwd": str(cwd),
               "failed_subtests": [event["Test"] for event in events
                                   if event.get("Action") == "fail" and "/" in event.get("Test", "")]}
        self.results.append(row)
        (self.output / "results.json").write_text(json.dumps(self.results, ensure_ascii=False, indent=2) + "\n")
        print(json.dumps({key: row[key] for key in ["case", "mode", "expected", "exit_code", "verified"]}), flush=True)
        if not verified:
            raise RuntimeError(f"unexpected result; inspect {log}; compilation/environment failures are not accepted")

    def cases(self, mode):
        generic = self.base / "tests/generics_test.go"
        join = self.base / "tests/joins_test.go"
        migrator = DRIVER / "migrator.go"
        columns = DRIVER / "migrator_columns.go"
        self.run("baseline-official", mode, True, ["TestGenericsScanUUID", "TestJoinWithSoftDeleted",
                 "TestGenericsPreloads", "TestGenericsNestedPreloads"])
        self.run("baseline-adapter", mode, False, ["TestQuotedIndexName", "TestMetadataSchemaIsolation",
                 "TestIndexMethodPlacement"])

        before = 'gorm.G[User](DB).Select("name").Where("id in ?", []uint{users[0].ID, users[1].ID, users[2].ID}).Order("age").Scan(ctx, &userIds)'
        after = 'gorm.G[User](DB).Raw("SELECT name FROM (SELECT name, age FROM users WHERE id IN ? UNION ALL SELECT ?, 24) AS audit_rows ORDER BY age", []uint{users[0].ID, users[1].ID, users[2].ID}, users[0].Name).Scan(ctx, &userIds)'
        mutation = function_change(generic, "TestGenericsScanUUID", lambda text: replace_once(text, before, after))
        self.run("uuid-extra-row", mode, True, ["TestGenericsScanUUID"], mutation, "Scan failed:")

        before = 'DB.Model(&User{}).Joins("NamedPet").Joins("Account").First(&user3, user.ID)'
        after = 'DB.Model(&User{}).Select("users.*, audit_deleted_account.id AS \\"Account__id\\"").Joins("NamedPet").Joins("LEFT JOIN accounts AS audit_deleted_account ON audit_deleted_account.user_id = users.id").First(&user3, user.ID)'
        mutation = function_change(join, "TestJoinWithSoftDeleted", lambda text: replace_once(text, before, after))
        self.run("soft-delete-account", mode, True, ["TestJoinWithSoftDeleted"], mutation, "final soft-delete join result:")
        after = before.replace('.First(&user3, user.ID)', '.Where("audit_missing_soft_delete_column=1").First(&user3, user.ID)')
        mutation = function_change(join, "TestJoinWithSoftDeleted", lambda text: replace_once(text, before, after))
        self.run("soft-delete-query-error", mode, True, ["TestJoinWithSoftDeleted"], mutation, "final soft-delete join query:")

        preloads = {
            "company": 'results, err := db.Preload("Company", func(db gorm.PreloadBuilder) error {\n\t\tdb.Where("name = ?", u.Company.Name)\n\t\treturn nil\n\t}).Where("name in ?", names).Find(ctx)',
            "pets-limit": 'results, err = db.Preload("Pets", func(db gorm.PreloadBuilder) error {\n\t\tdb.LimitPerRecord(5)\n\t\treturn nil\n\t}).Where("name in ?", names).Find(ctx)',
            "pets-desc": 'results, err = db.Preload("Pets", func(db gorm.PreloadBuilder) error {\n\t\tdb.Order("name desc").LimitPerRecord(5)\n\t\treturn nil\n\t}).Where("name in ?", names).Find(ctx)',
            "friends": 'results, err = db.Preload("Pets", func(db gorm.PreloadBuilder) error {\n\t\tdb.Order("name").LimitPerRecord(5)\n\t\treturn nil\n\t}).Preload("Friends", func(db gorm.PreloadBuilder) error {\n\t\tdb.Order("name")\n\t\treturn nil\n\t}).Where("name in ?", names).Find(ctx)',
        }
        for stage, anchor in preloads.items():
            for kind, condition in [("error", "audit_missing_preload_column=1"), ("empty", "1=0")]:
                after = anchor.replace('Where("name in ?", names)', f'Where("name in ? AND {condition}", names)')
                mutation = function_change(generic, "TestGenericsPreloads", lambda text: replace_once(text, anchor, after))
                self.run("preload-" + stage + "-" + kind, mode, True, ["TestGenericsPreloads"], mutation, "preload results:")
        anchor = preloads["friends"]
        friend_order = 'Preload("Friends", func(db gorm.PreloadBuilder) error {\n\t\tdb.Order("name")'
        after = replace_once(anchor, friend_order, friend_order.replace('db.Order("name")', 'db.Order("name DESC")'))
        mutation = function_change(generic, "TestGenericsPreloads", lambda text: replace_once(text, anchor, after))
        self.run("friends-wrong-order", mode, True, ["TestGenericsPreloads"], mutation, "friends not ordered correctly")

        before = 'Preload("Friends.Pets", func(db gorm.PreloadBuilder) error {\n\t\treturn nil\n\t})'
        after = 'Preload("Friends", func(db gorm.PreloadBuilder) error {\n\t\treturn nil\n\t})'
        mutation = function_change(generic, "TestGenericsNestedPreloads", lambda text: replace_once(text, before, after))
        self.run("nested-missing-pets", mode, True, ["TestGenericsNestedPreloads"], mutation, "nested preload: missing or incomplete friend pets")

        before = 'func (m Migrator) GetIndexes(value interface{}) ([]gorm.Index, error) {'
        mutation = function_change(migrator, "(m Migrator) GetIndexes", lambda text: replace_once(text, before, before + '\n return []gorm.Index{}, nil'))
        self.run("empty-index-metadata", mode, False, ["TestQuotedIndexName"], mutation, "quoted index metadata:")
        for kind, name in [("duplicate", "gorm_kb_accept_quoted_index_name_pkey"), ("unknown", "audit_unexpected_index")]:
            after = f'\t\tif tableName == "gorm_kb_accept_quoted_index_name" {{for _,index:=range indexes{{if index.Name()=="MiXeD"{{index.(*migrator.Index).NameValue="{name}"}}}}}}\n\t\treturn nil\n\t}})\n\treturn indexes, err'
            mutation = function_change(migrator, "(m Migrator) GetIndexes", lambda text: replace_once(text, '\t\treturn nil\n\t})\n\treturn indexes, err', after))
            self.run(kind + "-index-metadata", mode, False, ["TestQuotedIndexName"], mutation, "quoted index metadata: unexpected or duplicate name")

        for kind, fault in [("duplicate", 'for i:=range columnTypes{columnTypes[i]=byName["name"]}'),
                            ("unknown", 'byName["id"].NameValue.String="audit_wrong_id"')]:
            after = '\t\tif tableName == "scope_probe" {' + fault + '}\n\t\treturn nil\n\t})\n\treturn columnTypes, err'
            mutation = function_change(columns, "(m Migrator) ColumnTypes", lambda text: replace_once(text, '\t\treturn nil\n\t})\n\treturn columnTypes, err', after))
            self.run(kind + "-column-metadata", mode, False, ["TestMetadataSchemaIsolation"], mutation, "scoped column metadata:")

        before = '\t\tif idx.Type != "" {\n\t\t\tcreateSQL += " USING " + idx.Type\n\t\t}'
        for label, after in [("ignored-index-type", ""), ("ignored-comment-index-type", before.replace('idx.Type != ""', 'idx.Type != "" && idx.Comment == ""'))]:
            mutation = function_change(migrator, "(m Migrator) CreateIndex", lambda text: replace_once(text, before, after))
            self.run(label, mode, False, ["TestIndexMethodPlacement"], mutation, 'index method="btree" want=hash')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path, help="new evidence directory, outside adapter source")
    args = parser.parse_args()
    if os.environ.get("KINGBASE_TEST_ALLOW_DDL") != "yes" or any(not os.environ.get("KINGBASE_" + mode + "_DSN") for mode in ["MYSQL", "ORACLE"]):
        parser.error("requires both real Kingbase DSNs and KINGBASE_TEST_ALLOW_DDL=yes")
    output = args.output.resolve()
    if output == DRIVER or DRIVER in output.parents:
        parser.error("evidence directory must be outside adapter source")
    output.mkdir(parents=True, exist_ok=False)
    original = source_hashes()
    gokb_source = subprocess.check_output(
        ["go", "list", "-m", "-f", "{{.Dir}}", "kingbase.com/gokb"],
        cwd=DRIVER, env=dict(os.environ, GOWORK="off"), text=True).strip()
    (output / "source-sha256.json").write_text(json.dumps(original, indent=2) + "\n")
    spec = importlib.util.spec_from_file_location("prepare", DRIVER / "scripts/prepare-official-gorm.py")
    prepare = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(prepare)
    with tempfile.TemporaryDirectory(prefix="kingbase-assertion-controls-") as temporary:
        base = Path(temporary) / "gorm"
        prepare.prepare(DRIVER, base)
        for name in ["official-gorm-tests.patch", "official-gorm-assertions.patch"]:
            subprocess.run(["patch", "-s", "-d", str(base), "-p1"], input=(DRIVER / "scripts" / name).read_bytes(), check=True)
        subprocess.run(["go", "mod", "edit", "-require=kingbase.com/gorm-driver-kingbase@v0.0.0",
                        "-replace=kingbase.com/gorm-driver-kingbase=" + str(DRIVER),
                        "-replace=kingbase.com/gokb=" + gokb_source],
                       cwd=base / "tests", env=dict(os.environ, GOWORK="off"), check=True)
        audit = Verification(base, output)
        for mode in ["mysql", "oracle"]:
            with (output / ("connection-" + mode + ".log")).open("w") as log:
                subprocess.run(["go", "run", "./cmd/autodetect", "-expect-mode", mode], cwd=DRIVER,
                               env=dict(os.environ, GOWORK="off", KINGBASE_DSN=os.environ["KINGBASE_" + mode.upper() + "_DSN"]),
                               stdout=log, stderr=subprocess.STDOUT, check=True)
            audit.cases(mode)
        if source_hashes() != original:
            raise RuntimeError("adapter source changed during overlay verification")
        summary = {"run_calls": len(audit.results), "baselines": sum(row["expected"] == "pass" for row in audit.results),
                   "rejected_faults": sum(row["expected"] == "fail" for row in audit.results),
                   "all_verified": all(row["verified"] for row in audit.results), "source_unchanged": True}
        (output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        print(json.dumps(summary), flush=True)


if __name__ == "__main__":
    main()
