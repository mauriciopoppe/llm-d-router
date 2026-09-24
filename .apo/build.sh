#!/bin/bash
set -euo pipefail

RESULTS_DIR="${1:-.apo/results/scratch}"
mkdir -p "$RESULTS_DIR"

source .apo/set-env.sh 2>/dev/null || true

echo "Executing pre-flight compilation and unit tests for ./pkg/kvcache..."
EXIT_CODE=0
go test -count=1 ./pkg/kvcache > "$RESULTS_DIR/build_output.log" 2>&1 || EXIT_CODE=$?

if [ $EXIT_CODE -ne 0 ]; then
  cat "$RESULTS_DIR/build_output.log" >&2 || true
  cat <<EOF > "$RESULTS_DIR/error.json"
{
  "phase": "PREFLIGHT_BUILD",
  "error_type": "UNIT_TEST_FAILURE",
  "message": "go test ./pkg/kvcache/... failed during build validation. See build_output.log for details."
}
EOF
  echo "Build validation failed with exit code $EXIT_CODE" >&2
  exit 1
fi

echo "Build and unit test validation succeeded."
exit 0
