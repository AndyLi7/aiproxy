package main

import (
	"encoding/json"
	"testing"
)

func testValue[T any](t *testing.T, v any) T {
	t.Helper()

	value, ok := v.(T)
	if !ok {
		t.Fatalf("unexpected fixture type %T", v)
	}

	return value
}

func marshalFixture(t *testing.T, v any) []byte {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}
