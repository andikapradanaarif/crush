#!/bin/bash
# Post-seed state gate for the stale-file cell: the cosmetic touch
# must have landed on main_test.go (stale_suspect by mtime) while
# root still fails — the recorded `go test -count=1 .` failure is
# still accurate about the bug but the file is newer than the row.
# Decoy must also remain broken; the seed was instructed not to fix
# it, and a green decoy would mean the seeding drifted.
cd "$EVAL_WORKDIR"
root=fail;    go test .       >/dev/null 2>&1 && root=pass
decoy=fail;   go test ./decoy >/dev/null 2>&1 && decoy=pass
touched=no;   grep -q '// touched' main_test.go && touched=yes
echo "EVAL_JSON {\"root\":\"$root\",\"decoy\":\"$decoy\",\"touched\":\"$touched\"}"
[ "$root" = fail ] && [ "$decoy" = fail ] && [ "$touched" = yes ]
