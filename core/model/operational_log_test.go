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

func TestLogOperationalStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		log  Log
		want OperationalStatus
	}{
		{
			name: "rejected before upstream",
			log:  Log{Code: 402, FailureStage: FailureStageBalance},
			want: OperationalStatusRejected,
		},
		{
			name: "upstream failure",
			log:  Log{Code: 502, FailureStage: FailureStageUpstream},
			want: OperationalStatusFailed,
		},
		{
			name: "legacy failure without stage",
			log:  Log{Code: 500},
			want: OperationalStatusFailed,
		},
		{
			name: "async request still processing",
			log:  Log{Code: 200, AsyncUsageStatus: AsyncUsageStatusPending},
			want: OperationalStatusProcessing,
		},
		{
			name: "successful request",
			log:  Log{Code: 200, AsyncUsageStatus: AsyncUsageStatusCompleted},
			want: OperationalStatusSuccess,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.log.OperationalStatus(); got != tt.want {
				t.Fatalf("status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLogOperationalStatusTreatsAsyncFailureAsFailed(t *testing.T) {
	entry := &Log{Code: 200, AsyncUsageStatus: AsyncUsageStatusFailed}
	if got := entry.OperationalStatus(); got != OperationalStatusFailed {
		t.Fatalf("OperationalStatus() = %q, want %q", got, OperationalStatusFailed)
	}
}
