package model

import (
	"strings"
	"testing"
)

func TestBuildOperationalFieldsSanitizesServerFields(t *testing.T) {
	t.Parallel()

	fields := BuildOperationalFields(
		RequestSourcePlayground,
		FailureStageBalance,
		"Bearer sk-secret-value\ninsufficient balance",
	)

	if fields.RequestSource != RequestSourcePlayground {
		t.Fatalf("request source = %q, want %q", fields.RequestSource, RequestSourcePlayground)
	}

	if fields.FailureStage != FailureStageBalance {
		t.Fatalf("failure stage = %q, want %q", fields.FailureStage, FailureStageBalance)
	}

	if fields.ErrorCode != "insufficient_balance" {
		t.Fatalf("error code = %q, want insufficient_balance", fields.ErrorCode)
	}

	if strings.Contains(strings.ToLower(fields.SafeError), "bearer") ||
		strings.Contains(fields.SafeError, "sk-secret-value") {
		t.Fatalf("safe error leaked authorization: %q", fields.SafeError)
	}

	if strings.ContainsAny(fields.SafeError, "\r\n") {
		t.Fatalf("safe error contains a line break: %q", fields.SafeError)
	}
}

func TestBuildOperationalFieldsPreservesSafeSpecificErrorCode(t *testing.T) {
	t.Parallel()

	fields := BuildOperationalFields(
		RequestSourceAPI,
		FailureStageValidation,
		"unsupported size",
		"unsupported_size",
	)
	if fields.ErrorCode != "unsupported_size" {
		t.Fatalf("error code = %q, want unsupported_size", fields.ErrorCode)
	}

	invalid := BuildOperationalFields(
		RequestSourceAPI,
		FailureStageValidation,
		"invalid",
		"../../unsafe",
	)
	if invalid.ErrorCode != "invalid_request" {
		t.Fatalf("invalid error code = %q, want invalid_request", invalid.ErrorCode)
	}
}

func TestBuildOperationalFieldsDefaultsUntrustedValues(t *testing.T) {
	t.Parallel()

	fields := BuildOperationalFields("spoofed", FailureStage("spoofed"), strings.Repeat("x", 600))

	if fields.RequestSource != RequestSourceAPI {
		t.Fatalf("request source = %q, want %q", fields.RequestSource, RequestSourceAPI)
	}

	if fields.FailureStage != FailureStageNone {
		t.Fatalf("failure stage = %q, want empty", fields.FailureStage)
	}

	if got := len([]rune(fields.SafeError)); got != 512 {
		t.Fatalf("safe error rune length = %d, want 512", got)
	}
}

func TestBuildOperationalFieldsPreservesAdminDemoSource(t *testing.T) {
	t.Parallel()

	fields := BuildOperationalFields(RequestSourceAdminDemo, FailureStageNone, "")
	if fields.RequestSource != RequestSourceAdminDemo {
		t.Fatalf("request source = %q, want %q", fields.RequestSource, RequestSourceAdminDemo)
	}
}

func TestBuildOperationalFieldsNormalizesUnknownSourceToAPI(t *testing.T) {
	t.Parallel()

	fields := BuildOperationalFields("internal-secret", FailureStageNone, "")
	if fields.RequestSource != RequestSourceAPI {
		t.Fatalf("request source = %q, want %q", fields.RequestSource, RequestSourceAPI)
	}
}
