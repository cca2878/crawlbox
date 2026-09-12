#!/bin/sh
set -eu
export CGO_ENABLED=0
GO=${GO:-go}
for binary in "$@"; do
  $GO version -m "$binary" | grep -q 'CGO_ENABLED=0'
  if command -v readelf >/dev/null; then
    if readelf -d "$binary" 2>/dev/null | grep -q NEEDED; then
      echo "Unexpected native library dependency: $binary" >&2
      exit 1
    fi
  fi
done
