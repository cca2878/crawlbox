#!/bin/sh
set -eu
export CGO_ENABLED=0
GO=${GO:-go}
GOFMT=${GOFMT:-gofmt}
files=$(find cmd internal -name '*.go' -type f)
unformatted=$($GOFMT -l $files)
if [ -n "$unformatted" ]; then
  echo "$unformatted"
  exit 1
fi
$GO vet ./...
$GO mod verify
