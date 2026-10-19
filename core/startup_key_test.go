package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestAdminKeyRejectionSampling(t *testing.T) {
	// 248..255 cannot be reduced modulo 62 without introducing bias.
	input := append(bytes.Repeat([]byte{255}, 4), bytes.Repeat([]byte{61}, 48)...)
	key, err := generateAdminKeyFrom(bytes.NewReader(input))

	if err != nil || key != strings.Repeat("Z", 48) {
		t.Fatalf("key=%q err=%v", key, err)
	}
}

func TestAdminKeyEntropyFailure(t *testing.T) {
	key, err := generateAdminKeyFrom(bytes.NewReader([]byte{1, 2}))
	if key != "" || !errors.Is(err, io.EOF) {
		t.Fatalf("key=%q err=%v", key, err)
	}
}

func TestAdminKeyFormat(t *testing.T) {
	key, err := generateAdminKey()
	if err != nil {
		t.Fatal(err)
	}

	if len(key) != 48 || strings.Trim(key, keyChars) != "" {
		t.Fatal("invalid key format")
	}
}
