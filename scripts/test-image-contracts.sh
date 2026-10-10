#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
sh core/scripts/test-image-contracts.sh
go -C core build ./...
