#!/usr/bin/env bash
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
go="${GO:-go}"
python="${PYTHON:-python3}"
source scripts/test-common.sh
require_go
"$python" -c 'import sys; sys.version_info >= (3, 10) or sys.exit("testinit requires Python 3.10+")'

mkdir -p "$root/.coverage/e2e"
work="$(mktemp -d "$root/.coverage/testinit-build.XXXXXXXX")"
readonly work
cleanup() {
    case "$work" in
        "$root"/.coverage/testinit-build.*) rm -r -- "$work" ;;
        *) printf 'Refusing to clean unexpected E2E build directory: %s\n' "$work" >&2; return 1 ;;
    esac
}
trap cleanup EXIT
coverage="${AZURE_INIT_TESTINIT_COVERDIR:-$work/profiles}"
mkdir -p "$coverage"

"$go" vet ./testinit/testdata/consumer
CGO_ENABLED=0 "$go" build -cover -covermode=atomic \
    -coverpkg=github.com/peytonr18/azure-init-go/kvp,github.com/peytonr18/azure-init-go/wireserver \
    -o "$work/consumer" ./testinit/testdata/consumer
CGO_ENABLED=0 "$go" build -cover -covermode=atomic -coverpkg=./... \
    -o "$work/azure-init-kvp" ./cmd/azure-init-kvp

AZURE_INIT_TESTINIT_CONSUMER="$work/consumer" \
AZURE_INIT_TESTINIT_COVERAGE="$coverage" \
    "$python" -B -m unittest discover -s testinit/testing-server -p 'test_*.py' -v

arguments=(
    --consumer "$work/consumer"
    --cli "$work/azure-init-kvp"
    --artifacts "$root/.coverage/e2e"
    --coverage "$coverage"
)
if [[ -n "${AZURE_INIT_RUST_KVP:-}" ]]; then
    arguments+=(--rust-kvp "$AZURE_INIT_RUST_KVP")
fi
"$python" -B testinit/testing-server/module_provisioning.py "${arguments[@]}" "$@"
