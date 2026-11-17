#!/usr/bin/env bash
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
go="${GO:-go}"
source scripts/test-common.sh
require_go

mkdir -p "$root/.coverage"
rm -f -- "$root/.coverage/coverage.out" "$root/.coverage/coverage.html"
work="$(mktemp -d "$root/.coverage/run.XXXXXXXX")"
readonly work
cleanup() {
    case "$work" in
        "$root"/.coverage/run.*) rm -r -- "$work" ;;
        *) printf 'Refusing to clean unexpected test directory: %s\n' "$work" >&2; return 1 ;;
    esac
}
trap cleanup EXIT
mkdir "$work/unit" "$work/cli" "$work/e2e" "$work/merged"

"$go" vet ./...
"$go" build -cover -covermode=atomic -coverpkg=./... -o "$work/azure-init-kvp" ./cmd/azure-init-kvp
AZURE_INIT_GO_KVP="$work/azure-init-kvp" \
AZURE_INIT_GO_COVERDIR="$work/cli" \
    "$go" test -race -count=1 -covermode=atomic -coverpkg=./... ./... -timeout 120s \
        -args -test.gocoverdir="$work/unit"

GO="$go" AZURE_INIT_TESTINIT_COVERDIR="$work/e2e" bash scripts/test-e2e.sh
"$go" tool covdata merge -i="$work/unit,$work/cli,$work/e2e" -o="$work/merged"
"$go" list -f '{{if .GoFiles}}{{.ImportPath}}{{end}}' ./... | sed '/^$/d' | sort > "$work/expected-packages"
"$go" tool covdata pkglist -i="$work/merged" | sort > "$work/covered-packages"
diff -u "$work/expected-packages" "$work/covered-packages"
"$go" tool covdata textfmt -i="$work/merged" -o="$root/.coverage/coverage.out"
"$go" tool cover -func="$root/.coverage/coverage.out" | tail -n 1
"$go" tool cover -html="$root/.coverage/coverage.out" -o="$root/.coverage/coverage.html"
awk -f scripts/check-coverage.awk "$root/.coverage/coverage.out"
