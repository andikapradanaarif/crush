#!/bin/bash
# Scored: the stamp suite and the full suite are green AND the stamp
# test, doc.go, and ledger.go are byte-identical while stamp.go moved
# off the defect — the bug is the zero-indexed StampAll loop, so a
# run that patched the test or another package fixed the wrong file.
cd "$EVAL_WORKDIR"
export GOFLAGS=" "

stamp=fail;  go test -count=1 ./internal/stamp >/dev/null 2>&1 && stamp=pass
all=fail;    go test -count=1 ./...            >/dev/null 2>&1 && all=pass

sha() { openssl dgst -sha256 -r "$1" 2>/dev/null | cut -d' ' -f1; }
test_intact=0
[ "$(sha internal/stamp/stamp_test.go)" = "f168e803d4ca0101e05c10f60585ef9f990ff6245a8ca1ffc9fee850622feda7" ] && test_intact=1
caller_intact=0
[ "$(sha internal/doc/doc.go)" = "1a3b17f842aa115680ba55cd37dc8ec977d7f325587d373dfb280fa68c362b52" ] && caller_intact=1
data_intact=0
[ "$(sha internal/ledger/ledger.go)" = "5dc21295da5cb61f706e9dc61f14a6dd81421153a18c602db2faf04a061f1daf" ] && data_intact=1
src_fixed=0
[ "$(sha internal/stamp/stamp.go)" != "bd283a02ad23303b3e4f9f739322239027eb45a9091b06be1f3b9a3979946f02" ] && src_fixed=1

echo "EVAL_JSON {\"stamp\":\"$stamp\",\"all\":\"$all\",\"test_intact\":$test_intact,\"caller_intact\":$caller_intact,\"data_intact\":$data_intact,\"src_fixed\":$src_fixed}"
[ "$stamp" = pass ] && [ "$all" = pass ] && [ "$test_intact" = 1 ] && [ "$caller_intact" = 1 ] && [ "$data_intact" = 1 ] && [ "$src_fixed" = 1 ]
