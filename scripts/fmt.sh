#!/bin/sh
# Runs gofumpt over the repo, excluding eval/corpus: corpus fixture bytes
# are pinned by scorer checks (SHA-256 hashes in check-*.sh scripts), so
# reformatting them silently invalidates the eval gates. The corpus files
# are intentionally not gofumpt-clean; that is fine — they are fixtures,
# not production code.
set -e

# Filenames in this repo contain no spaces; newline iteration is safe.
files=$(git ls-files '*.go' | grep -v '^eval/corpus/')
# shellcheck disable=SC2086
exec gofumpt -w $files
