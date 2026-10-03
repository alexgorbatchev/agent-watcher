#!/usr/bin/env bash
set -euo pipefail

# Required statement coverage threshold
THRESHOLD="${THRESHOLD:-90.0}"

process_stream() {
  local exit_code=0
  local low_coverage=()

  while IFS= read -r line || [[ -n "$line" ]]; do
    echo "$line"
    if [[ "$line" == *"[no tests to run]"* ]] || [[ "$line" == *"[no test files]"* ]]; then
      continue
    fi
    if [[ "$line" =~ ^FAIL([[:space:]]|$) ]] || [[ "$line" =~ ^---[[:space:]]FAIL: ]]; then
      exit_code=1
    fi
    if [[ "$line" =~ coverage:[[:space:]]*([0-9.]+)%[[:space:]]*of[[:space:]]*statements ]]; then
      local pct="${BASH_REMATCH[1]}"
      local is_low
      is_low=$(awk -v p="$pct" -v t="$THRESHOLD" 'BEGIN { print (p < t) ? 1 : 0 }')
      if [ "$is_low" -eq 1 ]; then
        low_coverage+=("$line")
        exit_code=1
      fi
    fi
  done

  if [ ${#low_coverage[@]} -gt 0 ]; then
    echo "" >&2
    echo "FAIL: statement coverage below ${THRESHOLD}% threshold:" >&2
    for entry in "${low_coverage[@]}"; do
      echo "  $entry" >&2
    done
    return 1
  fi

  return $exit_code
}

if [ $# -gt 0 ]; then
  has_cover=0
  for arg in "$@"; do
    if [[ "$arg" == -cover* ]]; then
      has_cover=1
      break
    fi
  done
  if [ $has_cover -eq 0 ]; then
    set -- -cover "$@"
  fi
  go test "$@" 2>&1 | process_stream
else
  process_stream
fi
