#!/usr/bin/env bash

require_go() {
    local version
    version="$("$go" env GOVERSION)"
    if ! awk -v version="$version" 'BEGIN {
        sub(/^go/, "", version)
        split(version, parts, ".")
        exit !(parts[1] > 1 || (parts[1] == 1 && parts[2] >= 23))
    }'; then
        printf 'Go 1.23+ is required (found %s). Set GO to a supported Go executable.\n' "$version" >&2
        return 1
    fi
}
