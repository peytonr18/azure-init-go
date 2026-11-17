NR == 1 {
    if ($1 != "mode:" || NF != 2 || $2 !~ /^(set|count|atomic)$/) {
        print "Invalid coverage profile header" > "/dev/stderr"
        invalid = 1
        exit 2
    }
    next
}
{
    if (NF != 3 || $2 !~ /^[0-9]+$/ || $3 !~ /^[0-9]+$/ ||
        ($1 in statements && statements[$1] != $2)) {
        print "Invalid or inconsistent coverage block: " $0 > "/dev/stderr"
        invalid = 1
        exit 2
    }
    statements[$1] = $2
    hits[$1] += $3
}
END {
    if (invalid) {
        exit 2
    }
    for (block in statements) {
        total += statements[block]
        if (hits[block] > 0) {
            covered += statements[block]
        } else if (statements[block] > 0) {
            print "Uncovered: " block > "/dev/stderr"
            uncovered = 1
        }
    }
    if (total == 0) {
        print "Coverage profile contains no executable statements" > "/dev/stderr"
        exit 2
    }
    printf "Exact coverage: %d/%d executable statements\n", covered, total
    exit uncovered
}
