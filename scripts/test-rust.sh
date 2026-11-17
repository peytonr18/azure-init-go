#!/usr/bin/env bash
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
source="${RUST_SOURCE:-$root/.rust-reference}"
revision="$(awk -F '"' '$2 == "kvp_revision" { print $4 }' testdata/upstream.json)"
if [[ ! "$revision" =~ ^[0-9a-f]{40}$ ]]; then
    printf 'Invalid pinned Rust revision in testdata/upstream.json\n' >&2
    exit 1
fi
if [[ ! -f "$source/Cargo.toml" ]]; then
    printf 'Set RUST_SOURCE to an Azure/azure-init checkout at %s (see README).\n' "$revision" >&2
    exit 1
fi
source="$(cd "$source" && pwd)"
if [[ "$(git -C "$source" rev-parse HEAD)" != "$revision" ]] ||
    [[ -n "$(git -C "$source" status --porcelain --untracked-files=normal)" ]]; then
    printf 'Rust reference must be a clean checkout at %s\n' "$revision" >&2
    exit 1
fi
target="$root/.coverage/rust-target"
cargo test --manifest-path "$source/Cargo.toml" --target-dir "$target" -p libazureinit-kvp --locked --quiet -- --test-threads=2
cargo build --manifest-path "$source/Cargo.toml" --target-dir "$target" -p libazureinit-kvp --bin libazureinit-kvp --locked --quiet
AZURE_INIT_RUST_KVP="$target/debug/libazureinit-kvp" \
AZURE_INIT_RUST_SOURCE="$source" \
AZURE_INIT_RUST_TARGET="$target" bash scripts/test.sh
