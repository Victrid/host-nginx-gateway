#!/usr/bin/env bash
# Summarize a conformance run log: top-level test outcomes + report stats.
# Usage: bash e2e/conformance/summarize.sh /tmp/opencode/conformance-test.log
set -u
LOG=${1:-/tmp/opencode/conformance-test.log}
echo "== top-level outcomes =="
grep -E '^(=== RUN   TestConformance/|--- (PASS|FAIL|SKIP): TestConformance/)' "$LOG" \
  | grep -E '^--- ' | sed -E 's/^--- (PASS|FAIL|SKIP): TestConformance\/([^ ]+).*/\1 \2/' | sort -k2
echo
echo "== counts =="
for s in PASS FAIL SKIP; do
  printf '%s=%d\n' "$s" "$(grep -cE "^--- $s: TestConformance/" "$LOG" || true)"
done
echo
echo "== report profile stats =="
if [ -f "$(dirname "$0")/report.yaml" ]; then
  grep -E "^(  name|  summary|    result|      passed|      failed|      skipped)" "$(dirname "$0")/report.yaml" | head -40
fi
