#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
test -f model/image_task_test.go
test -f task/image_archive_test.go
test -f common/registryvalidation/discovery_test.go
go test -count=1 ./common/registryvalidation ./common/ownedimage ./task ./controller ./middleware ./relay/adaptor/fal ./relay/adaptor/doubao
go test -count=1 ./router -run 'Test(SlashfulModelDiscovery|ModelDiscoveryKeeps|SetRelayRouter)'

# Container-backed database/cache integration tests run separately on Docker hosts.
go test -count=1 ./model -run 'Test(Image|PendingImage|InterruptedImage|CompleteSyncImage|ResolveImage|Measured|QueueImage|QueuePixel|DiscoveryCacheKeeps)'
