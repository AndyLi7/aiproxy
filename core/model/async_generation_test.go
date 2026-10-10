package model_test

import (
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
)

// Pins the owner rule of 2026-10-08. The application repeats this value in its
// docs and client poll windows, so a change here needs a matching app change.
func TestAsyncGenerationRuleIsFifteenMinutes(t *testing.T) {
	if model.AsyncGenerationDeadline != 15*time.Minute {
		t.Fatalf("deadline = %s", model.AsyncGenerationDeadline)
	}
	if model.AsyncGenerationTimeoutCode != "generation_timeout" {
		t.Fatalf("code = %q", model.AsyncGenerationTimeoutCode)
	}
	for _, want := range []string{"15 minutes", "not charged", "new X-Request-Id"} {
		if !strings.Contains(model.AsyncGenerationTimeoutMessage, want) {
			t.Fatalf("message %q lacks %q", model.AsyncGenerationTimeoutMessage, want)
		}
	}
}
