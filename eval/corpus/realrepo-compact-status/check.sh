#!/bin/bash
# Pass when all four options exist on Options, are reachable through the
# `option` builtin, appear in the regenerated schema.json, and the two
# wiring turns left a consumer outside the declaration sites. On the
# start state none of the keys exist anywhere — the first grep fails.
set -e
cd "$EVAL_WORKDIR"

go build ./...

# The Options fields with their JSON tags.
for k in compact_status quiet_startup show_hints auto_update; do
	grep -rq "$k" internal/config/
	grep -q "\"$k\"" schema.json
done

# The `option` builtin keys.
for k in compact-status quiet-startup show-hints auto-update; do
	grep -q "$k" internal/shellconfig/options.go
done

# Consumer wiring — a reader outside config declaration/builtin plumbing.
grep -rl 'ShowHints\|show_hints' internal/ | grep -vq 'internal/config/\|internal/shellconfig/'
grep -rl 'AutoUpdate\|auto_update' internal/ | grep -vq 'internal/config/\|internal/shellconfig/'
