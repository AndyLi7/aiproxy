package model

import "time"

// Shared leaf for every async generation lane (native /v1/model-tasks and
// image /v1/images/tasks). It holds only the rule, so both lanes import the
// same values; keep it free of lane-specific code.

// AsyncGenerationDeadline is the owner rule of 2026-10-08 for every model and
// provider: an accepted task with no result this long after the provider
// accepted it is failed with AsyncGenerationTimeoutCode, refunded in full
// (wallet settle failed/platform_failure after execution_finished) and
// cancelled upstream on a best-effort basis. A later provider bill is platform
// loss. Never key this on a model. The application repeats the value in its
// docs and client poll windows.
const AsyncGenerationDeadline = 15 * time.Minute

// AsyncGenerationTimeoutCode is the public task error code for that failure.
// It is never a wallet reason: the wallet outcome stays platform_failure.
const AsyncGenerationTimeoutCode = "generation_timeout"

// AsyncGenerationTimeoutMessage is the client-facing sentence for that code.
// Retrying with the same X-Request-Id replays the failed task, so it asks for
// a new one.
const AsyncGenerationTimeoutMessage = "The provider did not finish this task within 15 minutes, so it was stopped. You were not charged; submit a new task with a new X-Request-Id."
