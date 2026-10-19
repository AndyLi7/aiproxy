package registryvalidation

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

func checkError(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Errorf("unexpected test error: %v", err)
	}
}
