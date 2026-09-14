#!/usr/bin/env bash
# Project lint rules:
#   1. no fmt.Print* calls outside main.go — use the logger
#   2. no panic( outside main.go — return errors instead
#   3. no TODO/FIXME markers — resolve or remove them
set -u
cd "$(dirname "$0")"

violations=0

while IFS= read -r line; do
	echo "fmt-print: $line"
	violations=$((violations + 1))
done < <(grep -rn "fmt\.Print" --include="*.go" . | grep -v "^\./main\.go" || true)

while IFS= read -r line; do
	echo "panic-call: $line"
	violations=$((violations + 1))
done < <(grep -rn "panic(" --include="*.go" . | grep -v "^\./main\.go" || true)

while IFS= read -r line; do
	echo "todo-marker: $line"
	violations=$((violations + 1))
done < <(grep -rn "TODO\|FIXME" --include="*.go" . || true)

if [ "$violations" -eq 0 ]; then
	echo "lint: clean"
	exit 0
fi
echo "lint: $violations violation(s)"
exit 1
