package imageprepayment

import (
	"testing"
)

func checkedType[T any](t *testing.T, value any) T {
	t.Helper()

	result, ok := value.(T)
	if !ok {
		t.Fatalf("unexpected test value type %T", value)
	}

	return result
}
