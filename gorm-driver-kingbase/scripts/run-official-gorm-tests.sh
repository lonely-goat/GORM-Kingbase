#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
driver_root=$(cd "$script_dir/.." && pwd)

usage() {
    echo "usage: KINGBASE_TEST_ALLOW_DDL=yes $0 <mysql|oracle> [go test arguments...]" >&2
}

if [[ $# -lt 1 ]]; then
    usage
    exit 2
fi

mode=$1
shift
case "$mode" in
    mysql)
        test_dsn=${KINGBASE_MYSQL_DSN:-}
        ;;
    oracle)
        test_dsn=${KINGBASE_ORACLE_DSN:-}
        ;;
    *)
        usage
        exit 2
        ;;
esac

if [[ ${KINGBASE_TEST_ALLOW_DDL:-} != "yes" ]]; then
    echo "refusing to run: set KINGBASE_TEST_ALLOW_DDL=yes after confirming the DSN points to a disposable test database/schema" >&2
    exit 2
fi
if [[ -z "$test_dsn" ]]; then
    echo "refusing to run: KINGBASE_${mode^^}_DSN is not set" >&2
    exit 2
fi
gokb_source=$(cd "$driver_root" && GOWORK=off go list -m -f '{{.Dir}}' kingbase.com/gokb)
# Verify the actual instance before the upstream TestMain can execute DDL.
(
    cd "$driver_root"
    GOWORK=off KINGBASE_DSN="$test_dsn" go run ./cmd/autodetect -expect-mode "$mode"
)

temporary_parent=$(mktemp -d -t gorm-kingbase-official-XXXXXXXX)
# logger/TestSlogLogger asserts that the source path contains
# "gorm/logger/slog_test.go". Keep that upstream path assumption intact.
temporary_root="$temporary_parent/gorm"
cleanup() {
    rm -rf -- "$temporary_parent"
}
trap cleanup EXIT

python3 "$script_dir/prepare-official-gorm.py" "$temporary_root"
patch -s --fuzz=0 -d "$temporary_root" -p1 < "$script_dir/official-gorm-tests.patch"
patch -s --fuzz=0 -d "$temporary_root" -p1 < "$script_dir/official-gorm-assertions.patch"
if [[ ${KINGBASE_DIAGNOSTIC_SQL:-} == "yes" ]]; then
    # Report every ToSQL mismatch in one diagnostic run. This is intentionally
    # opt-in and is not used for pass/fail regression counts.
    patch -s --fuzz=0 -d "$temporary_root" -p1 < "$script_dir/official-gorm-sql-diagnostic.patch"
fi

(
    cd "$temporary_root"
    # Always run the exact tag declared by the adapter, including its core
    # tests. Count executed entries from JSON, never from a hardcoded total.
    GOWORK=off go test -json -count=1 ./... "$@"

    cd "$temporary_root/tests"
    go mod edit -require=kingbase.com/gorm-driver-kingbase@v0.0.0
    go mod edit -replace=kingbase.com/gorm-driver-kingbase="$driver_root"
    # Dependency-module replace directives are not transitive. Until gokb has
    # a public tag, repeat the workspace-only replacement in this temp module.
    go mod edit -replace=kingbase.com/gokb="$gokb_source"
    GOWORK=off GORM_DIALECT=kingbase GORM_DSN="$test_dsn" go test -json -count=1 -mod=mod ./... "$@"
) | python3 "$script_dir/summarize-test-json.py"
