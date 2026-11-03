#!/usr/bin/env bash
set -euo pipefail

# Include new source files, but never traverse ignored build output or worktrees.
# NUL delimiters preserve whitespace and newlines in filenames.
output=$(
  git ls-files --cached --others --exclude-standard -z -- '*.go' |
    while IFS= read -r -d '' path; do
      # A tracked file may have been deleted in the working tree.
      if [[ -f "$path" ]]; then
        printf './%s\0' "$path"
      fi
    done |
    xargs -0 gofmt -l
)

if [[ -n "$output" ]]; then
  printf 'Unformatted Go files:\n%s\n' "$output" >&2
  exit 1
fi
