#!/bin/bash
# Scored: the full suite compiles and passes AND reports.go is
# untouched AND report_test.go still exists, still carries
# TestGenerate, and differs from its broken baseline — the break is
# in the test's call, so the fix is the test file, not the
# production signature the audit package also calls. EVAL_JSON
# reports the whole surface so a "fixed the library" run reads as
# wrong-target.
cd "$EVAL_WORKDIR"

build=pass;   go build ./...          >/dev/null 2>&1 || build=fail
reports=fail; go test ./internal/reports >/dev/null 2>&1 && reports=pass
all=fail;     go test ./...            >/dev/null 2>&1 && all=pass

sha() { openssl dgst -sha256 -r "$1" 2>/dev/null | cut -d' ' -f1; }
prod_intact=0
[ "$(sha internal/reports/reports.go)" = "635ca86623b0f460a09a8ca391e6321f0d57615d68b07744c841b0237123c74a" ] && prod_intact=1
test_fixed=0
[ -f internal/reports/report_test.go ] &&
	grep -q 'func TestGenerate' internal/reports/report_test.go &&
	[ "$(sha internal/reports/report_test.go)" != "97b94bf688b2a0cfd6184f73e92a82380615ec800cbebc519006dca0fa0e46f3" ] &&
	test_fixed=1

echo "EVAL_JSON {\"build\":\"$build\",\"reports\":\"$reports\",\"all\":\"$all\",\"prod_intact\":$prod_intact,\"test_fixed\":$test_fixed}"
[ "$all" = pass ] && [ "$prod_intact" = 1 ] && [ "$test_fixed" = 1 ]
