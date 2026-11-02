#!/usr/bin/env bash
# 仓库统一测试入口：在完整 Git 工作目录根目录执行。
# 不带参数或使用 unit：只做构建、无数据库测试和 vet，不连接真实数据库。
# integration：要求两种模式的 DSN 和 DDL 开关，真正通过 gokb 连接金仓测试库。
# race、official-mysql、official-oracle 的前提及结果边界见适配器 docs/维护导读.md。
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
