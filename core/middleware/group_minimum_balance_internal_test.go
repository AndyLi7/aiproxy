package middleware

import "testing"

func TestGetGroupMinimumBalanceUsesConfiguredUSDThreshold(t *testing.T) {
	t.Setenv("GROUP_MINIMUM_BALANCE", "0.025")

	if got := GetGroupMinimumBalance(); got != 0.025 {
		t.Fatalf("got %v, want 0.025", got)
	}
}

func TestGetGroupMinimumBalanceClampsNegativeThreshold(t *testing.T) {
	t.Setenv("GROUP_MINIMUM_BALANCE", "-1")

	if got := GetGroupMinimumBalance(); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}
