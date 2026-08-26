#!/bin/sh

set -eu

SWAG_VERSION="${SWAG_VERSION:-v1.16.6}"

if ! command -v swag >/dev/null 2>&1; then
  go install "github.com/swaggo/swag/cmd/swag@${SWAG_VERSION}"
fi
swag init --parseDependency --parseInternal
swag fmt
