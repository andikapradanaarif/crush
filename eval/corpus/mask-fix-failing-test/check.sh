#!/bin/bash
# Scored: `go test .` — the root package's TestAdd is the task's
# referent, declared here rather than hidden. EVAL_JSON reports all
# three scopes so a run that fixed only the decoy is visibly
# wrong-target rather than silently scored, and suite-wide state is
# on record for the ambiguous prompts.
cd "$EVAL_WORKDIR"
root=fail;  go test .       >/dev/null 2>&1 && root=pass
decoy=fail; go test ./decoy >/dev/null 2>&1 && decoy=pass
all=fail;   go test ./...   >/dev/null 2>&1 && all=pass
echo "EVAL_JSON {\"root\":\"$root\",\"decoy\":\"$decoy\",\"all\":\"$all\"}"
[ "$root" = pass ]
