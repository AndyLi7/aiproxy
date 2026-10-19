package balance

import (
	"encoding/json"
	"testing"
)

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode test JSON: %v", err)
	}

	return raw
}

func checkedType[T any](t *testing.T, value any) T {
	t.Helper()

	result, ok := value.(T)
	if !ok {
		t.Fatalf("unexpected test value type %T", value)
	}

	return result
}
