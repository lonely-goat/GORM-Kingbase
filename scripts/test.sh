#!/usr/bin/env bash
set -euo pipefail

repository_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
adapter_root="$repository_root/gorm-driver-kingbase"
mode=${1:-unit}
if [[ $# -gt 0 ]]; then
    shift
fi

usage() {
    echo "usage: bash scripts/test.sh [unit|race|integration|official-mysql|official-oracle] [go test arguments...]" >&2
}

case "$mode" in
    unit|race|integration|official-mysql|official-oracle) ;;
    *) usage; exit 2 ;;
esac

if ! command -v go >/dev/null 2>&1; then
    echo "Go is required; install a Go toolchain before running tests." >&2
    exit 2
fi

export GOWORK=off
export TZ=${TZ:-UTC}
cd "$adapter_root"

case "$mode" in
    unit|race)
        # This entry point never connects to databases, including when the
        # caller has real-database DSNs in their environment.
        unset KINGBASE_MYSQL_DSN KINGBASE_ORACLE_DSN KINGBASE_DSN
        unset KINGBASE_TEST_REQUIRE_DSN KINGBASE_TEST_ALLOW_DDL
        if [[ "$mode" == race ]]; then
            go test -buildvcs=false -race -mod=readonly -count=1 ./... "$@"
        else
            go build -buildvcs=false -mod=readonly ./...
            go test -buildvcs=false -mod=readonly -count=1 ./... "$@"
            go vet -buildvcs=false -mod=readonly ./...
        fi
        echo "No-database checks passed; real-database scenarios were skipped."
        ;;
    integration)
        for variable in KINGBASE_MYSQL_DSN KINGBASE_ORACLE_DSN; do
            if [[ -z ${!variable:-} ]]; then
                echo "Missing $variable; configure both dedicated database DSNs." >&2
                exit 2
            fi
        done
        if [[ ${KINGBASE_TEST_ALLOW_DDL:-} != yes ]]; then
            echo "Set KINGBASE_TEST_ALLOW_DDL=yes for disposable test databases; tests create and drop objects." >&2
            exit 2
        fi
        export KINGBASE_TEST_REQUIRE_DSN=yes
        go test -buildvcs=false -mod=readonly ./integration -count=1 -p=1 -timeout=20m -v "$@"
        ;;
    official-mysql|official-oracle)
        exec bash scripts/run-official-gorm-tests.sh "${mode#official-}" "$@"
        ;;
esac
