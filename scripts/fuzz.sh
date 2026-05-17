#!/usr/bin/env bash
#
# Run coverage-guided fuzzing on every target in fuzz_test.go.
#
# CI only replays the committed seed corpus (sub-second, in the `test` job).
# Real fuzzing — which mutates inputs for minutes per target — is a local,
# on-demand task: run this before a release, or after touching any parser.
#
# A crash stops the run; Go writes the minimized reproducer to
# testdata/fuzz/<target>/. Commit that file as a permanent regression case.
#
# Usage:
#   scripts/fuzz.sh                 # all targets, 1m each
#   scripts/fuzz.sh 30s             # all targets, 30s each
#   scripts/fuzz.sh 5m FuzzIsSafeCommand FuzzDiffToEdit   # named targets
#
set -euo pipefail

cd "$(dirname "$0")/.."

FUZZTIME="${1:-1m}"
if [[ $# -gt 0 ]]; then shift; fi

if [[ $# -gt 0 ]]; then
  targets=("$@")
else
  mapfile -t targets < <(go test -list '^Fuzz' . | grep '^Fuzz')
fi

echo "fuzzing ${#targets[@]} target(s), ${FUZZTIME} each"
failed=()
for t in "${targets[@]}"; do
  echo "=== $t ($FUZZTIME) ==="
  if ! go test -run '^$' -fuzz "^${t}$" -fuzztime "$FUZZTIME" .; then
    failed+=("$t")
    echo "!!! $t found a crash — reproducer in testdata/fuzz/$t/"
  fi
done

if [[ ${#failed[@]} -gt 0 ]]; then
  echo "FAIL: ${failed[*]}"
  exit 1
fi
echo "all ${#targets[@]} target(s) clean"
